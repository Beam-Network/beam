package relay

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/sfu"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
	"github.com/Beam-Network/beam/internal/workload/handlers/workertls"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

func (r *Relay) newUpstream(parent parentRef) *upstream {
	return &upstream{relay: r, parent: parent, bindings: map[string]*binding{}, bound: map[string]string{},
		idle: map[webrtc.RTPCodecType][]string{}, pendingCommands: map[string]chan commandReply{},
		seenParticipants: map[string]bool{}, seenTracks: map[string]bool{}, trigger: make(chan struct{}, 1),
		welcomed: make(chan struct{}), done: make(chan struct{})}
}

type upstream struct {
	relay  *Relay
	parent parentRef

	pc       *webrtc.PeerConnection
	dc       *webrtc.DataChannel
	client   *http.Client
	resource string
	usage    *usage.Connection

	mu               sync.Mutex
	bindings         map[string]*binding
	bound            map[string]string
	idle             map[webrtc.RTPCodecType][]string
	pendingCommands  map[string]chan commandReply
	seenParticipants map[string]bool
	seenTracks       map[string]bool
	welcomeDone      bool

	trigger  chan struct{}
	welcomed chan struct{}
	done     chan struct{}
	once     sync.Once
	closed   atomic.Bool
}

type binding struct {
	trackID string
	rid     string
	kind    webrtc.RTPCodecType
	layer   *sfu.Layer
	fed     atomic.Bool
}

type commandReply struct {
	kind   string
	sdp    string
	detail string
}

func (u *upstream) httpClient() *http.Client {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if u.parent.pin != "" {
		pin := strings.ToLower(u.parent.pin)
		config.ServerName = workertls.ServerName(pin)
		config.InsecureSkipVerify = true
		config.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("relay: parent sent no certificate")
			}
			digest := sha256.Sum256(raw[0])
			if hex.EncodeToString(digest[:]) != pin {
				return errors.New("relay: parent certificate does not match its pin")
			}
			return nil
		}
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: config, ForceAttemptHTTP2: true}}
}

func (u *upstream) connect(ctx context.Context) error {
	r := u.relay
	api, err := r.opts.Factory.Upstream(r.opts.E2EE)
	if err != nil {
		return err
	}
	servers := make([]webrtc.ICEServer, 0, len(r.opts.ICEServers))
	for _, server := range r.opts.ICEServers {
		if strings.HasPrefix(strings.ToLower(server), "stun:") {
			servers = append(servers, webrtc.ICEServer{URLs: []string{server}})
		}
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: servers, BundlePolicy: webrtc.BundlePolicyMaxBundle})
	if err != nil {
		return err
	}
	u.locked(func() { u.pc = pc })
	ordered := true
	dc, err := pc.CreateDataChannel(mediacontracts.EventsChannelLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return err
	}
	u.dc = dc
	dc.OnMessage(func(message webrtc.DataChannelMessage) {
		defer u.guard("parent event")
		if message.IsString {
			u.handleEvent(message.Data)
		}
	})
	dc.OnClose(func() { u.close() })
	pc.OnTrack(u.onTrack)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		defer u.guard("upstream state")
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			u.close()
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
	}
	u.client = u.httpClient()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.parent.url, strings.NewReader(pc.LocalDescription().SDP))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/sdp")
	request.Header.Set("Authorization", "Bearer "+r.opts.ServiceToken)
	response, err := u.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("parent answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	if location := response.Header.Get("Location"); location != "" {
		if resolved, err := url.Parse(u.parent.url); err == nil {
			if reference, err := url.Parse(location); err == nil {
				u.resource = resolved.ResolveReference(reference).String()
			}
		}
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(body)}); err != nil {
		return err
	}
	identity := mediacontracts.NodeIdentityPrefix + u.parent.nodeID
	u.usage = r.opts.Usage.Open(usage.ConnectionInfo{Identity: &identity, Direction: usage.DirectionIngress,
		Transport: usage.TransportUpstream, Detached: true})
	u.usage.FollowICE(pc, nil)
	go u.negotiateLoop()
	go u.reportListeners()
	return nil
}

