package sfu

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

type publisherConn struct {
	id          string
	session     *Session
	participant *Participant
	pc          *webrtc.PeerConnection
	claims      mediacontracts.TokenClaims
	usage       *usage.Connection

	mu      sync.Mutex
	remote  string
	patchMu sync.Mutex
	tracks  map[string]*Track
	closed  atomic.Bool
}

func (c *publisherConn) peer() *webrtc.PeerConnection      { return c.pc }
func (c *publisherConn) patching() *sync.Mutex             { return &c.patchMu }
func (c *publisherConn) token() mediacontracts.TokenClaims { return c.claims }
func (c *publisherConn) remoteSDP() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remote
}
func (c *publisherConn) setRemoteSDP(value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remote = value
}

func defaultSource(kind string) string {
	if kind == "audio" {
		return "microphone"
	}
	return "camera"
}

func (s *Session) serveWHIP(w http.ResponseWriter, r *http.Request) {
	if s.opts.Role != mediacontracts.RoleSFU {
		WriteError(w, httpError(http.StatusNotFound, "not_found", "this node does not accept publishers"))
		return
	}
	verifiedMS := s.opts.Usage.NowMS()
	claims, err := s.authorize(r, "whip")
	if err != nil {
		WriteError(w, err)
		return
	}
	sources, err := parseTrackSources(r.Header.Get(mediacontracts.TrackSourcesHeader))
	if err != nil {
		WriteError(w, err)
		return
	}
	offer, err := readSDP(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	sections, err := parseOffer(offer)
	if err != nil {
		WriteError(w, err)
		return
	}
	e2ee := s.isE2EE()
	type planned struct {
		section offerSection
		source  string
	}
	if mediaSections(sections) > maxPublishSections {
		WriteError(w, httpError(http.StatusBadRequest, "invalid_request", "a WHIP resource publishes one group of at most 4 tracks"))
		return
	}
	tooMany := func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		existing := s.identities[claims.Identity]
		return existing != nil && !existing.remote && len(existing.whips) >= maxPublishResources
	}()
	if tooMany {
		WriteError(w, httpError(http.StatusBadRequest, "invalid_request", "too many WHIP resources for this participant"))
		return
	}
	plan := make([]planned, 0, len(sections))
	for _, section := range sections {
		if section.kind != "audio" && section.kind != "video" {
			continue
		}
		source := sources[section.mid]
		if source == "" {
			source = defaultSource(section.kind)
		}
		if source == mediacontracts.TrackSourceMusic && section.kind != "audio" {
			WriteError(w, httpError(http.StatusBadRequest, "invalid_request", "music is an audio source"))
			return
		}
		if err := mediacontracts.AuthorizeToken(claims, mediacontracts.RolePublish, source); err != nil {
			WriteError(w, httpError(http.StatusForbidden, mediacontracts.TokenErrorCode(err), source))
			return
		}
		if !validRIDs(section.sendRIDs) {
			WriteError(w, httpError(http.StatusBadRequest, "invalid_request", "simulcast needs at most 3 unique alphanumeric RIDs"))
			return
		}
		if e2ee && !e2eeCodecAllowed(section) {
			WriteError(w, httpError(http.StatusUnsupportedMediaType, "codec_not_allowed", "e2ee sessions carry Opus and VP8 only"))
			return
		}
		plan = append(plan, planned{section: section, source: source})
	}
	if len(plan) == 0 {
		WriteError(w, httpError(http.StatusBadRequest, "invalid_request", "the offer publishes no media"))
		return
	}
	if s.opts.Admission != nil {
		if err := s.opts.Admission("whip"); err != nil {
			WriteError(w, err)
			return
		}
	}
	api, err := s.opts.Factory.Publisher(e2ee)
	if err != nil {
		WriteError(w, err)
		return
	}
	pc, err := api.NewPeerConnection(s.peerConfiguration())
	if err != nil {
		WriteError(w, err)
		return
	}
	nonce, identity, kind := claims.Nonce, claims.Identity, mediacontracts.ParticipantKindOf(claims)
	admission := s.admitted(r, claims, verifiedMS)
	conn := &publisherConn{id: newID("r_"), session: s, pc: pc, claims: claims, tracks: map[string]*Track{}, remote: offer,
		usage: s.opts.Usage.Open(usage.ConnectionInfo{ID: admission.id, Proof: admission.proof, TokenNonce: &nonce, Identity: &identity,
			Kind: &kind, ReceiptKey: admission.receiptKey(), Direction: usage.DirectionIngress, Transport: usage.TransportWHIP, Detached: true})}
	conn.usage.SetClassify(conn.mediaClass)
	conn.usage.FollowICE(pc, nil)
	pc.OnTrack(conn.onTrack)
	s.watchPeer(pc, "whip", func(webrtc.PeerConnectionState) { conn.lost() }, conn.abort)
	plannedSources := map[string]string{}
	for _, item := range plan {
		plannedSources[item.section.mid] = item.source
	}
	local, err := answer(pc, offer, func() error { return applySourceCodecs(pc, sections, plannedSources) })
	if err != nil {
		conn.usage.Close()
		_ = pc.Close()
		WriteError(w, err)
		return
	}
	codecs := map[string]string{}
	for _, transceiver := range pc.GetTransceivers() {
		if receiver := transceiver.Receiver(); receiver != nil {
			if parameters := receiver.GetParameters(); len(parameters.Codecs) > 0 {
				codecs[transceiver.Mid()] = codecName(parameters.Codecs[0].MimeType)
			}
		}
	}
	refusal := func() *HTTPError {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed || !s.accepting {
			return httpError(http.StatusServiceUnavailable, "draining", "")
		}
		participant, _ := s.participantLocked(claims)
		if len(participant.whips) >= maxPublishResources {
			return httpError(http.StatusBadRequest, "invalid_request", "too many WHIP resources for this participant")
		}
		conn.participant = participant
		participant.whips[conn.id] = conn
		s.publishers[conn.id] = conn
		for _, item := range plan {
			track := s.detachedTrackLocked(participant, item.source, item.section.kind)
			reattached := track != nil
			if !reattached {
				track = s.newLocalTrackLocked(participant, item.section, item.source, codecs[item.section.mid])
			}
			if track.attach(conn) {
				s.broadcastLocked(track.stateEvent(), nil)
			}
			conn.tracks[item.section.mid] = track
			if !reattached {
				s.publishLocked(track, participant)
			}
		}
		s.grantLocked(participant, claims)
		s.pruneLostLocked(participant)
		s.endGraceLocked(participant)
		return nil
	}()
	if refusal != nil {
		conn.usage.Close()
		_ = pc.Close()
		WriteError(w, refusal)
		return
	}
	setReceiptConnection(w, admission)
	writeSDP(w, "/v1/rtc/"+s.opts.SessionID+"/whip/"+conn.id, local)
}

