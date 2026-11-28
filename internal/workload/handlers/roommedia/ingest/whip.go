package ingest

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/ratelimit"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

const (
	maxOfferBytes  = 256 << 10
	maxReceiptBody = 2 << 10
)

type whipServer struct {
	role *Role
	api  *webrtc.API

	mu        sync.Mutex
	resources map[string]*whipResource
	limits    broadcast.RequestLimiter
}

type whipResource struct {
	id        string
	pc        *webrtc.PeerConnection
	publisher *Publisher
	ended     sync.Once
}

func newWHIPServer(role *Role) (*whipServer, error) {
	engine := &webrtc.MediaEngine{}
	feedback := []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}}
	for i, profile := range []string{"42e01f", "42001f", "4d001f", "640032", "64001f"} {
		if err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profile,
			RTCPFeedback: feedback}, PayloadType: webrtc.PayloadType(102 + 2*i)}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}
	if err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"},
		PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(engine, registry); err != nil {
		return nil, err
	}
	settings := webrtc.SettingEngine{}
	config := role.env.Config
	if config.PublicIP != "" {
		if err := settings.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{External: []string{config.PublicIP},
			AsCandidateType: webrtc.ICECandidateTypeHost, Mode: webrtc.ICEAddressRewriteReplace}); err != nil {
			return nil, err
		}
	}
	if config.UDPPortMin != 0 {
		if err := settings.SetEphemeralUDPPortRange(config.UDPPortMin, config.UDPPortMax); err != nil {
			return nil, err
		}
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry), webrtc.WithSettingEngine(settings))
	return &whipServer{role: role, api: api, resources: map[string]*whipResource{}}, nil
}

func (s *whipServer) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/v1/ingest/"+s.role.node.SessionID+"/ice" {
		s.ice(response, request)
		return
	}
	prefix := "/v1/ingest/" + s.role.node.SessionID + "/whip"
	rest := strings.TrimPrefix(request.URL.Path, prefix)
	switch {
	case rest == "" && request.Method == http.MethodPost:
		s.create(response, request)
	case strings.HasPrefix(rest, "/") && len(rest) > 1:
		s.resource(response, request, rest[1:])
	default:
		broadcast.WriteError(response, http.StatusNotFound, "not_found", "")
	}
}

func (s *whipServer) credential(request *http.Request, offer *sdp.SessionDescription) (keyID, keySHA string, nonce, identity *string, status int, code string) {
	value := broadcast.Bearer(request)
	if strings.HasPrefix(value, "bsk_") {
		digest := keyDigest(value)
		if digest == "" {
			return "", "", nil, nil, http.StatusUnauthorized, mc.ErrTokenMalformed
		}
		entry, listed := s.role.findKey(digest)
		if !listed {
			return "", "", nil, nil, http.StatusUnauthorized, mc.ErrTokenRevoked
		}
		if entry.Channel {
			return "", "", nil, nil, http.StatusUnauthorized, mc.ErrGrantRequired
		}
		return entry.KeyID, digest, nil, nil, 0, ""
	}
	claims, err := s.role.node.VerifyToken(value, time.Now())
	if err != nil {
		code := mc.TokenErrorCode(err)
		if code == "" {
			code = mc.ErrTokenMalformed
		}
		return "", "", nil, nil, http.StatusUnauthorized, code
	}
	if len(offer.MediaDescriptions) == 0 {
		if err := mc.AuthorizeToken(claims, mc.RolePublish, ""); err != nil {
			return "", "", nil, nil, http.StatusForbidden, mc.TokenErrorCode(err)
		}
	}
	sources := trackSources(request.Header.Get(mc.TrackSourcesHeader))
	for _, media := range offer.MediaDescriptions {
		mid, _ := media.Attribute("mid")
		source := sources[mid]
		if source == "" {
			source = map[string]string{"audio": "microphone", "video": "camera"}[media.MediaName.Media]
		}
		if err := mc.AuthorizeToken(claims, mc.RolePublish, source); err != nil {
			return "", "", nil, nil, http.StatusForbidden, mc.TokenErrorCode(err)
		}
	}
	return "", "", broadcast.StringPtr(claims.Nonce), broadcast.StringPtr(claims.Identity), 0, ""
}

