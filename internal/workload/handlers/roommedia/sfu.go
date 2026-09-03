package roommedia

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
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

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const maxOfferBytes = 1 << 20
const mediaReceiveMTU = 8192

type sfu struct {
	mu                sync.RWMutex
	api               *webrtc.API
	roomID            string
	config            Config
	peerConfiguration webrtc.Configuration
	publisher         *webrtc.PeerConnection
	publisherEpoch    uint64
	publisherTimer    *time.Timer
	viewers           map[string]*webrtc.PeerConnection
	tracks            map[string]*mediaTrack
	publisherClosed   chan struct{}
	closePublisher    sync.Once
	closed            chan struct{}
	closeSFU          sync.Once
	sequence          atomic.Uint64
	packets           atomic.Int64
	bytes             atomic.Int64
	dropped           atomic.Int64
}

type mediaTrack struct {
	id      string
	kind    string
	codec   webrtc.RTPCodecCapability
	local   *webrtc.TrackLocalStaticRTP
	ssrc    webrtc.SSRC
	rebase  rtpRebaser
	packets atomic.Int64
	bytes   atomic.Int64
	dropped atomic.Int64
}

type rtpRebaser struct {
	mu                  sync.Mutex
	source              *webrtc.PeerConnection
	sourceStarted       bool
	haveOutput          bool
	inputBaseSequence   uint16
	inputBaseTimestamp  uint32
	outputBaseSequence  uint16
	outputBaseTimestamp uint32
	lastOutputSequence  uint16
	lastOutputTimestamp uint32
	timestampStep       uint32
}

func newSFU(config Config, roomID string, expiresAt time.Time) (*sfu, error) {
	if config.PublisherReconnectGrace <= 0 {
		config.PublisherReconnectGrace = 30 * time.Second
	}
	engine := &webrtc.MediaEngine{}
	if err := engine.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(engine, interceptors); err != nil {
		return nil, err
	}
	settings := webrtc.SettingEngine{}
	settings.SetReceiveMTU(mediaReceiveMTU)
	if config.PublicIP != "" {
		settings.SetNAT1To1IPs([]string{config.PublicIP}, webrtc.ICECandidateTypeHost)
	}
	if config.UDPPortMin != 0 {
		if err := settings.SetEphemeralUDPPortRange(config.UDPPortMin, config.UDPPortMax); err != nil {
			return nil, err
		}
	}
	peerConfiguration := buildPeerConfiguration(config, roomID, expiresAt)
	media := &sfu{api: webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(interceptors),
		webrtc.WithSettingEngine(settings)), roomID: roomID, config: config, viewers: make(map[string]*webrtc.PeerConnection),
		peerConfiguration: peerConfiguration, tracks: make(map[string]*mediaTrack), publisherClosed: make(chan struct{}), closed: make(chan struct{})}
	go media.keyframeLoop()
	return media, nil
}

func (s *sfu) Handler(baseURL string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/rooms/{room_id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("room_id") != s.roomID {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, s.status(baseURL))
	})
	mux.HandleFunc("GET /v1/rooms/{room_id}/ice", s.ice)
	mux.HandleFunc("POST /v1/rooms/{room_id}/whip", s.whip)
	mux.HandleFunc("POST /v1/rooms/{room_id}/whep", s.whep)
	mux.HandleFunc("DELETE /v1/rooms/{room_id}/participants/{participant_id}", s.deleteParticipant)
	return mux
}

func (s *sfu) peerConfig() webrtc.Configuration {
	return s.peerConfiguration
}

func buildPeerConfiguration(config Config, roomID string, expiresAt time.Time) webrtc.Configuration {
	servers := make([]webrtc.ICEServer, 0, len(config.ICEServers))
	username, credential := turnCredentials(config, roomID, expiresAt)
	for _, endpoint := range config.ICEServers {
		server := webrtc.ICEServer{URLs: []string{endpoint}}
		lower := strings.ToLower(strings.TrimSpace(endpoint))
		if username != "" && (strings.HasPrefix(lower, "turn:") || strings.HasPrefix(lower, "turns:")) {
			server.Username = username
			server.Credential = credential
			server.CredentialType = webrtc.ICECredentialTypePassword
		}
		servers = append(servers, server)
	}
	return webrtc.Configuration{ICEServers: servers}
}