func (u *upstream) close() {
	u.once.Do(func() {
		u.closed.Store(true)
		close(u.done)
		var pc *webrtc.PeerConnection
		u.locked(func() {
			pc = u.pc
			for id, reply := range u.pendingCommands {
				close(reply)
				delete(u.pendingCommands, id)
			}
		})
		if u.resource != "" && u.client != nil {
			go func() {
				request, err := http.NewRequest(http.MethodDelete, u.resource, nil)
				if err != nil {
					return
				}
				request.Header.Set("Authorization", "Bearer "+u.relay.opts.ServiceToken)
				if response, err := u.client.Do(request); err == nil {
					_ = response.Body.Close()
				}
			}()
		}
		if pc != nil {
			_ = pc.Close()
		}
		u.usage.Close()
	})
}

func (u *upstream) ready() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.welcomeDone {
		return false
	}
	_, remoteTracks := u.relay.opts.Session.RemoteState()
	for track := range remoteTracks {
		if !u.seenTracks[track] {
			continue
		}
		boundAny := false
		for key := range u.bound {
			if strings.HasPrefix(key, track+"/") {
				boundAny = true
				break
			}
		}
		if !boundAny {
			return false
		}
	}
	for _, value := range u.bindings {
		if value.layer != nil && !value.fed.Load() && value.kind == webrtc.RTPCodecTypeVideo {
			return false
		}
	}
	return true
}

func (u *upstream) reconcile() {
	session := u.relay.opts.Session
	participants, tracks := session.RemoteState()
	var seenParticipants, seenTracks map[string]bool
	u.locked(func() { seenParticipants, seenTracks = u.seenParticipants, u.seenTracks })
	for id := range tracks {
		if !seenTracks[id] {
			session.RemoveTrack(id)
		}
	}
	for id := range participants {
		if !seenParticipants[id] {
			session.RemoveRemoteParticipant(id, "moved")
		}
	}
}

type eventHead struct {
	Type string `json:"type"`
}

