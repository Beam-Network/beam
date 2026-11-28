package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/opus"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

const (
	minAudioSlots    = 8
	maxAudioSlots    = 64
	videoHold        = 2 * time.Second
	firstVideoWait   = 5 * time.Second
	opusFrameMS      = 20
	audioQueueFrames = 15
)

var opusSilence = []byte{0xf8, 0xff, 0xfe}

type composer struct {
	role      *Role
	slotCount int
	parent    mc.TopologyNode
	pc        *webrtc.PeerConnection
	events    *webrtc.DataChannel
	start     time.Time
	failed    chan error
	fail      sync.Once
	meter     *usage.Connection

	mu           sync.Mutex
	tracks       map[string]mc.Track
	joined       map[string]int64
	dominant     string
	videoMID     string
	audioMIDs    []string
	videoTrack   string
	videoSince   time.Time
	videoCodec   string
	videoSSRC    webrtc.SSRC
	needKey      bool
	audioBinding map[string]string
	slots        map[string]*audioSlot
	commandID    int
	frames       chan videoFrame
}

type videoFrame struct {
	at    int64
	key   bool
	codec string
	data  []byte
}

type audioSlot struct {
	number uint64
	mu     sync.Mutex
	queue  [][]byte
	next   int64
}

func (c *composer) failWith(err error) {
	c.fail.Do(func() { c.failed <- err })
}