func (s *whipServer) ice(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "GET only")
		return
	}
	now := time.Now()
	if !s.limits.Allow("ip:"+ratelimit.ClientIP(request), now) {
		broadcast.WriteRateLimited(response)
		return
	}
	if code := s.role.accepting(); code != "" {
		broadcast.WriteError(response, http.StatusServiceUnavailable, code, "")
		return
	}
	keyID, _, nonce, _, status, code := s.credential(request, &sdp.SessionDescription{})
	if status != 0 {
		broadcast.WriteError(response, status, code, "")
		return
	}
	holder := keyID
	if nonce != nil {
		holder = *nonce
	}
	if !s.limits.Allow("holder:"+holder, now) {
		broadcast.WriteRateLimited(response)
		return
	}
	encoded, err := mc.MarshalWire(broadcast.ICEServers(s.role.env.Config, holder, now))
	if err != nil {
		broadcast.WriteError(response, http.StatusInternalServerError, "invalid_request", "")
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(encoded)
}

func musicVideo(header string, offer *sdp.SessionDescription) bool {
	sources := trackSources(header)
	for _, media := range offer.MediaDescriptions {
		mid, _ := media.Attribute("mid")
		if sources[mid] == mc.TrackSourceMusic && media.MediaName.Media != "audio" {
			return true
		}
	}
	return false
}

func trackSources(header string) map[string]string {
	sources := map[string]string{}
	for _, item := range strings.Split(header, ",") {
		if mid, source, ok := strings.Cut(strings.TrimSpace(item), "="); ok {
			sources[strings.TrimSpace(mid)] = strings.TrimSpace(source)
		}
	}
	return sources
}