func (s *Session) newLocalTrackLocked(participant *Participant, section offerSection, source, codec string) *Track {
	kind := webrtc.RTPCodecTypeVideo
	if section.kind == "audio" {
		kind = webrtc.RTPCodecTypeAudio
	}
	if codec == "" {
		codec = "vp8"
		if kind == webrtc.RTPCodecTypeAudio {
			codec = "opus"
		}
	}
	info := mediacontracts.Track{TrackID: newID("t_"), ParticipantID: participant.info.ParticipantID, Kind: section.kind,
		Source: source, Codec: codec, Layers: []mediacontracts.SimulcastLayer{}}
	track := &Track{session: s, identity: participant.info.Identity, kind: kind, info: info}
	if kind == webrtc.RTPCodecTypeVideo && len(section.sendRIDs) > 0 {
		for position, rid := range section.sendRIDs {
			layer := track.Layer(rid)
			layer.spatial = spatialOfRID(rid, position)
			size := section.ridSize[rid]
			track.info.Layers = append(track.info.Layers, mediacontracts.SimulcastLayer{RID: rid, Spatial: int64(layer.spatial),
				Width: size[0], Height: size[1]})
		}
	} else {
		track.Layer("")
	}
	return track
}

func (s *Session) detachedTrackLocked(participant *Participant, source, kind string) *Track {
	for _, track := range s.tracks {
		if track.info.ParticipantID != participant.info.ParticipantID || track.info.Source != source || track.info.Kind != kind {
			continue
		}
		detached := track.waitsForPublisher()
		if detached {
			return track
		}
	}
	return nil
}

func (s *Session) endGraceLocked(participant *Participant) {
	if participant.graceEnd == nil {
		return
	}
	for _, track := range s.tracks {
		if track.info.ParticipantID != participant.info.ParticipantID {
			continue
		}
		detached := track.ownedBy(nil)
		if detached {
			return
		}
	}
	participant.graceEnd.Stop()
	participant.graceEnd = nil
}