func turnCredentials(config Config, roomID string, expiresAt time.Time) (string, string) {
	secret := strings.TrimSpace(config.TURNSecret)
	if secret == "" {
		return "", ""
	}
	deadline := time.Now().Add(config.TURNCredentialTTL)
	if expiresAt.After(deadline) {
		deadline = expiresAt.Add(5 * time.Minute)
	}
	prefix := strings.NewReplacer(":", "_", " ", "_").Replace(strings.TrimSpace(config.TURNUsernamePrefix))
	session := strings.NewReplacer(":", "_", " ", "_").Replace(roomID)
	username := fmt.Sprintf("%d:%s:%s", deadline.Unix(), prefix, session)
	digest := hmac.New(sha1.New, []byte(secret))
	_, _ = digest.Write([]byte(username))
	return username, base64.StdEncoding.EncodeToString(digest.Sum(nil))
}

func (s *sfu) ice(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("room_id") != s.roomID {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ice_servers": s.peerConfig().ICEServers})
}

func (s *sfu) whip(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("room_id") != s.roomID {
		http.NotFound(w, r)
		return
	}
	offer, raw, err := readOffer(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	pc, err := s.api.NewPeerConnection(s.peerConfig())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	pc.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		item := s.publisherTrack(pc, remote)
		if item == nil {
			s.dropped.Add(1)
			_ = pc.Close()
			return
		}
		go drainReceiver(receiver)
		for {
			packet, _, readErr := remote.ReadRTP()
			if readErr != nil {
				return
			}
			if !s.isPublisher(pc) {
				return
			}
			if !item.rebasePacket(pc, packet) {
				return
			}
			if writeErr := item.local.WriteRTP(packet); writeErr != nil {
				item.dropped.Add(1)
				s.dropped.Add(1)
				continue
			}
			n := packet.MarshalSize()
			item.packets.Add(1)
			item.bytes.Add(int64(n))
			s.packets.Add(1)
			s.bytes.Add(int64(n))
			s.sequence.Add(1)
		}
	})
	closePeerAfterDisconnect(pc, func() { s.publisherDisconnected(pc) })
	answer, err := acceptOffer(pc, offer)
	if err != nil {
		_ = pc.Close()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	previous := s.publisher
	s.publisher = pc
	s.publisherEpoch++
	if s.publisherTimer != nil {
		s.publisherTimer.Stop()
		s.publisherTimer = nil
	}
	s.mu.Unlock()
	if previous != nil && previous != pc {
		_ = previous.Close()
	}
	s.writeAnswer(w, r, "publisher", answer, raw)
}

func (s *sfu) publisherTrack(pc *webrtc.PeerConnection, remote *webrtc.TrackRemote) *mediaTrack {
	codec := remote.Codec().RTPCodecCapability
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publisher != pc {
		return nil
	}
	item := s.tracks[remote.ID()]
	if item == nil {
		for _, candidate := range s.tracks {
			if candidate.kind == remote.Kind().String() && sameCodec(candidate.codec, codec) && !candidate.rebase.activeFor(pc) {
				item = candidate
				break
			}
		}
	}
	if item == nil {
		track, err := webrtc.NewTrackLocalStaticRTP(codec, remote.ID(), remote.StreamID())
		if err != nil {
			return nil
		}
		item = &mediaTrack{id: remote.ID(), kind: remote.Kind().String(), codec: codec, local: track}
		s.tracks[item.id] = item
	} else if item.kind != remote.Kind().String() || !sameCodec(item.codec, codec) {
		return nil
	}
	item.ssrc = remote.SSRC()
	item.rebase.activate(pc)
	return item
}

func (track *mediaTrack) rebasePacket(source *webrtc.PeerConnection, packet *rtp.Packet) bool {
	track.rebase.mu.Lock()
	defer track.rebase.mu.Unlock()
	if track.rebase.source != source {
		return false
	}
	if !track.rebase.sourceStarted {
		track.rebase.inputBaseSequence = packet.SequenceNumber
		track.rebase.inputBaseTimestamp = packet.Timestamp
		if track.rebase.haveOutput {
			track.rebase.outputBaseSequence = track.rebase.lastOutputSequence + 1
			step := track.rebase.timestampStep
			if step == 0 {
				step = track.defaultTimestampStep()
			}
			track.rebase.outputBaseTimestamp = track.rebase.lastOutputTimestamp + step
		} else {
			track.rebase.outputBaseSequence = packet.SequenceNumber
			track.rebase.outputBaseTimestamp = packet.Timestamp
		}
		track.rebase.sourceStarted = true
	}
	packet.SequenceNumber = track.rebase.outputBaseSequence + (packet.SequenceNumber - track.rebase.inputBaseSequence)
	packet.Timestamp = track.rebase.outputBaseTimestamp + (packet.Timestamp - track.rebase.inputBaseTimestamp)
	if track.rebase.haveOutput {
		if step := packet.Timestamp - track.rebase.lastOutputTimestamp; step > 0 && step <= track.codec.ClockRate {
			track.rebase.timestampStep = step
		}
	}
	track.rebase.lastOutputSequence = packet.SequenceNumber
	track.rebase.lastOutputTimestamp = packet.Timestamp
	track.rebase.haveOutput = true
	return true
}

func (track *mediaTrack) defaultTimestampStep() uint32 {
	if track.kind == webrtc.RTPCodecTypeAudio.String() {
		return track.codec.ClockRate / 50
	}
	return track.codec.ClockRate / 30
}

func (rebase *rtpRebaser) activate(source *webrtc.PeerConnection) {
	rebase.mu.Lock()
	rebase.source = source
	rebase.sourceStarted = false
	rebase.mu.Unlock()
}

func (rebase *rtpRebaser) activeFor(source *webrtc.PeerConnection) bool {
	rebase.mu.Lock()
	defer rebase.mu.Unlock()
	return rebase.source == source
}

func sameCodec(left, right webrtc.RTPCodecCapability) bool {
	return strings.EqualFold(strings.TrimSpace(left.MimeType), strings.TrimSpace(right.MimeType)) &&
		left.ClockRate == right.ClockRate && left.Channels == right.Channels &&
		strings.TrimSpace(left.SDPFmtpLine) == strings.TrimSpace(right.SDPFmtpLine)
}

func (s *sfu) isPublisher(pc *webrtc.PeerConnection) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.publisher == pc
}