func (u *upstream) handleEvent(data []byte) {
	var head eventHead
	if json.Unmarshal(data, &head) != nil {
		return
	}
	session := u.relay.opts.Session
	switch head.Type {
	case "welcome":
		var value mediacontracts.WelcomeEvent
		if json.Unmarshal(data, &value) != nil {
			return
		}
		u.mirror(value.Participants, value.Tracks)
		session.SetUpstreamListeners(value.Listeners)
		if value.SnapshotComplete {
			u.welcome()
		}
	case "snapshot":
		var value mediacontracts.SnapshotEvent
		if json.Unmarshal(data, &value) != nil {
			return
		}
		u.mirror(value.Participants, value.Tracks)
		if value.Complete {
			u.welcome()
		}
	case "participant_joined", "participant_updated":
		var value mediacontracts.ParticipantEvent
		if json.Unmarshal(data, &value) == nil {
			u.mirror([]mediacontracts.Participant{value.Participant}, nil)
		}
	case "participant_left":
		var value mediacontracts.ParticipantLeftEvent
		if json.Unmarshal(data, &value) == nil && u.isActive() {
			session.RemoveRemoteParticipant(value.ParticipantID, value.Reason)
		}
	case "track_published":
		var value mediacontracts.TrackPublishedEvent
		if json.Unmarshal(data, &value) == nil {
			u.mirror(nil, []mediacontracts.Track{value.Track})
		}
	case "track_unpublished":
		var value mediacontracts.TrackUnpublishedEvent
		if json.Unmarshal(data, &value) == nil {
			u.unbindTrack(value.TrackID)
			if u.isActive() {
				session.RemoveTrack(value.TrackID)
			}
		}
	case "track_state":
		var value mediacontracts.TrackStateEvent
		if json.Unmarshal(data, &value) == nil && value.Validate() == nil {
			session.SetTrackState(value.TrackID, value.State, value.GraceUntilMS)
		}
	case "track_muted":
		var value mediacontracts.TrackMutedEvent
		if json.Unmarshal(data, &value) == nil && u.isActive() {
			session.SetTrackMuted(value.TrackID, value.Muted)
		}
	case "active_speakers":
		var value mediacontracts.ActiveSpeakersEvent
		if json.Unmarshal(data, &value) == nil && u.isActive() {
			session.ForwardSpeakers(value.Speakers, value.DominantID)
		}
	case "listener_count":
		var value mediacontracts.ListenerCountEvent
		if json.Unmarshal(data, &value) == nil && u.isActive() {
			session.SetUpstreamListeners(value.Listeners)
		}
	case "answer":
		var value mediacontracts.AnswerEvent
		if json.Unmarshal(data, &value) == nil {
			u.reply(value.ID, commandReply{kind: "answer", sdp: value.SDP})
		}
	case "ack":
		var value mediacontracts.AckEvent
		if json.Unmarshal(data, &value) == nil {
			u.reply(value.ID, commandReply{kind: "ack"})
		}
	case "error":
		var value mediacontracts.ErrorEvent
		if json.Unmarshal(data, &value) == nil && value.ID != nil {
			detail := value.Code
			if value.Detail != nil {
				detail += ": " + *value.Detail
			}
			u.reply(*value.ID, commandReply{kind: "error", detail: detail})
		}
	case "slot_released":
		var value mediacontracts.SlotReleasedEvent
		if json.Unmarshal(data, &value) == nil {
			u.releaseMID(value.MID)
		}
	case "connection_closed", "kicked":
		u.relay.opts.Logf("relay %s: parent %s closed the upstream (%s)", u.relay.opts.NodeID, u.parent.nodeID, head.Type)
	}
}

func (u *upstream) isActive() bool {
	u.relay.mu.Lock()
	defer u.relay.mu.Unlock()
	return u.relay.active == u
}

func (u *upstream) welcome() {
	first := false
	u.locked(func() {
		first = !u.welcomeDone
		u.welcomeDone = true
	})
	if first {
		close(u.welcomed)
		if u.isActive() {
			u.reconcile()
		}
	}
}

func (u *upstream) mirror(participants []mediacontracts.Participant, tracks []mediacontracts.Track) {
	session := u.relay.opts.Session
	for _, participant := range participants {
		u.locked(func() { u.seenParticipants[participant.ParticipantID] = true })
		session.UpsertRemoteParticipant(participant)
	}
	for _, track := range tracks {
		u.locked(func() { u.seenTracks[track.TrackID] = true })
		session.UpsertRemoteTrack(track)
	}
	if len(tracks) > 0 {
		select {
		case u.trigger <- struct{}{}:
		default:
		}
	}
}

func (u *upstream) reply(id string, value commandReply) {
	var channel chan commandReply
	u.locked(func() {
		channel = u.pendingCommands[id]
		delete(u.pendingCommands, id)
	})
	if channel != nil {
		channel <- value
		close(channel)
	}
}

func (u *upstream) command(value map[string]any) (commandReply, error) {
	id := fmt.Sprintf("r%d", u.relay.nextID.Add(1))
	value["id"] = id
	encoded, err := json.Marshal(value)
	if err != nil {
		return commandReply{}, err
	}
	reply := make(chan commandReply, 1)
	open := false
	u.locked(func() {
		if open = !u.closed.Load(); open {
			u.pendingCommands[id] = reply
		}
	})
	if !open {
		return commandReply{}, errors.New("upstream closed")
	}
	if err := u.dc.SendText(string(encoded)); err != nil {
		return commandReply{}, err
	}
	select {
	case result, ok := <-reply:
		if !ok {
			return commandReply{}, errors.New("upstream closed")
		}
		if result.kind == "error" {
			return result, errors.New(result.detail)
		}
		return result, nil
	case <-time.After(commandTimeout):
		u.locked(func() { delete(u.pendingCommands, id) })
		return commandReply{}, errors.New("command timed out")
	case <-u.done:
		return commandReply{}, errors.New("upstream closed")
	}
}