func (s *whipServer) create(response http.ResponseWriter, request *http.Request) {
	if !s.role.allows(ProtocolWHIP) {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "WHIP is not enabled for this session")
		return
	}
	if reason := s.role.accepting(); reason != "" {
		broadcast.WriteError(response, http.StatusServiceUnavailable, reason, "")
		return
	}
	if !strings.HasPrefix(request.Header.Get("Content-Type"), "application/sdp") {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "application/sdp required")
		return
	}
	grant, granted, problem := grantParameter(request)
	if problem != "" {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", problem)
		return
	}
	var grantKey mc.TopologyStreamKey
	if granted {
		key, status, code := s.spendGrant(grant)
		if status != 0 {
			if status == http.StatusTooManyRequests {
				broadcast.WriteRateLimited(response)
			} else {
				broadcast.WriteError(response, status, code, "")
			}
			return
		}
		grantKey = key
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, maxOfferBytes+1))
	if err != nil || len(raw) > maxOfferBytes {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "offer too large")
		return
	}
	var parsed sdp.SessionDescription
	if err := parsed.UnmarshalString(string(raw)); err != nil {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "malformed SDP")
		return
	}
	if musicVideo(request.Header.Get(mc.TrackSourcesHeader), &parsed) {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "music is an audio source")
		return
	}
	var keyID, keySHA string
	var nonce, identity *string
	verifiedMS := s.role.env.Usage.Reporter().NowMS()
	if granted {
		keyID, keySHA = grantKey.KeyID, grantKey.SHA256
	} else {
		var status int
		var code string
		keyID, keySHA, nonce, identity, status, code = s.credential(request, &parsed)
		if status != 0 {
			broadcast.WriteError(response, status, code, "")
			return
		}
	}
	admission := s.admitted(request, nonce, identity, verifiedMS)
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		broadcast.WriteError(response, http.StatusServiceUnavailable, "session_full", "no peer connection")
		return
	}
	resource := &whipResource{id: "w_" + randomResourceID(), pc: pc}
	attached := make(chan *Publisher, 1)
	var ssrcMu sync.Mutex
	var videoSSRC webrtc.SSRC
	keyNeeded := func() {
		ssrcMu.Lock()
		ssrc := videoSSRC
		ssrcMu.Unlock()
		if ssrc != 0 {
			_ = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)}})
		}
	}
	indexByKind := map[string]int{}
	started := time.Now()
	var publisher *Publisher
	var publisherMu sync.Mutex
	end := func(err error) {
		resource.ended.Do(func() {
			_ = pc.Close()
			s.mu.Lock()
			delete(s.resources, resource.id)
			s.mu.Unlock()
			publisherMu.Lock()
			current := publisher
			publisherMu.Unlock()
			if current != nil {
				s.role.endPublisher(current, err)
			}
		})
	}
	pc.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		go drainRTCP(receiver)
		current := <-attached
		attached <- current
		switch remote.Kind() {
		case webrtc.RTPCodecTypeVideo:
			index, declared := indexByKind["video"]
			if !declared || !strings.EqualFold(remote.Codec().MimeType, webrtc.MimeTypeH264) {
				return
			}
			ssrcMu.Lock()
			videoSSRC = remote.SSRC()
			ssrcMu.Unlock()
			keyNeeded()
			s.readVideo(current, remote, index, started)
		case webrtc.RTPCodecTypeAudio:
			if index, declared := indexByKind["audio"]; declared {
				s.readAudio(current, remote, index, started)
			}
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed:
			end(errors.New("ICE failed"))
		case webrtc.PeerConnectionStateClosed:
			end(nil)
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(raw)}); err != nil {
		end(nil)
		broadcast.WriteError(response, http.StatusUnsupportedMediaType, "codec_not_allowed", "the offer has no usable H.264/Opus media")
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		end(nil)
		broadcast.WriteError(response, http.StatusUnsupportedMediaType, "codec_not_allowed", "H.264 and Opus only")
		return
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		end(nil)
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "")
		return
	}
	var tracks []TrackInfo
	var accepted sdp.SessionDescription
	if err := accepted.UnmarshalString(pc.LocalDescription().SDP); err == nil {
		for _, media := range accepted.MediaDescriptions {
			kind := media.MediaName.Media
			if _, seen := indexByKind[kind]; seen || media.MediaName.Port.Value == 0 || (kind != "audio" && kind != "video") {
				continue
			}
			if _, inactive := media.Attribute("inactive"); inactive {
				continue
			}
			indexByKind[kind] = len(tracks)
			if kind == "video" {
				tracks = append(tracks, TrackInfo{Video: true, Codec: CodecH264})
			} else {
				tracks = append(tracks, TrackInfo{Codec: CodecOpus, OpusChannels: 2})
			}
		}
	}
	if len(tracks) == 0 {
		end(nil)
		broadcast.WriteError(response, http.StatusUnsupportedMediaType, "codec_not_allowed", "no H.264 video or Opus audio accepted")
		return
	}
	ladder := s.role.node.Topology().Ladder
	source := mc.IsSourceLadder(ladder)
	if source {
		if refusal := whipSourceRefusal(&parsed, &accepted); refusal != "" {
			end(nil)
			broadcast.WriteError(response, http.StatusUnsupportedMediaType, "codec_not_allowed", "source ladder: "+refusal)
			return
		}
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
	}
	publisherMu.Lock()
	var attachedNow bool
	publisher, attachedNow = s.role.startPublisher(ProtocolWHIP, keyID, keySHA, admission, func(string) { _ = pc.Close() }, tracks, keyNeeded)
	publisherMu.Unlock()
	resource.publisher = publisher
	publisher.meter.FollowICE(pc, nil)
	attached <- publisher
	if !attachedNow {
		end(nil)
		broadcast.WriteError(response, http.StatusUnauthorized, mc.ErrTokenRevoked, "")
		return
	}
	s.mu.Lock()
	s.resources[resource.id] = resource
	s.mu.Unlock()
	if source {
		go func() {
			ticker := time.NewTicker(time.Duration(ladder.GOPMS) * time.Millisecond)
			defer ticker.Stop()
			var sequence uint8
			for range ticker.C {
				state := pc.ConnectionState()
				if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
					return
				}
				ssrcMu.Lock()
				ssrc := videoSSRC
				ssrcMu.Unlock()
				if ssrc != 0 {
					sequence++
					_ = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)},
						&rtcp.FullIntraRequest{MediaSSRC: uint32(ssrc), FIR: []rtcp.FIREntry{{SSRC: uint32(ssrc), SequenceNumber: sequence}}}})
				}
			}
		}()
	}
	go func() {
		time.Sleep(30 * time.Second)
		if pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			end(errors.New("ICE timeout"))
		}
	}()
	response.Header().Set("Content-Type", "application/sdp")
	response.Header().Set("Location", s.resourceLocation(resource.id))
	response.Header().Set("ETag", `"`+resource.id+`"`)
	if admission.grant != nil {
		response.Header().Set(mc.ReceiptConnectionHeader, admission.id)
		response.Header().Set("Access-Control-Expose-Headers", response.Header().Get("Access-Control-Expose-Headers")+", "+mc.ReceiptConnectionHeader)
	}
	response.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(response, pc.LocalDescription().SDP)
}