func (r *Role) runFromSFU(ctx context.Context, parent mc.TopologyNode) error {
	if r.node.Topology().Protection.Mode == "e2ee" {
		return errors.New("an e2ee session cannot be transcoded")
	}
	if r.source {
		return errors.New("a source ladder copies one ingest source; an sfu composition is encoded")
	}
	runCtx, cancel := r.parentWatch(ctx, parent)
	defer cancel()
	whep := broadcast.Endpoint(parent, "whep")
	if whep == "" {
		return errors.New("sfu parent announces no WHEP endpoint")
	}
	if r.composeSlots == 0 {
		r.composeSlots = minAudioSlots
	}
	c := &composer{role: r, slotCount: r.composeSlots, parent: parent, start: time.Now(), failed: make(chan error, 1), tracks: map[string]mc.Track{},
		joined: map[string]int64{}, audioBinding: map[string]string{}, slots: map[string]*audioSlot{},
		frames: make(chan videoFrame, 256)}
	identity := broadcast.StringPtr(mc.NodeIdentityPrefix + parent.NodeID)
	c.meter = r.env.Usage.OpenWith(usage.ConnectionInfo{Identity: identity, Direction: broadcast.DirectionIngress,
		Transport: broadcast.TransportUpstream, Detached: true, Versions: usage.OnlyV2}).Meter()
	defer c.meter.Close()
	if err := c.connect(runCtx, whep); err != nil {
		return err
	}
	defer c.pc.Close()
	upstream := r.env.Usage.OpenWith(usage.ConnectionInfo{Identity: identity, Direction: broadcast.DirectionIngress,
		Transport: broadcast.TransportUpstream, Versions: usage.OnlyV1})
	defer upstream.Close()

	var first *videoFrame
	deadline := time.After(firstVideoWait)
wait:
	for {
		select {
		case <-runCtx.Done():
			return nil
		case err := <-c.failed:
			return err
		case frame := <-c.frames:
			if frame.key {
				first = &frame
				break wait
			}
		case <-deadline:
			break wait
		}
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	mkv := &mkvWriter{w: &meteredWriter{w: writer, usage: upstream, total: &r.env.IngressB}}
	var tracks []mkvTrack
	if first != nil {
		track := mkvTrack{number: 1, video: true, width: 1280, height: 720}
		switch first.codec {
		case "vp8":
			track.codecID = "V_VP8"
			if len(first.data) >= 10 {
				track.width = uint64(int(first.data[6])|int(first.data[7]&0x3f)<<8) & 0x3fff
				track.height = uint64(int(first.data[8])|int(first.data[9]&0x3f)<<8) & 0x3fff
			}
		case "vp9":
			track.codecID = "V_VP9"
		case "h264":
			track.codecID = "V_MPEG4/ISO/AVC"
			track.codecPrivate = first.data[:0]
			var au h264.AVCC
			if err := au.Unmarshal(first.data); err == nil {
				var sps, pps []byte
				for _, nalu := range au {
					switch h264.NALUType(nalu[0] & 0x1f) {
					case h264.NALUTypeSPS:
						sps = nalu
					case h264.NALUTypePPS:
						pps = nalu
					}
				}
				track.codecPrivate = avcConfiguration(sps, pps)
				var parsed h264.SPS
				if parsed.Unmarshal(sps) == nil {
					track.width, track.height = uint64(parsed.Width()), uint64(parsed.Height())
				}
			}
		}
		if track.width == 0 || track.height == 0 {
			track.width, track.height = 1280, 720
		}
		tracks = append(tracks, track)
	}
	for i := range c.slotCount {
		tracks = append(tracks, mkvTrack{number: uint64(2 + i), codecID: "A_OPUS", codecPrivate: opusHead(2), channels: 2})
	}
	plan := inputPlan{format: "matroska", hasVideo: first != nil, audioInputs: c.slotCount}
	encodeDone := make(chan error, 1)
	go func() { encodeDone <- r.encode(runCtx, reader, plan) }()
	writeErr := make(chan error, 1)
	go func() { writeErr <- c.write(runCtx, mkv, tracks, first) }()
	select {
	case <-runCtx.Done():
		_ = writer.Close()
		<-encodeDone
		return nil
	case err := <-c.failed:
		_ = writer.CloseWithError(err)
		<-encodeDone
		return err
	case err := <-writeErr:
		_ = writer.CloseWithError(err)
		<-encodeDone
		return err
	case err := <-encodeDone:
		_ = writer.Close()
		return err
	}
}

func (c *composer) write(ctx context.Context, mkv *mkvWriter, tracks []mkvTrack, first *videoFrame) error {
	if err := mkv.writeHeader(tracks); err != nil {
		return err
	}
	hasVideo := first != nil
	videoCodec := ""
	if first != nil {
		videoCodec = first.codec
		if err := mkv.writeBlock(1, first.at, true, first.data); err != nil {
			return err
		}
	}
	slots := make([]*audioSlot, 0, c.slotCount)
	c.mu.Lock()
	for _, mid := range c.audioMIDs {
		slots = append(slots, c.slots[mid])
	}
	c.mu.Unlock()
	now := c.elapsed()
	for _, slot := range slots {
		slot.mu.Lock()
		slot.next = now
		slot.mu.Unlock()
	}
	ticker := time.NewTicker(opusFrameMS * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case frame := <-c.frames:
			if !hasVideo {
				return errors.New("video was published after an audio-only start; restarting the composition")
			}
			if frame.codec != videoCodec {
				continue
			}
			if err := mkv.writeBlock(1, frame.at, frame.key, frame.data); err != nil {
				return err
			}
		case <-ticker.C:
			horizon := c.elapsed() + opusFrameMS
			for _, slot := range slots {
				slot.mu.Lock()
				for slot.next <= horizon {
					packet := opusSilence
					if len(slot.queue) > 0 {
						packet, slot.queue = slot.queue[0], slot.queue[1:]
					}
					duration := opus.PacketDuration2(packet) / 48
					if duration <= 0 {
						duration = opusFrameMS
					}
					if err := mkv.writeBlock(slot.number, slot.next, true, packet); err != nil {
						slot.mu.Unlock()
						return err
					}
					slot.next += duration
				}
				slot.mu.Unlock()
			}
		}
	}
}

func (c *composer) elapsed() int64 { return time.Since(c.start).Milliseconds() }