const listenerReportInterval = 5 * time.Second

func (u *upstream) reportListeners() {
	defer u.guard("listener report")
	select {
	case <-u.welcomed:
	case <-u.done:
		return
	}
	const maximumBackoff = 10 * time.Minute
	last, logged := int64(-1), false
	wait := listenerReportInterval
	for {
		if total := u.relay.opts.Session.ListenerReport(); total != last {
			_, err := u.command(map[string]any{"type": "listener_report", "listeners": total})
			switch {
			case err == nil:
				last, wait = total, listenerReportInterval
			case u.closed.Load():
				return
			default:
				if !logged {
					u.relay.opts.Logf("relay %s: parent %s refuses listener_report (%v); retrying with backoff",
						u.relay.opts.NodeID, u.parent.nodeID, err)
					logged = true
				}
				wait = min(wait*2, maximumBackoff)
			}
		}
		select {
		case <-u.done:
			return
		case <-time.After(wait):
		}
	}
}

type wantedLayer struct {
	trackID string
	rid     string
	kind    webrtc.RTPCodecType
}

func (u *upstream) negotiateLoop() {
	defer u.guard("upstream negotiation")
	open := make(chan struct{})
	u.dc.OnOpen(func() { close(open) })
	if u.dc.ReadyState() == webrtc.DataChannelStateOpen {
		select {
		case <-open:
		default:
			close(open)
		}
	}
	select {
	case <-open:
	case <-u.done:
		return
	case <-time.After(commandTimeout):
		u.close()
		return
	}
	for {
		select {
		case <-u.done:
			return
		case <-u.trigger:
		case <-time.After(2 * time.Second):
		}
		if err := u.bindMissing(); err != nil && !u.closed.Load() {
			u.relay.opts.Logf("relay %s: binding layers from %s: %v", u.relay.opts.NodeID, u.parent.nodeID, err)
		}
	}
}

func (u *upstream) missing() []wantedLayer {
	session := u.relay.opts.Session
	var tracks []string
	u.locked(func() {
		tracks = make([]string, 0, len(u.seenTracks))
		for id := range u.seenTracks {
			tracks = append(tracks, id)
		}
	})
	var wanted []wantedLayer
	for _, id := range tracks {
		track := session.Track(id)
		if track == nil {
			continue
		}
		info := track.Info()
		kind := webrtc.RTPCodecTypeVideo
		if info.Kind == "audio" {
			kind = webrtc.RTPCodecTypeAudio
		}
		rids := []string{""}
		if kind == webrtc.RTPCodecTypeVideo && len(info.Layers) > 0 {
			rids = rids[:0]
			for _, layer := range info.Layers {
				rids = append(rids, layer.RID)
			}
		}
		u.locked(func() {
			for _, rid := range rids {
				if _, ok := u.bound[id+"/"+rid]; !ok {
					wanted = append(wanted, wantedLayer{trackID: id, rid: rid, kind: kind})
				}
			}
		})
	}
	return wanted
}

