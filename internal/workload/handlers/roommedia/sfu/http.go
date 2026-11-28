package sfu

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/ratelimit"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

const (
	maxSlots            = 32
	maxChildSlots       = 1024
	maxPublishSections  = 4
	maxPublishResources = 4
	maxRIDs             = 3
)

var ridPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)

func validRIDs(rids []string) bool {
	if len(rids) > maxRIDs {
		return false
	}
	seen := map[string]bool{}
	for _, rid := range rids {
		if !ridPattern.MatchString(rid) || seen[rid] {
			return false
		}
		seen[rid] = true
	}
	return true
}

func mediaSections(sections []offerSection) int {
	count := 0
	for _, section := range sections {
		if section.kind == "audio" || section.kind == "video" {
			count++
		}
	}
	return count
}

const (
	maxSDPBytes      = 128 << 10
	maxFragmentBytes = 32 << 10
	sdpContentType   = "application/sdp"
	fragContentType  = "application/trickle-ice-sdpfrag"
)

type HTTPError struct {
	Status int
	Code   string
	Detail string
}

func (e *HTTPError) Error() string { return e.Code + ": " + e.Detail }

func httpError(status int, code, detail string) *HTTPError {
	return &HTTPError{Status: status, Code: code, Detail: detail}
}

func SessionFull(detail string) error { return httpError(http.StatusConflict, "session_full", detail) }

func WriteError(w http.ResponseWriter, err error) {
	var value *HTTPError
	if !errors.As(err, &value) {
		value = httpError(http.StatusInternalServerError, "invalid_request", err.Error())
	}
	body := mediacontracts.APIError{Error: value.Code}
	if value.Detail != "" {
		detail := value.Detail
		if len(detail) > maxErrorDetail {
			detail = strings.ToValidUTF8(detail[:maxErrorDetail], "")
		}
		body.Detail = &detail
	}
	encoded, _ := mediacontracts.MarshalWire(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(value.Status)
	_, _ = w.Write(encoded)
}

func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-Match, "+mediacontracts.TrackSourcesHeader)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Expose-Headers", "Location, ETag, Link")
}