func (s *whipServer) admitted(request *http.Request, nonce, identity *string, verifiedMS int64) admission {
	result := admission{id: usage.NewConnectionID(), nonce: nonce, identity: identity}
	if nonce == nil {
		return result
	}
	token := broadcast.Bearer(request)
	claims, _, _, err := mc.ParseTokenUnverified(token)
	if err != nil {
		return result
	}
	kind := mc.ParticipantKindOf(claims)
	result.kind = &kind
	settings, reporter := s.role.node.Topology().Usage, s.role.env.Usage.Reporter()
	if value := usage.RequestGrant(request); value != "" {
		if grant, ok := reporter.BindGrant(value, settings, claims, verifiedMS); ok {
			result.grant, result.proof = &grant, usage.GrantProof(value, verifiedMS)
			return result
		}
	}
	result.proof = reporter.TokenProof(token, settings, result.id, *nonce, verifiedMS)
	return result
}

func grantParameter(request *http.Request) (grant string, granted bool, problem string) {
	values := request.URL.Query()[mc.IngestGrantQueryParameter]
	switch {
	case len(values) > 1:
		return "", false, "grant given more than once"
	case len(values) == 0:
		return "", false, ""
	}
	return values[0], true, ""
}

func (s *whipServer) spendGrant(grant string) (mc.TopologyStreamKey, int, string) {
	key, err := s.role.node.VerifyIngestGrant(grant, time.Now(), s.role.grants)
	if err == nil {
		return key, 0, ""
	}
	switch code := mc.TokenErrorCode(err); code {
	case mc.ErrGrantRateLimited:
		return mc.TopologyStreamKey{}, http.StatusTooManyRequests, code
	case mc.ErrTokenRoleForbidden, mc.ErrTokenTrackForbidden:
		return mc.TopologyStreamKey{}, http.StatusForbidden, code
	case "":
		return mc.TopologyStreamKey{}, http.StatusUnauthorized, mc.ErrGrantInvalid
	default:
		return mc.TopologyStreamKey{}, http.StatusUnauthorized, code
	}
}

func (s *whipServer) resourceLocation(id string) string {
	path := "/v1/ingest/" + s.role.node.SessionID + "/whip/" + id
	if absolute := s.role.env.URL(path); absolute != "" {
		return absolute
	}
	return path
}

func (s *whipServer) resource(response http.ResponseWriter, request *http.Request, id string) {
	receipt := false
	if s.role.env.Usage.Reporter().TakesReceipts() {
		id, receipt = strings.CutSuffix(id, "/"+broadcast.ReceiptName)
	}
	s.mu.Lock()
	resource := s.resources[id]
	s.mu.Unlock()
	if resource == nil {
		broadcast.WriteError(response, http.StatusGone, "resource_gone", "")
		return
	}
	if receipt {
		s.receipt(response, request, resource)
		return
	}
	switch request.Method {
	case http.MethodDelete:
		_ = resource.pc.Close()
		response.WriteHeader(http.StatusOK)
	case http.MethodPatch:
		if !strings.HasPrefix(request.Header.Get("Content-Type"), "application/trickle-ice-sdpfrag") {
			broadcast.WriteError(response, http.StatusUnsupportedMediaType, "invalid_request", "")
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, 64<<10))
		if err != nil {
			broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "")
			return
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "a=candidate:") {
				_ = resource.pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: strings.TrimPrefix(line, "a=")})
			}
		}
		response.WriteHeader(http.StatusNoContent)
	default:
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "")
	}
}