func (s *sfu) publisherDisconnected(pc *webrtc.PeerConnection) {
	s.mu.Lock()
	if s.publisher != pc {
		s.mu.Unlock()
		return
	}
	s.publisher = nil
	s.publisherEpoch++
	epoch := s.publisherEpoch
	if s.publisherTimer != nil {
		s.publisherTimer.Stop()
	}
	s.publisherTimer = time.AfterFunc(s.config.PublisherReconnectGrace, func() {
		s.publisherGraceExpired(epoch)
	})
	s.mu.Unlock()
	_ = pc.Close()
}

func (s *sfu) publisherGraceExpired(epoch uint64) {
	s.mu.Lock()
	if s.publisher != nil || s.publisherEpoch != epoch {
		s.mu.Unlock()
		return
	}
	s.publisherTimer = nil
	s.mu.Unlock()
	s.closePublisher.Do(func() { close(s.publisherClosed) })
}

func (s *sfu) whep(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("room_id") != s.roomID {
		http.NotFound(w, r)
		return
	}
	offer, raw, err := readOffer(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.mu.RLock()
	tracks := make([]*mediaTrack, 0, len(s.tracks))
	for _, track := range s.tracks {
		tracks = append(tracks, track)
	}
	viewers := len(s.viewers)
	s.mu.RUnlock()
	if len(tracks) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "publisher has no media tracks yet"})
		return
	}
	if viewers >= s.config.MaxViewers {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "viewer limit reached"})
		return
	}
	pc, err := s.api.NewPeerConnection(s.peerConfig())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	for _, track := range tracks {
		sender, addErr := pc.AddTrack(track.local)
		if addErr != nil {
			_ = pc.Close()
			writeJSON(w, 500, map[string]string{"error": addErr.Error()})
			return
		}
		go drainSender(sender)
	}
	viewerID, err := randomToken(12)
	if err != nil {
		_ = pc.Close()
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	closePeerAfterDisconnect(pc, func() { s.removeViewer(viewerID, pc) })
	answer, err := acceptOffer(pc, offer)
	if err != nil {
		_ = pc.Close()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	s.viewers[viewerID] = pc
	s.mu.Unlock()
	s.requestKeyframes()
	s.writeAnswer(w, r, viewerID, answer, raw)
}

func acceptOffer(pc *webrtc.PeerConnection, offer webrtc.SessionDescription) (string, error) {
	if err := pc.SetRemoteDescription(offer); err != nil {
		return "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	<-gathered
	return pc.LocalDescription().SDP, nil
}

func readOffer(r *http.Request) (webrtc.SessionDescription, bool, error) {
	payload, err := io.ReadAll(io.LimitReader(r.Body, maxOfferBytes+1))
	if err != nil || len(payload) > maxOfferBytes {
		return webrtc.SessionDescription{}, false, errors.New("invalid SDP offer")
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/sdp") {
		if len(payload) == 0 {
			return webrtc.SessionDescription{}, true, errors.New("SDP offer is empty")
		}
		return webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(payload)}, true, nil
	}
	var value struct{ Type, SDP string }
	if json.Unmarshal(payload, &value) != nil || value.Type != "offer" || value.SDP == "" {
		return webrtc.SessionDescription{}, false, errors.New("invalid JSON SDP offer")
	}
	return webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: value.SDP}, false, nil
}