func (s *Session) Handler() http.Handler {
	mux := http.NewServeMux()
	base := "/v1/rtc/{session}/"
	mux.HandleFunc("POST "+base+"whip", s.serveWHIP)
	mux.HandleFunc("PATCH "+base+"whip/{resource}", s.servePatch)
	mux.HandleFunc("DELETE "+base+"whip/{resource}", s.serveDelete)
	mux.HandleFunc("POST "+base+"whip/{resource}/receipt", s.serveReceipt)
	mux.HandleFunc("POST "+base+"whep", s.serveWHEP)
	mux.HandleFunc("PATCH "+base+"whep/{resource}", s.servePatch)
	mux.HandleFunc("DELETE "+base+"whep/{resource}", s.serveDelete)
	mux.HandleFunc("POST "+base+"whep/{resource}/receipt", s.serveReceipt)
	mux.HandleFunc("GET "+base+"ice", s.serveICE)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/rtc/"+s.opts.SessionID+"/") {
			WriteError(w, httpError(http.StatusNotFound, "not_found", "unknown session"))
			return
		}
		if !s.limiter.Allow("ip:"+ratelimit.ClientIP(r), time.Now()) {
			WriteError(w, httpError(http.StatusTooManyRequests, "rate_limited", "too many requests from this address"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Session) authorize(r *http.Request, action string) (mediacontracts.TokenClaims, error) {
	view, verifier, accepting, membersOnly, children := func() (*mediacontracts.Topology, *mediacontracts.TokenVerifier, bool, bool, map[string]bool) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.view, s.verifier, s.accepting, s.membersOnly, s.children
	}()
	now := time.Now()
	if view == nil || verifier == nil {
		return mediacontracts.TokenClaims{}, httpError(http.StatusServiceUnavailable, "topology_expired", "the node has no topology yet")
	}
	if now.UnixMilli() > view.ExpiresAtMS {
		return mediacontracts.TokenClaims{}, httpError(http.StatusServiceUnavailable, "topology_expired", "")
	}
	if !accepting {
		return mediacontracts.TokenClaims{}, httpError(http.StatusServiceUnavailable, "draining", "re-join the directory")
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return mediacontracts.TokenClaims{}, httpError(http.StatusUnauthorized, mediacontracts.ErrTokenMalformed, "missing Bearer token")
	}
	claims, err := verifier.Verify(strings.TrimSpace(token), now.Unix())
	if err != nil {
		code := mediacontracts.TokenErrorCode(err)
		status := http.StatusUnauthorized
		if code == mediacontracts.ErrTokenRoleForbidden || code == mediacontracts.ErrTokenTrackForbidden {
			status = http.StatusForbidden
		}
		if code == "" {
			code = mediacontracts.ErrTokenMalformed
		}
		return mediacontracts.TokenClaims{}, httpError(status, code, "")
	}
	if !s.limiter.Allow("nonce:"+claims.Nonce, now) {
		return mediacontracts.TokenClaims{}, httpError(http.StatusTooManyRequests, "rate_limited", "too many requests for this token")
	}
	s.refundAddress(r)
	if child, isNode := strings.CutPrefix(claims.Identity, mediacontracts.NodeIdentityPrefix); isNode {
		if action == "whip" || !children[child] {
			return mediacontracts.TokenClaims{}, httpError(http.StatusForbidden, mediacontracts.ErrTokenRoleForbidden, "node identity is not a child of this node")
		}
		return claims, nil
	}
	if membersOnly && mediacontracts.MembersOnlyRefused(view.Protection.Mode, view.Protection.KeyProvider, claims.Identity) {
		return mediacontracts.TokenClaims{}, httpError(http.StatusForbidden, mediacontracts.E2EEMembersOnly, "")
	}
	need := mediacontracts.RoleSubscribe
	if action == "whip" {
		need = mediacontracts.RolePublish
	}
	if err := mediacontracts.AuthorizeToken(claims, need, ""); err != nil {
		return mediacontracts.TokenClaims{}, httpError(http.StatusForbidden, mediacontracts.TokenErrorCode(err), "")
	}
	return claims, nil
}

func (s *Session) refundAddress(r *http.Request) {
	s.limiter.Refund("ip:" + ratelimit.ClientIP(r))
}

func readSDP(r *http.Request) (string, error) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), sdpContentType) {
		return "", httpError(http.StatusBadRequest, "invalid_request", "Content-Type must be application/sdp")
	}
	payload, err := io.ReadAll(io.LimitReader(r.Body, maxSDPBytes+1))
	if err != nil || len(payload) == 0 || len(payload) > maxSDPBytes {
		return "", httpError(http.StatusBadRequest, "invalid_request", "invalid SDP offer")
	}
	return string(payload), nil
}

type offerSection struct {
	mid      string
	kind     string
	codecs   []string
	opusPT   uint8
	redPT    uint8
	fmtps    map[uint8]string
	sendRIDs []string
	ridSize  map[string][2]int64
	recvonly bool
}

func parseOffer(raw string) ([]offerSection, error) {
	var description sdp.SessionDescription
	if err := description.UnmarshalString(raw); err != nil {
		return nil, httpError(http.StatusBadRequest, "invalid_request", "malformed SDP")
	}
	sections := make([]offerSection, 0, len(description.MediaDescriptions))
	for _, media := range description.MediaDescriptions {
		section := offerSection{kind: media.MediaName.Media, ridSize: map[string][2]int64{}}
		for _, attribute := range media.Attributes {
			switch attribute.Key {
			case "mid":
				section.mid = attribute.Value
			case "rtpmap":
				if _, codec, ok := strings.Cut(attribute.Value, " "); ok {
					name, _, _ := strings.Cut(codec, "/")
					section.codecs = append(section.codecs, strings.ToLower(name))
				}
				if pt := opusPayloadType(attribute.Value); pt != 0 && section.opusPT == 0 {
					section.opusPT = pt
				}
				if pt := payloadTypeOf(attribute.Value, "red/"); pt != 0 && section.redPT == 0 {
					section.redPT = pt
				}
			case "fmtp":
				if pt, line, ok := strings.Cut(attribute.Value, " "); ok {
					if value, err := strconv.ParseUint(pt, 10, 7); err == nil {
						if section.fmtps == nil {
							section.fmtps = map[uint8]string{}
						}
						section.fmtps[uint8(value)] = line
					}
				}
			case "rid":
				fields := strings.Fields(attribute.Value)
				if len(fields) >= 2 && fields[1] == "send" {
					section.sendRIDs = append(section.sendRIDs, fields[0])
					if len(fields) >= 3 {
						section.ridSize[fields[0]] = ridRestrictions(fields[2])
					}
				}
			case "recvonly":
				section.recvonly = true
			}
		}
		sections = append(sections, section)
	}
	return sections, nil
}