func (c *composer) connect(ctx context.Context, whep string) error {
	engine := &webrtc.MediaEngine{}
	for _, codec := range []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000,
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}}}, PayloadType: 96},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}}}, PayloadType: 98},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}}}, PayloadType: 102},
	} {
		if err := engine.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}
	if err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"},
		PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}
	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(engine, registry); err != nil {
		return err
	}
	settings := webrtc.SettingEngine{}
	config := c.role.env.Config
	if config.PublicIP != "" {
		if err := settings.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{External: []string{config.PublicIP},
			AsCandidateType: webrtc.ICECandidateTypeHost, Mode: webrtc.ICEAddressRewriteReplace}); err != nil {
			return err
		}
	}
	if config.UDPPortMin != 0 {
		if err := settings.SetEphemeralUDPPortRange(config.UDPPortMin, config.UDPPortMax); err != nil {
			return err
		}
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry), webrtc.WithSettingEngine(settings))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: c.iceServers(ctx, whep)})
	if err != nil {
		return err
	}
	c.pc = pc
	c.meter.FollowICE(pc, nil)
	recvonly := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, recvonly); err != nil {
		return err
	}
	for range c.slotCount {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, recvonly); err != nil {
			return err
		}
	}
	ordered := true
	channel, err := pc.CreateDataChannel(mc.EventsChannelLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return err
	}
	c.events = channel
	channel.OnMessage(func(message webrtc.DataChannelMessage) { c.onEvent(message.Data) })
	channel.OnClose(func() { c.failWith(errors.New("event channel closed")) })
	pc.OnTrack(c.onTrack)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			c.failWith(errors.New("sfu connection " + state.String()))
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, transceiver := range pc.GetTransceivers() {
		mid := transceiver.Mid()
		if transceiver.Kind() == webrtc.RTPCodecTypeVideo {
			c.videoMID = mid
			continue
		}
		c.audioMIDs = append(c.audioMIDs, mid)
		c.slots[mid] = &audioSlot{number: uint64(2 + len(c.audioMIDs) - 1)}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, whep, strings.NewReader(pc.LocalDescription().SDP))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/sdp")
	request.Header.Set("Authorization", "Bearer "+c.role.node.ServiceToken())
	response, err := broadcast.UpstreamClient(config, c.parent.TLSCertificateSHA256).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return fmt.Errorf("sfu WHEP: HTTP %d: %s", response.StatusCode, bytes.TrimSpace(answer))
	}
	return pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)})
}

func (c *composer) iceServers(ctx context.Context, whep string) []webrtc.ICEServer {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(whep, "/whep")+"/ice", nil)
	if err != nil {
		return nil
	}
	request.Header.Set("Authorization", "Bearer "+c.role.node.ServiceToken())
	response, err := broadcast.UpstreamClient(c.role.env.Config, c.parent.TLSCertificateSHA256).Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	var ice mc.ICEResponse
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&ice) != nil {
		return nil
	}
	var servers []webrtc.ICEServer
	for _, server := range ice.ICEServers {
		entry := webrtc.ICEServer{URLs: server.URLs}
		if server.Username != nil && server.Credential != nil {
			entry.Username, entry.Credential = *server.Username, *server.Credential
		}
		servers = append(servers, entry)
	}
	return servers
}

func (c *composer) command(value any) {
	encoded, err := mc.MarshalWire(value)
	if err == nil && c.events != nil {
		_ = c.events.SendText(string(encoded))
	}
}

func (c *composer) nextID() string {
	c.commandID++
	return "c" + strconv.Itoa(c.commandID)
}

func (c *composer) onEvent(data []byte) {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &head) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch head.Type {
	case "welcome":
		var event mc.WelcomeEvent
		if json.Unmarshal(data, &event) == nil {
			c.addSnapshot(event.Participants, event.Tracks)
		}
	case "snapshot":
		var event mc.SnapshotEvent
		if json.Unmarshal(data, &event) == nil {
			c.addSnapshot(event.Participants, event.Tracks)
		}
	case "participant_joined":
		var event mc.ParticipantEvent
		if json.Unmarshal(data, &event) == nil {
			c.joined[event.Participant.ParticipantID] = event.Participant.JoinedAtMS
		}
	case "participant_left":
		var event mc.ParticipantLeftEvent
		if json.Unmarshal(data, &event) == nil {
			delete(c.joined, event.ParticipantID)
			for id, track := range c.tracks {
				if track.ParticipantID == event.ParticipantID {
					delete(c.tracks, id)
				}
			}
		}
	case "track_published":
		var event mc.TrackPublishedEvent
		if json.Unmarshal(data, &event) == nil {
			c.tracks[event.Track.TrackID] = event.Track
		}
	case "track_unpublished":
		var event mc.TrackUnpublishedEvent
		if json.Unmarshal(data, &event) == nil {
			delete(c.tracks, event.TrackID)
		}
	case "track_muted":
		var event mc.TrackMutedEvent
		if json.Unmarshal(data, &event) == nil {
			if track, ok := c.tracks[event.TrackID]; ok {
				track.Muted = event.Muted
				c.tracks[event.TrackID] = track
			}
		}
	case "active_speakers":
		var event mc.ActiveSpeakersEvent
		if json.Unmarshal(data, &event) == nil && event.DominantID != nil {
			c.dominant = *event.DominantID
		}
	case "migrate", "connection_closed", "kicked":
		go c.failWith(errors.New("sfu asked the transcoder to reconnect (" + head.Type + ")"))
		return
	default:
		return
	}
	c.reselect()
}