func (s *whipServer) receipt(response http.ResponseWriter, request *http.Request, resource *whipResource) {
	if request.Method != http.MethodPost {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "POST only")
		return
	}
	if !s.limits.Allow("ip:"+ratelimit.ClientIP(request), time.Now()) || !s.limits.Allow("receipt:"+resource.id, time.Now()) {
		broadcast.WriteRateLimited(response)
		return
	}
	if !strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/json") {
		broadcast.WriteError(response, http.StatusUnsupportedMediaType, "invalid_request", "application/json required")
		return
	}
	var post mc.ReceiptPost
	body, err := io.ReadAll(io.LimitReader(request.Body, maxReceiptBody+1))
	if err != nil || len(body) > maxReceiptBody || mc.UnmarshalStrict(body, &post) != nil {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "invalid body")
		return
	}
	if err := resource.publisher.meter.Receipt(post.Receipt); err != nil {
		code := usage.ReceiptCode(err)
		switch code {
		case "receipt_unbound":
			broadcast.WriteError(response, http.StatusNotFound, code, "")
		case "receipt_connection":
			broadcast.WriteError(response, http.StatusConflict, code, "")
		case "rate_limited":
			broadcast.WriteRateLimited(response)
		default:
			broadcast.WriteError(response, http.StatusBadRequest, code, "")
		}
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *whipServer) closeAll() {
	s.mu.Lock()
	resources := make([]*whipResource, 0, len(s.resources))
	for _, resource := range s.resources {
		resources = append(resources, resource)
	}
	s.mu.Unlock()
	for _, resource := range resources {
		_ = resource.pc.Close()
	}
}

type clock struct {
	rate     int64
	anchored bool
	anchor   int64
	last     uint32
	extended int64
}

func (c *clock) at(timestamp uint32, started time.Time) int64 {
	if !c.anchored {
		c.anchored = true
		c.last = timestamp
		c.extended = int64(timestamp)
		c.anchor = time.Since(started).Microseconds()*90/1000 - c.extended*90000/c.rate
		return c.extended*90000/c.rate + c.anchor
	}
	c.extended += int64(int32(timestamp - c.last))
	c.last = timestamp
	return c.extended*90000/c.rate + c.anchor
}

func (s *whipServer) readVideo(publisher *Publisher, remote *webrtc.TrackRemote, index int, started time.Time) {
	builder := samplebuilder.New(512, &codecs.H264Packet{}, 90000)
	timeline := &clock{rate: 90000}
	var stream usage.Stream
	for {
		packet, attributes, err := remote.ReadRTP()
		if err != nil {
			return
		}
		media, overhead := stream.PacketBytes(packet, attributes)
		publisher.meter.AddRTP(0, media, overhead)
		builder.Push(packet)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			var au h264.AnnexB
			if err := au.Unmarshal(sample.Data); err != nil || len(au) == 0 {
				continue
			}
			at := timeline.at(sample.PacketTimestamp, started)
			s.role.write(publisher, Unit{Track: index, PTS: at, DTS: at, Key: h264.IsRandomAccess(au), AU: au})
		}
	}
}

func (s *whipServer) readAudio(publisher *Publisher, remote *webrtc.TrackRemote, index int, started time.Time) {
	timeline := &clock{rate: 48000}
	var stream usage.Stream
	for {
		packet, attributes, err := remote.ReadRTP()
		if err != nil {
			return
		}
		media, overhead := stream.PacketBytes(packet, attributes)
		publisher.meter.AddRTP(0, media, overhead)
		if len(packet.Payload) == 0 {
			continue
		}
		at := timeline.at(packet.Timestamp, started)
		s.role.write(publisher, Unit{Track: index, PTS: at, DTS: at, Key: true, AU: [][]byte{append([]byte(nil), packet.Payload...)}})
	}
}

func drainRTCP(receiver *webrtc.RTPReceiver) {
	buffer := make([]byte, 1500)
	for {
		if _, _, err := receiver.Read(buffer); err != nil {
			return
		}
	}
}

func randomResourceID() string {
	return strings.ToLower(broadcast.RandomID(20))
}