func ridRestrictions(value string) [2]int64 {
	var size [2]int64
	for _, part := range strings.Split(value, ";") {
		key, number, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		var parsed int64
		if _, err := fmt.Sscan(number, &parsed); err != nil {
			continue
		}
		switch key {
		case "max-width":
			size[0] = parsed
		case "max-height":
			size[1] = parsed
		}
	}
	return size
}

func parseTrackSources(value string) (map[string]string, error) {
	sources := map[string]string{}
	if strings.TrimSpace(value) == "" {
		return sources, nil
	}
	for _, item := range strings.Split(value, ",") {
		mid, source, ok := strings.Cut(strings.TrimSpace(item), "=")
		mid, source = strings.TrimSpace(mid), strings.TrimSpace(source)
		if !ok || mid == "" || !validSource(source) {
			return nil, httpError(http.StatusBadRequest, "invalid_request", "invalid "+mediacontracts.TrackSourcesHeader)
		}
		sources[mid] = source
	}
	return sources, nil
}

func validSource(source string) bool { return slices.Contains(mediacontracts.TrackSources, source) }

func e2eeCodecAllowed(section offerSection) bool {
	for _, codec := range section.codecs {
		switch {
		case section.kind == "audio" && codec == "opus":
			return true
		case section.kind == "video" && codec == "vp8":
			return true
		}
	}
	return false
}

func (s *Session) peerConfiguration() webrtc.Configuration {
	servers := make([]webrtc.ICEServer, 0, len(s.opts.ICE.Servers))
	for _, server := range s.opts.ICE.Servers {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(server)), "stun:") {
			servers = append(servers, webrtc.ICEServer{URLs: []string{server}})
		}
	}
	return webrtc.Configuration{ICEServers: servers, BundlePolicy: webrtc.BundlePolicyMaxBundle,
		RTCPMuxPolicy: webrtc.RTCPMuxPolicyRequire}
}

func answer(pc *webrtc.PeerConnection, offer string, before func() error) (string, error) {
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return "", httpError(http.StatusBadRequest, "invalid_request", "offer refused: "+err.Error())
	}
	if before != nil {
		if err := before(); err != nil {
			return "", err
		}
	}
	description, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", httpError(http.StatusBadRequest, "invalid_request", "answer failed: "+err.Error())
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(description); err != nil {
		return "", httpError(http.StatusBadRequest, "invalid_request", "answer failed: "+err.Error())
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
	}
	return pc.LocalDescription().SDP, nil
}

func writeSDP(w http.ResponseWriter, location, answer string) {
	w.Header().Set("Location", location)
	w.Header().Set("ETag", resourceETag(location[strings.LastIndex(location, "/")+1:]))
	w.Header().Set("Content-Type", sdpContentType)
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, answer)
}