func (c *composer) addSnapshot(participants []mc.Participant, tracks []mc.Track) {
	for _, participant := range participants {
		c.joined[participant.ParticipantID] = participant.JoinedAtMS
	}
	for _, track := range tracks {
		c.tracks[track.TrackID] = track
	}
}

func (c *composer) chooseVideo() string {
	var screens, cameras []mc.Track
	for _, track := range c.tracks {
		if track.Kind != "video" || (c.videoCodec != "" && track.Codec != c.videoCodec) ||
			!(track.Codec == "vp8" || track.Codec == "vp9" || track.Codec == "h264") {
			continue
		}
		switch track.Source {
		case "screen":
			screens = append(screens, track)
		case "camera", "custom":
			cameras = append(cameras, track)
		}
	}
	byJoin := func(tracks []mc.Track) {
		sort.Slice(tracks, func(i, j int) bool {
			left, right := c.joined[tracks[i].ParticipantID], c.joined[tracks[j].ParticipantID]
			if left != right {
				return left < right
			}
			return tracks[i].TrackID < tracks[j].TrackID
		})
	}
	if len(screens) > 0 {
		byJoin(screens)
		return screens[len(screens)-1].TrackID
	}
	byJoin(cameras)
	for _, track := range cameras {
		if track.ParticipantID == c.dominant && !track.Muted {
			return track.TrackID
		}
	}
	if current, ok := c.tracks[c.videoTrack]; ok && current.Kind == "video" {
		return current.TrackID
	}
	if len(cameras) > 0 {
		return cameras[0].TrackID
	}
	return ""
}

func (c *composer) reselect() {
	desired := c.chooseVideo()
	_, currentExists := c.tracks[c.videoTrack]
	hold := currentExists && time.Since(c.videoSince) < videoHold
	if desired != "" && desired != c.videoTrack && !hold && c.videoMID != "" {
		track := c.tracks[desired]
		var rid *string
		if len(track.Layers) > 0 {
			best := track.Layers[0]
			for _, layer := range track.Layers[1:] {
				if layer.Spatial > best.Spatial {
					best = layer
				}
			}
			rid = &best.RID
		}
		mid := c.videoMID
		c.command(mc.SubscribeCommand{CommandHeader: mc.CommandHeader{Type: "subscribe", ID: c.nextID()},
			TrackID: desired, MID: &mid, RID: rid})
		c.videoTrack, c.videoSince, c.needKey = desired, time.Now(), true
		if c.videoCodec == "" {
			c.videoCodec = track.Codec
		}
		if c.videoSSRC != 0 {
			go func(ssrc webrtc.SSRC) {
				_ = c.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)}})
			}(c.videoSSRC)
		}
	}
	bound := map[string]string{}
	for mid, trackID := range c.audioBinding {
		if _, ok := c.tracks[trackID]; ok {
			bound[trackID] = mid
			continue
		}
		delete(c.audioBinding, mid)
		c.command(mc.UnsubscribeCommand{CommandHeader: mc.CommandHeader{Type: "unsubscribe", ID: c.nextID()}, TrackID: trackID})
	}
	var audio []string
	for id, track := range c.tracks {
		if track.Kind == "audio" && track.Codec == "opus" {
			if _, ok := bound[id]; !ok {
				audio = append(audio, id)
			}
		}
	}
	sort.Strings(audio)
	if total := len(bound) + len(audio); total > c.slotCount && c.slotCount < maxAudioSlots {
		c.role.composeSlots = min(maxAudioSlots, (total+minAudioSlots-1)/minAudioSlots*minAudioSlots)
		go c.failWith(fmt.Errorf("%d audio tracks: restarting the composition with %d slots", total, c.role.composeSlots))
	}
	for _, trackID := range audio {
		for _, mid := range c.audioMIDs {
			if _, used := c.audioBinding[mid]; used {
				continue
			}
			slotMID := mid
			c.audioBinding[mid] = trackID
			c.command(mc.SubscribeCommand{CommandHeader: mc.CommandHeader{Type: "subscribe", ID: c.nextID()},
				TrackID: trackID, MID: &slotMID})
			break
		}
	}
}