func (s *sfu) writeAnswer(w http.ResponseWriter, r *http.Request, participantID, answer string, raw bool) {
	w.Header().Set("Location", strings.TrimRight(r.URL.Path, "/")+"/../participants/"+url.PathEscape(participantID))
	if raw {
		w.Header().Set("Content-Type", "application/sdp")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, answer)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"type": "answer", "sdp": answer, "participant_id": participantID})
}

func (s *sfu) deleteParticipant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("participant_id")
	if id == "publisher" {
		s.mu.RLock()
		pc := s.publisher
		s.mu.RUnlock()
		if pc != nil {
			s.publisherDisconnected(pc)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mu.Lock()
	pc := s.viewers[id]
	delete(s.viewers, id)
	s.mu.Unlock()
	if pc != nil {
		_ = pc.Close()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *sfu) removeViewer(id string, pc *webrtc.PeerConnection) {
	s.mu.Lock()
	found := s.viewers[id] == pc
	if s.viewers[id] == pc {
		delete(s.viewers, id)
	}
	s.mu.Unlock()
	if found {
		_ = pc.Close()
	}
}

func (s *sfu) status(baseURL string) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]any{"room_id": s.roomID, "transport": "worker_sfu", "publisher": s.publisher != nil,
		"viewers": len(s.viewers), "tracks": len(s.tracks), "urls": map[string]string{"whip": baseURL + "/whip", "whep": baseURL + "/whep", "ice": baseURL + "/ice"}}
}

func (s *sfu) trackCounters() []contracts.MediaTrackCounters {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]contracts.MediaTrackCounters, 0, len(s.tracks))
	for _, track := range s.tracks {
		result = append(result, contracts.MediaTrackCounters{TrackID: track.id, Packets: track.packets.Load(), Bytes: track.bytes.Load(), Dropped: track.dropped.Load()})
	}
	return result
}

func (s *sfu) Close() {
	s.closeSFU.Do(func() { close(s.closed) })
	s.mu.Lock()
	publisher := s.publisher
	s.publisher = nil
	s.publisherEpoch++
	if s.publisherTimer != nil {
		s.publisherTimer.Stop()
		s.publisherTimer = nil
	}
	viewers := make([]*webrtc.PeerConnection, 0, len(s.viewers))
	for id, viewer := range s.viewers {
		viewers = append(viewers, viewer)
		delete(s.viewers, id)
	}
	s.mu.Unlock()
	if publisher != nil {
		_ = publisher.Close()
	}
	for _, viewer := range viewers {
		_ = viewer.Close()
	}
}

func (s *sfu) keyframeLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-ticker.C:
			s.requestKeyframes()
		}
	}
}

func (s *sfu) requestKeyframes() {
	s.mu.RLock()
	publisher := s.publisher
	viewers := len(s.viewers)
	ssrcs := make([]webrtc.SSRC, 0, len(s.tracks))
	if publisher != nil && viewers > 0 {
		for _, track := range s.tracks {
			if track.kind == webrtc.RTPCodecTypeVideo.String() {
				ssrcs = append(ssrcs, track.ssrc)
			}
		}
	}
	s.mu.RUnlock()
	for _, ssrc := range ssrcs {
		_ = publisher.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)}})
	}
}

func closePeerAfterDisconnect(pc *webrtc.PeerConnection, close func()) {
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			go close()
		case webrtc.PeerConnectionStateDisconnected:
			go func() {
				timer := time.NewTimer(8 * time.Second)
				defer timer.Stop()
				<-timer.C
				if pc.ConnectionState() == webrtc.PeerConnectionStateDisconnected {
					close()
				}
			}()
		}
	})
}

func drainReceiver(receiver *webrtc.RTPReceiver) {
	buffer := make([]byte, 1500)
	for {
		if _, _, err := receiver.Read(buffer); err != nil {
			return
		}
	}
}
func drainSender(sender *webrtc.RTPSender) {
	buffer := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buffer); err != nil {
			return
		}
	}
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