func (s *Session) serveICE(w http.ResponseWriter, r *http.Request) {
	claims, err := s.authorize(r, "ice")
	if err != nil {
		WriteError(w, err)
		return
	}
	ttl := s.opts.ICE.TURNTTL
	if ttl <= 0 {
		ttl = 3 * time.Hour
	}
	expires := time.Now().Add(ttl)
	prefix := strings.NewReplacer(":", "_", " ", "_").Replace(strings.TrimSpace(s.opts.ICE.UsernamePrefix))
	if prefix == "" {
		prefix = "beam"
	}
	servers := make([]mediacontracts.ICEServer, 0, len(s.opts.ICE.Servers))
	for _, server := range s.opts.ICE.Servers {
		entry := mediacontracts.ICEServer{URLs: []string{server}}
		lower := strings.ToLower(strings.TrimSpace(server))
		if secret := strings.TrimSpace(s.opts.ICE.TURNSecret); secret != "" && (strings.HasPrefix(lower, "turn:") || strings.HasPrefix(lower, "turns:")) {
			username := fmt.Sprintf("%d:%s-%s", expires.Unix(), prefix, claims.Nonce)
			digest := hmac.New(sha1.New, []byte(secret))
			_, _ = digest.Write([]byte(username))
			credential := base64.StdEncoding.EncodeToString(digest.Sum(nil))
			entry.Username, entry.Credential = &username, &credential
		}
		servers = append(servers, entry)
		if len(servers) == 8 {
			break
		}
	}
	encoded, _ := mediacontracts.MarshalWire(mediacontracts.ICEResponse{ICEServers: servers, ExpiresAtMS: expires.UnixMilli()})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

type resource interface {
	peer() *webrtc.PeerConnection
	patching() *sync.Mutex
	remoteSDP() string
	setRemoteSDP(string)
	token() mediacontracts.TokenClaims
	close(connectionReason, leaveReason string)
	metered() *usage.Connection
}

func (s *Session) lookup(kind, id string) resource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if kind == "whip" {
		if conn := s.publishers[id]; conn != nil {
			return conn
		}
		return nil
	}
	if conn := s.subscribers[id]; conn != nil {
		return conn
	}
	return nil
}

func resourceKind(r *http.Request) string {
	if strings.Contains(r.URL.Path, "/whip/") {
		return "whip"
	}
	return "whep"
}

func (s *Session) requireOwner(r *http.Request, target resource) error {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return httpError(http.StatusUnauthorized, mediacontracts.ErrTokenMalformed, "missing Bearer token")
	}
	verifier := func() *mediacontracts.TokenVerifier {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.verifier
	}()
	if verifier == nil {
		return httpError(http.StatusServiceUnavailable, "topology_expired", "")
	}
	claims, err := verifier.Verify(strings.TrimSpace(token), time.Now().Unix())
	if err != nil {
		parsed, _, _, parseErr := mediacontracts.ParseTokenUnverified(strings.TrimSpace(token))
		if parseErr != nil || mediacontracts.TokenErrorCode(err) != mediacontracts.ErrTokenExpired || parsed.Nonce != target.token().Nonce {
			return httpError(http.StatusUnauthorized, mediacontracts.TokenErrorCode(err), "")
		}
		claims = parsed
	}
	if claims.Identity != target.token().Identity {
		return httpError(http.StatusForbidden, mediacontracts.ErrTokenRoleForbidden, "not the owner of this resource")
	}
	if !s.limiter.Allow("nonce:"+claims.Nonce, time.Now()) {
		return httpError(http.StatusTooManyRequests, "rate_limited", "too many requests for this token")
	}
	s.refundAddress(r)
	return nil
}

func (s *Session) serveDelete(w http.ResponseWriter, r *http.Request) {
	kind, id := resourceKind(r), r.PathValue("resource")
	target := s.lookup(kind, id)
	if target == nil && kind == "whip" {
		if lost := s.lostWHIP(id); lost != nil {
			if err := s.requireOwner(r, lost); err != nil {
				WriteError(w, err)
				return
			}
			if s.deleteLost(lost) {
				if status := s.goneStatus(id); status == http.StatusGone {
					WriteError(w, httpError(status, "resource_gone", ""))
					return
				}
				w.WriteHeader(http.StatusOK)
				return
			}
		}
	}
	if target == nil {
		WriteError(w, httpError(s.goneStatus(id), "resource_gone", ""))
		return
	}
	if err := s.requireOwner(r, target); err != nil {
		WriteError(w, err)
		return
	}
	target.close("", "left")
	w.WriteHeader(http.StatusOK)
}

func resourceETag(id string) string {
	return `"` + id + `"`
}

func ifMatchAny(value string) bool {
	return value == "*" || value == `"*"`
}