func (c *composer) onTrack(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	mid := ""
	for _, transceiver := range c.pc.GetTransceivers() {
		if transceiver.Receiver() == receiver {
			mid = transceiver.Mid()
		}
	}
	go drainReceiver(receiver)
	if remote.Kind() == webrtc.RTPCodecTypeVideo {
		c.mu.Lock()
		c.videoSSRC = remote.SSRC()
		c.mu.Unlock()
		c.readVideo(remote)
		return
	}
	c.mu.Lock()
	slot := c.slots[mid]
	c.mu.Unlock()
	if slot == nil {
		return
	}
	var stream usage.Stream
	for {
		packet, attributes, err := remote.ReadRTP()
		if err != nil {
			return
		}
		c.meter.Received(&stream, packet, attributes)
		if len(packet.Payload) == 0 {
			continue
		}
		c.mu.Lock()
		_, bound := c.audioBinding[mid]
		c.mu.Unlock()
		if !bound {
			continue
		}
		slot.mu.Lock()
		slot.queue = append(slot.queue, append([]byte(nil), packet.Payload...))
		if len(slot.queue) > audioQueueFrames {
			slot.queue = slot.queue[len(slot.queue)-audioQueueFrames:]
		}
		slot.mu.Unlock()
	}
}

func (c *composer) readVideo(remote *webrtc.TrackRemote) {
	mime := strings.ToLower(remote.Codec().MimeType)
	var depacketizer rtp.Depacketizer
	codec := ""
	switch mime {
	case strings.ToLower(webrtc.MimeTypeVP8):
		depacketizer, codec = &codecs.VP8Packet{}, "vp8"
	case strings.ToLower(webrtc.MimeTypeVP9):
		depacketizer, codec = &codecs.VP9Packet{}, "vp9"
	case strings.ToLower(webrtc.MimeTypeH264):
		depacketizer, codec = &codecs.H264Packet{}, "h264"
	default:
		return
	}
	builder := samplebuilder.New(512, depacketizer, 90000)
	var stream usage.Stream
	for {
		packet, attributes, err := remote.ReadRTP()
		if err != nil {
			return
		}
		c.meter.Received(&stream, packet, attributes)
		builder.Push(packet)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			if len(sample.Data) == 0 {
				continue
			}
			data, key := sample.Data, false
			switch codec {
			case "vp8":
				key = data[0]&0x01 == 0
			case "vp9":
				key = vp9KeyFrame(data)
			case "h264":
				var au h264.AnnexB
				if au.Unmarshal(data) != nil || len(au) == 0 {
					continue
				}
				key = h264.IsRandomAccess(au)
				avcc, err := h264.AVCC(au).Marshal()
				if err != nil {
					continue
				}
				data = avcc
			}
			c.mu.Lock()
			if c.needKey {
				if !key {
					c.mu.Unlock()
					continue
				}
				c.needKey = false
			}
			c.mu.Unlock()
			select {
			case c.frames <- videoFrame{at: c.elapsed(), key: key, codec: codec, data: data}:
			default:
			}
		}
	}
}

func vp9KeyFrame(data []byte) bool {
	if len(data) == 0 || data[0]>>6 != 2 {
		return false
	}
	profile := int(data[0]>>5&1) | int(data[0]>>4&1)<<1
	bit := 4
	if profile == 3 {
		bit++
	}
	if data[0]>>(7-bit)&1 == 1 {
		return false
	}
	bit++
	return data[0]>>(7-bit)&1 == 0
}

func drainReceiver(receiver *webrtc.RTPReceiver) {
	buffer := make([]byte, 1500)
	for {
		if _, _, err := receiver.Read(buffer); err != nil {
			return
		}
	}
}

type meteredWriter struct {
	w     io.Writer
	usage *broadcast.Connection
	total interface{ Add(int64) int64 }
}

func (m *meteredWriter) Write(p []byte) (int, error) {
	n, err := m.w.Write(p)
	m.usage.AddV1(n)
	m.total.Add(int64(n))
	return n, err
}