func (u *upstream) bindMissing() error {
	wanted := u.missing()
	if len(wanted) == 0 {
		return nil
	}
	var fresh []wantedLayer
	for _, layer := range wanted {
		mid := ""
		u.locked(func() {
			if idle := u.idle[layer.kind]; len(idle) > 0 {
				mid, u.idle[layer.kind] = idle[0], idle[1:]
			}
		})
		if mid == "" {
			fresh = append(fresh, layer)
			continue
		}
		if err := u.subscribe(layer, mid); err != nil {
			return err
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	transceivers := make([]*webrtc.RTPTransceiver, 0, len(fresh))
	for _, layer := range fresh {
		transceiver, err := u.pc.AddTransceiverFromKind(layer.kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
		if err != nil {
			return err
		}
		transceivers = append(transceivers, transceiver)
	}
	offer, err := u.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := u.pc.SetLocalDescription(offer); err != nil {
		return err
	}
	reply, err := u.command(map[string]any{"type": "offer", "sdp": u.pc.LocalDescription().SDP})
	if err != nil {
		return err
	}
	if err := u.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: reply.sdp}); err != nil {
		return err
	}
	for index, transceiver := range transceivers {
		if err := u.subscribe(fresh[index], transceiver.Mid()); err != nil {
			return err
		}
	}
	return nil
}

func (u *upstream) subscribe(layer wantedLayer, mid string) error {
	session := u.relay.opts.Session
	track := session.Track(layer.trackID)
	if track == nil {
		u.releaseMID(mid)
		return nil
	}
	local := track.Layer(layer.rid)
	value := &binding{trackID: layer.trackID, rid: layer.rid, kind: layer.kind, layer: local}
	u.locked(func() {
		u.bindings[mid] = value
		u.bound[layer.trackID+"/"+layer.rid] = mid
	})
	var rid any
	if layer.rid != "" {
		rid = layer.rid
	}
	if _, err := u.command(map[string]any{"type": "subscribe", "track_id": layer.trackID, "mid": mid, "rid": rid,
		"max_spatial": nil, "max_temporal": nil, "width": nil, "height": nil}); err != nil {
		u.locked(func() {
			delete(u.bindings, mid)
			delete(u.bound, layer.trackID+"/"+layer.rid)
			u.idle[layer.kind] = append(u.idle[layer.kind], mid)
		})
		return err
	}
	return nil
}

func (u *upstream) unbindTrack(trackID string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.seenTracks, trackID)
	for key, mid := range u.bound {
		if strings.HasPrefix(key, trackID+"/") {
			delete(u.bound, key)
			if value := u.bindings[mid]; value != nil {
				delete(u.bindings, mid)
				u.idle[value.kind] = append(u.idle[value.kind], mid)
			}
		}
	}
}

func (u *upstream) releaseMID(mid string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	value := u.bindings[mid]
	if value == nil {
		return
	}
	delete(u.bindings, mid)
	delete(u.bound, value.trackID+"/"+value.rid)
	u.idle[value.kind] = append(u.idle[value.kind], mid)
}

func (u *upstream) onTrack(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	defer u.guard("upstream track")
	mid := ""
	for _, transceiver := range u.pc.GetTransceivers() {
		if transceiver.Receiver() == receiver {
			mid = transceiver.Mid()
			break
		}
	}
	ssrc := uint32(remote.SSRC())
	pc := u.pc
	ids := sfu.ExtensionIDs{}
	for _, extension := range receiver.GetParameters().HeaderExtensions {
		switch extension.URI {
		case sfu.DependencyDescriptorURI:
			ids.DependencyDescriptor = uint8(extension.ID)
		case sfu.AudioLevelURI:
			ids.AudioLevel = uint8(extension.ID)
		}
	}
	mime := strings.ToLower(remote.Codec().MimeType)
	rid := remote.RID()
	go func() {
		defer u.guard("upstream rtcp")
		buffer := make([]byte, 1500)
		for {
			var err error
			if rid != "" {
				_, _, err = receiver.ReadSimulcast(buffer, rid)
			} else {
				_, _, err = receiver.Read(buffer)
			}
			if err != nil {
				return
			}
		}
	}()
	var current *binding
	var source *sfu.Source
	var stream usage.Stream
	for {
		packet, attributes, err := remote.ReadRTP()
		if err != nil {
			return
		}
		u.usage.Received(&stream, packet, attributes)
		var value *binding
		u.locked(func() { value = u.bindings[mid] })
		if value == nil {
			continue
		}
		if value != current {
			current = value
			source = sfu.NewSource(func() {
				_ = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
			})
		}
		value.fed.Store(true)
		value.layer.Feed(source, packet, ids, mime)
	}
}