func (s *Session) servePatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("resource")
	target := s.lookup(resourceKind(r), id)
	if target == nil {
		WriteError(w, httpError(s.goneStatus(id), "resource_gone", ""))
		return
	}
	if err := s.requireOwner(r, target); err != nil {
		WriteError(w, err)
		return
	}
	lock := target.patching()
	lock.Lock()
	defer lock.Unlock()
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), fragContentType) {
		WriteError(w, httpError(http.StatusUnsupportedMediaType, "invalid_request", "Content-Type must be "+fragContentType))
		return
	}
	payload, err := io.ReadAll(io.LimitReader(r.Body, maxFragmentBytes+1))
	if err != nil || len(payload) > maxFragmentBytes {
		WriteError(w, httpError(http.StatusBadRequest, "invalid_request", "invalid fragment"))
		return
	}
	fragment := parseFragment(string(payload))
	pc := target.peer()
	current := target.remoteSDP()
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	restart := ifMatch == "" || ifMatchAny(ifMatch)
	credentials := fragment.ufrag != "" && fragment.pwd != ""
	running := credentials && strings.Contains(current, "a=ice-ufrag:"+fragment.ufrag+"\r\n")
	if credentials && !running && !restart {
		WriteError(w, httpError(http.StatusUnprocessableEntity, "invalid_request",
			"the fragment's ICE credentials are not the resource's ICE session: send the restart with If-Match \"*\""))
		return
	}
	if credentials && running && ifMatchAny(ifMatch) {
		for _, candidate := range fragment.candidates {
			_ = pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidate})
		}
		local := pc.LocalDescription()
		if local == nil {
			WriteError(w, httpError(http.StatusConflict, "invalid_request", "no local description"))
			return
		}
		w.Header().Set("Content-Type", fragContentType)
		w.Header().Set("ETag", resourceETag(id))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, localFragment(local.SDP))
		return
	}
	if credentials && !running {
		restarted := replaceICECredentials(current, fragment.ufrag, fragment.pwd)
		local, err := answer(pc, restarted, nil)
		if err != nil {
			WriteError(w, err)
			return
		}
		target.setRemoteSDP(restarted)
		for _, candidate := range fragment.candidates {
			_ = pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidate})
		}
		w.Header().Set("Content-Type", fragContentType)
		w.Header().Set("ETag", resourceETag(id))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, localFragment(local))
		return
	}
	for _, candidate := range fragment.candidates {
		if err := pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidate}); err != nil {
			WriteError(w, httpError(http.StatusBadRequest, "invalid_request", "invalid candidate"))
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

type sdpFragment struct {
	ufrag, pwd string
	candidates []string
}

func parseFragment(value string) sdpFragment {
	var fragment sdpFragment
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "a=ice-ufrag:"):
			fragment.ufrag = strings.TrimPrefix(line, "a=ice-ufrag:")
		case strings.HasPrefix(line, "a=ice-pwd:"):
			fragment.pwd = strings.TrimPrefix(line, "a=ice-pwd:")
		case strings.HasPrefix(line, "a=candidate:"):
			fragment.candidates = append(fragment.candidates, strings.TrimPrefix(line, "a="))
		}
	}
	return fragment
}

func replaceICECredentials(description, ufrag, pwd string) string {
	lines := strings.Split(description, "\r\n")
	for index, line := range lines {
		switch {
		case strings.HasPrefix(line, "a=ice-ufrag:"):
			lines[index] = "a=ice-ufrag:" + ufrag
		case strings.HasPrefix(line, "a=ice-pwd:"):
			lines[index] = "a=ice-pwd:" + pwd
		}
	}
	return strings.Join(lines, "\r\n")
}

func localFragment(description string) string {
	var builder strings.Builder
	seen := map[string]bool{}
	for _, line := range strings.Split(description, "\r\n") {
		if (strings.HasPrefix(line, "a=ice-ufrag:") || strings.HasPrefix(line, "a=ice-pwd:")) && !seen[line[:10]] {
			seen[line[:10]] = true
			builder.WriteString(line + "\r\n")
		}
		if strings.HasPrefix(line, "a=candidate:") {
			builder.WriteString(line + "\r\n")
		}
	}
	builder.WriteString("a=end-of-candidates\r\n")
	return builder.String()
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