func (c *publisherConn) onTrack(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	defer c.session.guard("whip track", c.abort)
	mid := ""
	for _, transceiver := range c.pc.GetTransceivers() {
		if transceiver.Receiver() == receiver {
			mid = transceiver.Mid()
			break
		}
	}
	track := func() *Track {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.tracks[mid]
	}()
	if track == nil {
		go c.drain(remote)
		return
	}
	layer := track.Layer(remote.RID())
	ssrc := uint32(remote.SSRC())
	pc := c.pc
	source := NewSource(func() {
		_ = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
	})
	ids := extensionIDs(receiver.GetParameters().HeaderExtensions)
	mime := lowerMime(remote.Codec().MimeType)
	session := c.session
	rid := remote.RID()
	var stream usage.Stream
	go func() {
		defer session.guard("whip rtp", c.abort)
		for {
			packet, attributes, err := remote.ReadRTP()
			if err != nil {
				return
			}
			if c.closed.Load() {
				return
			}
			c.usage.Received(&stream, packet, attributes)
			session.packets.Add(1)
			layer.Feed(source, packet, ids, mime)
		}
	}()
	go func() {
		defer session.guard("whip rtcp", c.abort)
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
	layer.RequestKeyframe()
}

func extensionIDs(extensions []webrtc.RTPHeaderExtensionParameter) ExtensionIDs {
	var ids ExtensionIDs
	for _, extension := range extensions {
		switch extension.URI {
		case DependencyDescriptorURI:
			ids.DependencyDescriptor = uint8(extension.ID)
		case AudioLevelURI:
			ids.AudioLevel = uint8(extension.ID)
		}
	}
	return ids
}

func (c *publisherConn) drain(remote *webrtc.TrackRemote) {
	defer c.session.guard("whip drain", c.abort)
	buffer := make([]byte, 1500)
	for {
		if _, _, err := remote.Read(buffer); err != nil {
			return
		}
	}
}

func (c *publisherConn) close(_ string, leaveReason string) {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	_ = c.pc.Close()
	c.usage.Close()
	s := c.session
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.publishers, c.id)
	participant := c.participant
	if participant == nil {
		return
	}
	delete(participant.whips, c.id)
	delete(participant.grants, c.claims.Nonce)
	for _, track := range c.tracks {
		if track.ownedBy(c) {
			s.unpublishLocked(track)
		}
	}
	if participant.whep == nil && len(participant.whips) == 0 {
		s.leaveLocked(participant, leaveReason)
	} else {
		s.refreshParticipantLocked(participant)
	}
}

func (c *publisherConn) abort() { c.lost() }

func (c *publisherConn) lost() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	_ = c.pc.Close()
	c.usage.Close()
	s := c.session
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.publishers, c.id)
	participant := c.participant
	if participant == nil {
		return
	}
	delete(participant.whips, c.id)
	now := time.Now()
	for _, track := range c.tracks {
		track.detach(c, now)
	}
	if s.lostWHIPs == nil {
		s.lostWHIPs = map[string]*publisherConn{}
	}
	s.lostWHIPs[c.id] = c
	if participant.graceEnd != nil {
		participant.graceEnd.Stop()
	}
	participant.graceEnd = time.AfterFunc(s.opts.PublisherGrace, func() { s.graceExpired(participant) })
	until := now.Add(s.opts.PublisherGrace).UnixMilli()
	for _, track := range s.tracks {
		if track.info.ParticipantID == participant.info.ParticipantID && track.waitsForPublisher() && track.detachedUntil(until) {
			s.broadcastLocked(track.stateEvent(), nil)
		}
	}
}

func (s *Session) graceExpired(participant *Participant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	participant.graceEnd = nil
	for _, track := range s.tracks {
		if track.info.ParticipantID != participant.info.ParticipantID {
			continue
		}
		detached := track.waitsForPublisher()
		if detached {
			s.unpublishLocked(track)
		}
	}
	s.pruneLostLocked(participant)
	for nonce := range participant.grants {
		if !s.grantLiveLocked(participant, nonce) {
			delete(participant.grants, nonce)
		}
	}
	if participant.whep == nil && len(participant.whips) == 0 {
		s.leaveLocked(participant, "timeout")
	} else {
		s.refreshParticipantLocked(participant)
	}
}

func (s *Session) grantLiveLocked(participant *Participant, nonce string) bool {
	if participant.whep != nil && participant.whep.claims.Nonce == nonce {
		return true
	}
	for _, conn := range participant.whips {
		if conn.claims.Nonce == nonce {
			return true
		}
	}
	return false
}

func (s *Session) muteTrackLocked(track *Track, muted, server bool) {
	changed := track.setMuted(muted, server)
	if changed {
		s.broadcastLocked(mediacontracts.TrackMutedEvent{EventHeader: header("track_muted"), TrackID: track.info.TrackID,
			ParticipantID: track.info.ParticipantID, Muted: muted}, nil)
	}
}

func isNodeIdentity(identity string) bool {
	return strings.HasPrefix(identity, mediacontracts.NodeIdentityPrefix)
}
