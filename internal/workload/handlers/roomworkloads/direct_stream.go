package roomworkloads

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

const (
	streamFramesContentType = "application/vnd.beam.room-stream-frames.v1"
	streamRecordHeaderBytes = 12
	streamPipelineDepth     = 8
	streamDefaultWait       = 20 * time.Second
	streamMaxWait           = 25 * time.Second
	streamDefaultBatchBytes = 1 << 20
	streamMaxBatchBytes     = 4 << 20
	streamDetailLimit       = 256
	streamRequestBodyLimit  = 4 << 10
	streamBaseHeader        = "X-Beam-Stream-Base-Sequence"
)

const (
	streamOpen   = "open"
	streamEnding = "ending"
	streamEnded  = "ended"
)

// DirectStreamHandler serves room.stream.direct.v1 sessions on the shared
// direct room listener. The source posts sequenced frames of the protected
// beam-mls-stream-v1 byte stream; targets long-poll frame batches and ack
// cumulatively. Frames are freed only once every live target has them.
type DirectStreamHandler struct {
	server           *DirectServer
	now              func() time.Time
	holdTimeout      time.Duration
	overrunGrace     time.Duration
	progressInterval time.Duration
	checkInterval    time.Duration
	linger           time.Duration
}

func NewDirectStreamHandler(server *DirectServer) *DirectStreamHandler {
	return &DirectStreamHandler{server: server, now: time.Now, holdTimeout: 5 * time.Second, overrunGrace: time.Second,
		progressInterval: 5 * time.Second, checkInterval: 250 * time.Millisecond, linger: 30 * time.Second}
}

func (*DirectStreamHandler) Kind() domain.Kind { return domain.KindRoomStream }

func (h *DirectStreamHandler) Validate(spec domain.Spec) error {
	if spec.Kind != domain.KindRoomStream {
		return errors.New("room workload handler received another kind")
	}
	assignment, err := decodeSpec[contracts.StreamUnitDetails](spec)
	if err != nil {
		return err
	}
	if assignment.Identity.Kind != domain.KindRoomStream {
		return errors.New("room stream assignment carries another kind")
	}
	if err := assignment.Details.Validate(); err != nil {
		return err
	}
	if len(assignment.Targets) > contracts.RoomStreamMaxTargets {
		return errors.New("room.stream allows at most 64 targets")
	}
	seen := make(map[string]struct{}, len(assignment.Targets))
	for _, target := range assignment.Targets {
		if target.MemberID == "" {
			return errors.New("room.stream target is empty")
		}
		if _, duplicate := seen[target.MemberID]; duplicate {
			return errors.New("duplicate room.stream target")
		}
		seen[target.MemberID] = struct{}{}
	}
	return ValidateDirectServerConfig(h.server.config)
}

// Execute creates the session and its role tokens, pins the lease certificate,
// reports the runtime, and then serves the session until it ends.
func (h *DirectStreamHandler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	if err := h.Validate(spec); err != nil {
		return domain.Result{}, err
	}
	assignment, _ := decodeSpec[contracts.StreamUnitDetails](spec)
	identity := assignment.Identity
	identity.WorkerID = spec.Identity.WorkerID
	baseURL, certificates, err := h.server.ready()
	if err != nil {
		return domain.Result{}, err
	}
	session, err := newStreamSession(h, identity, assignment)
	if err != nil {
		return domain.Result{}, err
	}
	sessionID := directSessionID(identity)
	if err := h.server.registerStream(sessionID, session); err != nil {
		return domain.Result{}, err
	}
	// An ended session stays addressable briefly so late requests learn the
	// lease is over instead of treating a 404 as Worker loss.
	defer func() {
		session.close()
		time.AfterFunc(h.linger, func() { h.server.unregisterStream(sessionID, session) })
	}()
	fingerprint, err := certificates.ForLease(identity.ExpiresAt)
	if err != nil {
		return domain.Result{}, err
	}
	runtime := contracts.RoomStreamRuntime{SchemaVersion: contracts.RoomStreamRuntimeSchema,
		WorkloadID: identity.WorkloadID, UnitID: identity.UnitID, Epoch: identity.Epoch, Attempt: identity.Attempt,
		WorkerID: identity.WorkerID, Capability: contracts.RoomStreamDirectCapability,
		Transport: contracts.RoomWorkerHTTPSTransport, BaseURL: directSessionURL(baseURL, roomStreamsPath, sessionID),
		TLSCertificateSHA256: fingerprint, ExpiresAt: identity.ExpiresAt, SourceAccessToken: session.sourceToken,
		TargetAccessTokens: session.targetTokens}
	if err := runtime.Validate(identity, session.members, h.now().UTC()); err != nil {
		return domain.Result{}, err
	}
	report(ctx, contracts.StreamProgressDetails{Runtime: &runtime})
	return h.run(ctx, session)
}

func (h *DirectStreamHandler) run(ctx context.Context, session *streamSession) (domain.Result, error) {
	ticker := time.NewTicker(h.checkInterval)
	defer ticker.Stop()
	for {
		session.mu.Lock()
		now := h.now()
		session.evaluateLocked(now)
		details, due := session.progressDueLocked(now)
		if session.state == streamEnded {
			final, processed := session.resultLocked(), session.committedBytes
			session.mu.Unlock()
			return result(final, processed), nil
		}
		changed := session.notify
		session.mu.Unlock()
		if due {
			report(ctx, details)
		}
		select {
		case <-ctx.Done():
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return domain.Result{}, ctx.Err()
			}
			// The assignment deadline is the lease expiry.
			session.mu.Lock()
			session.endLocked(contracts.RoomStreamExpired)
			final, processed := session.resultLocked(), session.committedBytes
			session.mu.Unlock()
			return result(final, processed), nil
		case <-changed:
		case <-ticker.C:
		}
	}
}

type streamSession struct {
	handler      *DirectStreamHandler
	epoch        uint64
	attempt      uint64
	policy       string
	maxBuffer    int64
	liveness     time.Duration
	expiresAt    time.Time
	sourceToken  string
	targetTokens []contracts.RoomStreamTargetToken
	members      []string

	mu             sync.Mutex
	notify         chan struct{}
	version        uint64
	state          string
	terminal       string
	baseKnown      bool
	base           uint64
	accepted       uint64
	committed      uint64
	finalKnown     bool
	final          uint64
	frames         map[uint64][]byte
	held           map[uint64]*heldFrame
	heldBytes      int64
	buffered       int64
	committedBytes int64
	baseAt         time.Time
	source         streamPeer
	sourceActive   bool
	targets        map[string]*streamTarget
	windowFullAt   time.Time
	progressDirty  bool
	committedOnce  bool
	lastProgress   time.Time
}

type streamPeer struct {
	inFlight int
	lastSeen time.Time
}

type streamTarget struct {
	peer      streamPeer
	joined    bool
	state     string
	delivered uint64
	served    uint64
	reason    *string
}

type heldFrame struct {
	data    []byte
	waiters int
}

type streamRole struct {
	source bool
	member string
}

func newStreamSession(h *DirectStreamHandler, identity contracts.RoomWorkloadIdentity,
	assignment contracts.RoomWorkerSpec[contracts.StreamUnitDetails]) (*streamSession, error) {
	sourceToken, err := newAccessToken()
	if err != nil {
		return nil, err
	}
	now := h.now()
	session := &streamSession{handler: h, epoch: identity.Epoch, attempt: identity.Attempt,
		policy: assignment.Details.BackpressurePolicy, maxBuffer: assignment.Details.MaxBufferBytes,
		liveness: time.Duration(assignment.Details.HeartbeatTimeoutMS) * time.Millisecond, expiresAt: identity.ExpiresAt, sourceToken: sourceToken, notify: make(chan struct{}), state: streamOpen,
		frames: make(map[uint64][]byte), held: make(map[uint64]*heldFrame), source: streamPeer{lastSeen: now},
		targets: make(map[string]*streamTarget, len(assignment.Targets)), lastProgress: now}
	for _, target := range assignment.Targets {
		token, err := newAccessToken()
		if err != nil {
			return nil, err
		}
		session.targetTokens = append(session.targetTokens,
			contracts.RoomStreamTargetToken{TargetMemberID: target.MemberID, AccessToken: token})
		session.members = append(session.members, target.MemberID)
		session.targets[target.MemberID] = &streamTarget{state: contracts.RoomStreamTargetJoining}
	}
	slices.Sort(session.members)
	return session, nil
}

func (s *streamSession) close() {
	s.mu.Lock()
	if s.state != streamEnded {
		s.endLocked("")
	}
	s.mu.Unlock()
}

func liveTarget(state string) bool {
	return state == contracts.RoomStreamTargetJoining || state == contracts.RoomStreamTargetActive
}

func (s *streamSession) signalLocked() {
	close(s.notify)
	s.notify = make(chan struct{})
}

// changeLocked records a session or target state change that wakes receipt
// long polls and triggers an immediate progress report.
func (s *streamSession) changeLocked() {
	s.version++
	s.progressDirty = true
	s.signalLocked()
}

func (s *streamSession) endLocked(terminal string) {
	if s.state == streamEnded {
		return
	}
	s.state, s.terminal = streamEnded, terminal
	s.frames, s.held, s.heldBytes = nil, nil, 0
	s.changeLocked()
}

func (s *streamSession) setTargetLocked(target *streamTarget, state string, reason *string) {
	if target.state == state {
		return
	}
	target.state, target.reason = state, reason
	s.changeLocked()
}

func (s *streamSession) failOpenTargetsLocked(reason string) {
	for _, member := range s.members {
		if target := s.targets[member]; liveTarget(target.state) {
			s.setTargetLocked(target, contracts.RoomStreamTargetFailed, &reason)
		}
	}
}

// declareBaseLocked fixes the attempt's base from the first source frame or
// end request and reports whether a later request names the same base.
func (s *streamSession) declareBaseLocked(base uint64, now time.Time) bool {
	if s.baseKnown {
		return s.base == base
	}
	s.baseKnown, s.base, s.baseAt = true, base, now
	s.accepted, s.committed = base-1, base-1
	s.signalLocked()
	return true
}

// advanceLocked moves held frames that have become contiguous into the
// accepted log.
func (s *streamSession) advanceLocked() {
	if s.state == streamEnded || !s.baseKnown {
		return
	}
	advanced := false
	for {
		frame, ok := s.held[s.accepted+1]
		if !ok {
			break
		}
		delete(s.held, s.accepted+1)
		s.heldBytes -= int64(len(frame.data))
		s.accepted++
		s.frames[s.accepted] = frame.data
		s.buffered += int64(len(frame.data))
		advanced = true
	}
	if advanced {
		s.signalLocked()
		s.settleLocked()
	}
}

func (s *streamSession) releaseHeldLocked(seq uint64) {
	frame, ok := s.held[seq]
	if !ok {
		return
	}
	frame.waiters--
	if frame.waiters <= 0 {
		delete(s.held, seq)
		s.heldBytes -= int64(len(frame.data))
	}
}

// commitLocked advances committed_through to the lowest sequence delivered by
// every live target and frees the frames at or below it.
func (s *streamSession) commitLocked() {
	if !s.baseKnown || s.state == streamEnded {
		return
	}
	floor := s.base - 1
	commit := s.accepted
	for _, target := range s.targets {
		if liveTarget(target.state) {
			commit = min(commit, max(target.delivered, floor))
		}
	}
	if commit <= s.committed {
		return
	}
	for seq := s.committed + 1; seq <= commit; seq++ {
		size := int64(len(s.frames[seq]))
		s.committedBytes += size
		s.buffered -= size
		delete(s.frames, seq)
	}
	s.committed = commit
	if !s.committedOnce {
		s.committedOnce = true
		s.progressDirty = true
	}
	s.signalLocked()
}

// settleLocked recomputes the commit and ends the session once no target is
// live: after end{eof|renewed} every target has finished, and before it the
// source has no one left to stream to.
func (s *streamSession) settleLocked() {
	s.commitLocked()
	if s.state == streamEnded {
		return
	}
	for _, target := range s.targets {
		if liveTarget(target.state) {
			return
		}
	}
	s.endLocked(contracts.RoomStreamEnded)
}

func (s *streamSession) advanceTargetLocked(target *streamTarget, delivered uint64) {
	target.delivered = max(target.delivered, delivered)
	if s.finalKnown && liveTarget(target.state) && target.delivered >= s.final {
		s.setTargetLocked(target, contracts.RoomStreamTargetCompleted, nil)
	}
	s.settleLocked()
}

func (s *streamSession) activateLocked(target *streamTarget) {
	if target.state == contracts.RoomStreamTargetJoining {
		s.setTargetLocked(target, contracts.RoomStreamTargetActive, nil)
	}
}

// evaluateLocked applies expiry, source and target liveness, and drop_oldest
// eviction.
func (s *streamSession) evaluateLocked(now time.Time) {
	if s.state == streamEnded {
		return
	}
	if !now.Before(s.expiresAt) {
		s.endLocked(contracts.RoomStreamExpired)
		return
	}
	liveness := s.liveness
	// After end{eof|renewed} the source has nothing left to send; targets drain
	// the accepted frames on their own liveness.
	if s.state == streamOpen && s.source.inFlight == 0 && now.Sub(s.source.lastSeen) >= liveness {
		s.failOpenTargetsLocked(contracts.RoomStreamReasonSourceLost)
		s.endLocked(contracts.RoomStreamSourceLost)
		return
	}
	for _, member := range s.members {
		target := s.targets[member]
		if !liveTarget(target.state) {
			continue
		}
		if !target.joined {
			if s.baseKnown && now.Sub(s.baseAt) >= liveness {
				reason := contracts.RoomStreamReasonTargetJoinTimeout
				s.setTargetLocked(target, contracts.RoomStreamTargetDropped, &reason)
			}
		} else if target.peer.inFlight == 0 && now.Sub(target.peer.lastSeen) >= liveness {
			reason := contracts.RoomStreamReasonTargetHeartbeatTimeout
			s.setTargetLocked(target, contracts.RoomStreamTargetDropped, &reason)
		}
	}
	if s.policy == "drop_oldest" && s.baseKnown {
		if s.maxBuffer-s.buffered < contracts.RoomStreamMaxFrameBytes {
			if s.windowFullAt.IsZero() {
				s.windowFullAt = now
			} else if now.Sub(s.windowFullAt) > s.handler.overrunGrace {
				// Frames are never dropped; the targets holding the commit back are.
				for _, member := range s.members {
					target := s.targets[member]
					if liveTarget(target.state) && max(target.delivered, s.base-1) == s.committed {
						reason := contracts.RoomStreamReasonSlowTargetOverrun
						s.setTargetLocked(target, contracts.RoomStreamTargetDropped, &reason)
					}
				}
				s.windowFullAt = time.Time{}
			}
		} else {
			s.windowFullAt = time.Time{}
		}
	}
	s.settleLocked()
}

func (s *streamSession) progressDueLocked(now time.Time) (contracts.StreamProgressDetails, bool) {
	if s.state == streamEnded {
		return contracts.StreamProgressDetails{}, false
	}
	if !s.progressDirty && (!s.sourceActive || now.Sub(s.lastProgress) < s.handler.progressInterval) {
		return contracts.StreamProgressDetails{}, false
	}
	s.progressDirty, s.lastProgress = false, now
	return contracts.StreamProgressDetails{Sequence: s.committed, Bytes: s.committedBytes,
		BufferedBytes: s.buffered, Dropped: s.droppedLocked(), Targets: s.targetStatesLocked()}, true
}

func (s *streamSession) resultLocked() contracts.StreamResultDetails {
	return contracts.StreamResultDetails{Sequence: s.committed, Bytes: s.committedBytes, BufferedBytes: s.buffered,
		Dropped: s.droppedLocked(), Targets: s.targetStatesLocked(), TerminalReason: s.terminal}
}

func (s *streamSession) droppedLocked() int64 {
	dropped := int64(0)
	for _, target := range s.targets {
		if target.state == contracts.RoomStreamTargetDropped || target.state == contracts.RoomStreamTargetFailed {
			dropped++
		}
	}
	return dropped
}

func (s *streamSession) targetStatesLocked() []contracts.RoomStreamTargetState {
	states := make([]contracts.RoomStreamTargetState, 0, len(s.members))
	for _, member := range s.members {
		target := s.targets[member]
		states = append(states, contracts.RoomStreamTargetState{TargetMemberID: member, State: target.state,
			DeliveredThrough: target.delivered, Reason: target.reason})
	}
	return states
}

type streamReceipt struct {
	BaseSequence     uint64                            `json:"base_sequence"`
	AcceptedThrough  uint64                            `json:"accepted_through"`
	CommittedThrough uint64                            `json:"committed_through"`
	BufferedBytes    int64                             `json:"buffered_bytes"`
	MaxBufferBytes   int64                             `json:"max_buffer_bytes"`
	FinalSequence    *uint64                           `json:"final_sequence"`
	State            string                            `json:"state"`
	Targets          []contracts.RoomStreamTargetState `json:"targets"`
}

// receiptLocked reports base_sequence 0 until the source's first frame or end
// request declares the base.
func (s *streamSession) receiptLocked() *streamReceipt {
	receipt := &streamReceipt{AcceptedThrough: s.accepted, CommittedThrough: s.committed, BufferedBytes: s.buffered,
		MaxBufferBytes: s.maxBuffer, FinalSequence: s.finalLocked(), State: s.state, Targets: s.targetStatesLocked()}
	if s.baseKnown {
		receipt.BaseSequence = s.base
	}
	return receipt
}

func (s *streamSession) finalLocked() *uint64 {
	if !s.finalKnown {
		return nil
	}
	final := s.final
	return &final
}

func (s *streamSession) authenticate(request *http.Request) (streamRole, bool) {
	authorization := request.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return streamRole{}, false
	}
	provided := []byte(strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer ")))
	var role streamRole
	found := false
	if subtle.ConstantTimeCompare(provided, []byte(s.sourceToken)) == 1 {
		role, found = streamRole{source: true}, true
	}
	for _, target := range s.targetTokens {
		if subtle.ConstantTimeCompare(provided, []byte(target.AccessToken)) == 1 {
			role, found = streamRole{member: target.TargetMemberID}, true
		}
	}
	return role, found
}

func (s *streamSession) attemptMatches(request *http.Request) bool {
	epoch, epochErr := strconv.ParseUint(strings.TrimSpace(request.Header.Get("X-Beam-Stream-Epoch")), 10, 64)
	attempt, attemptErr := strconv.ParseUint(strings.TrimSpace(request.Header.Get("X-Beam-Stream-Attempt")), 10, 64)
	return epochErr == nil && attemptErr == nil && epoch == s.epoch && attempt == s.attempt
}

// track marks a request in progress for liveness until the returned function
// runs.
func (s *streamSession) track(role streamRole) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	peer := &s.source
	if role.source {
		s.sourceActive = true
	} else {
		target := s.targets[role.member]
		target.joined = true
		peer = &target.peer
	}
	peer.inFlight++
	peer.lastSeen = s.handler.now()
	return func() {
		s.mu.Lock()
		peer.inFlight--
		peer.lastSeen = s.handler.now()
		s.mu.Unlock()
	}
}

func serveStream(session *streamSession, response http.ResponseWriter, request *http.Request, parts []string) {
	if session == nil {
		writeStreamError(response, http.StatusNotFound, "session_not_found", "no stream session at this path", nil)
		return
	}
	role, ok := session.authenticate(request)
	if !ok {
		writeStreamError(response, http.StatusUnauthorized, "unauthorized", "missing or unknown access token", nil)
		return
	}
	if !session.attemptMatches(request) {
		writeStreamError(response, http.StatusConflict, "attempt_mismatch",
			"X-Beam-Stream-Epoch and X-Beam-Stream-Attempt must name this attempt", nil)
		return
	}
	switch {
	case len(parts) == 1 && parts[0] == "heartbeat":
		if requireMethod(response, request, http.MethodPost) {
			defer session.track(role)()
			session.heartbeat(response, role)
		}
	case len(parts) >= 1 && parts[0] == "source":
		if !role.source {
			writeStreamError(response, http.StatusForbidden, "forbidden", "a target token cannot use source endpoints", nil)
			return
		}
		session.serveSource(response, request, parts[1:])
	case len(parts) == 3 && parts[0] == "targets":
		if _, exists := session.targets[parts[1]]; !exists {
			writeStreamError(response, http.StatusNotFound, "target_not_found", "no such stream target", nil)
			return
		}
		if role.source || role.member != parts[1] {
			writeStreamError(response, http.StatusForbidden, "forbidden", "this token cannot act for that target", nil)
			return
		}
		session.serveTarget(response, request, role, parts[2])
	default:
		writeStreamError(response, http.StatusNotFound, "not_found", "unknown stream endpoint", nil)
	}
}

func (s *streamSession) serveSource(response http.ResponseWriter, request *http.Request, parts []string) {
	role := streamRole{source: true}
	switch {
	case len(parts) == 2 && parts[0] == "frames":
		if requireMethod(response, request, http.MethodPost) {
			defer s.track(role)()
			s.postFrame(response, request, parts[1])
		}
	case len(parts) == 1 && parts[0] == "receipt":
		if requireMethod(response, request, http.MethodGet) {
			defer s.track(role)()
			s.sourceReceipt(response, request)
		}
	case len(parts) == 1 && parts[0] == "end":
		if requireMethod(response, request, http.MethodPost) {
			defer s.track(role)()
			s.sourceEnd(response, request)
		}
	default:
		writeStreamError(response, http.StatusNotFound, "not_found", "unknown stream endpoint", nil)
	}
}

func (s *streamSession) serveTarget(response http.ResponseWriter, request *http.Request, role streamRole, action string) {
	switch action {
	case "frames":
		if requireMethod(response, request, http.MethodGet) {
			defer s.track(role)()
			s.targetFrames(response, request, role.member)
		}
	case "ack":
		if requireMethod(response, request, http.MethodPost) {
			defer s.track(role)()
			s.targetAck(response, request, role.member)
		}
	case "close":
		if requireMethod(response, request, http.MethodPost) {
			defer s.track(role)()
			s.targetClose(response, request, role.member)
		}
	default:
		writeStreamError(response, http.StatusNotFound, "not_found", "unknown stream endpoint", nil)
	}
}

func (s *streamSession) heartbeat(response http.ResponseWriter, role streamRole) {
	s.mu.Lock()
	if role.source {
		receipt := s.receiptLocked()
		ended := s.state == streamEnded
		s.mu.Unlock()
		if ended {
			writeStreamError(response, http.StatusGone, "session_ended", "the stream lease has ended", &streamErrorExtra{receipt: receipt})
			return
		}
		writeStreamJSON(response, http.StatusOK, receipt)
		return
	}
	target := s.targets[role.member]
	if s.state == streamEnded && liveTarget(target.state) {
		s.mu.Unlock()
		writeStreamError(response, http.StatusGone, "session_ended", "the stream lease has ended", nil)
		return
	}
	reply := struct {
		State         string  `json:"state"`
		FinalSequence *uint64 `json:"final_sequence"`
	}{target.state, s.finalLocked()}
	s.mu.Unlock()
	writeStreamJSON(response, http.StatusOK, reply)
}

func (s *streamSession) postFrame(response http.ResponseWriter, request *http.Request, rawSequence string) {
	seq, err := strconv.ParseUint(rawSequence, 10, 64)
	if err != nil || seq == 0 {
		response.Header().Set("Connection", "close")
		writeStreamError(response, http.StatusBadRequest, "invalid_request", "frame sequence must be a positive integer", nil)
		return
	}
	base, ok := declaredBase(request)
	if !ok {
		response.Header().Set("Connection", "close")
		writeStreamError(response, http.StatusBadRequest, "invalid_request", streamBaseHeader+" must be a positive integer", nil)
		return
	}
	switch {
	case request.ContentLength < 0:
		response.Header().Set("Connection", "close")
		writeStreamError(response, http.StatusLengthRequired, "length_required", "frames require Content-Length", nil)
		return
	case request.ContentLength == 0:
		writeStreamError(response, http.StatusBadRequest, "empty_frame", "frames carry 1 to 65536 bytes", nil)
		return
	case request.ContentLength > contracts.RoomStreamMaxFrameBytes:
		response.Header().Set("Connection", "close")
		writeStreamError(response, http.StatusRequestEntityTooLarge, "frame_too_large", "frames carry at most 65536 bytes", nil)
		return
	}
	data := make([]byte, request.ContentLength)
	if _, err := io.ReadFull(request.Body, data); err != nil {
		writeStreamError(response, http.StatusBadRequest, "invalid_request", "frame body is shorter than Content-Length", nil)
		return
	}
	if seq == 1 && !bytes.HasPrefix(data, []byte("BMS1")) {
		writeStreamError(response, http.StatusBadRequest, "invalid_protected_stream", "a stream starts with a BMS1 header", nil)
		return
	}
	s.acceptFrame(response, request.Context(), base, seq, data)
}

// declaredBase reads the base sequence the source names for this attempt.
func declaredBase(request *http.Request) (uint64, bool) {
	base, err := strconv.ParseUint(strings.TrimSpace(request.Header.Get(streamBaseHeader)), 10, 64)
	return base, err == nil && base > 0
}

func (s *streamSession) acceptFrame(response http.ResponseWriter, ctx context.Context, base, seq uint64, data []byte) {
	s.mu.Lock()
	now := s.handler.now()
	fail := func(status int, code, detail string) {
		receipt := s.receiptLocked()
		s.mu.Unlock()
		writeStreamError(response, status, code, detail, &streamErrorExtra{receipt: receipt})
	}
	if s.state == streamEnded {
		fail(http.StatusGone, "session_ended", "the stream lease has ended")
		return
	}
	if !s.declareBaseLocked(base, now) {
		fail(http.StatusConflict, "base_mismatch", "this attempt already has another base sequence")
		return
	}
	if seq < s.base {
		fail(http.StatusConflict, "sequence_gap", "frame precedes this attempt's base sequence")
		return
	}
	if seq <= s.accepted {
		if seq <= s.committed || bytes.Equal(s.frames[seq], data) {
			receipt := s.receiptLocked()
			s.mu.Unlock()
			writeStreamJSON(response, http.StatusOK, receipt)
			return
		}
		fail(http.StatusConflict, "frame_conflict", "frame differs from the accepted frame")
		return
	}
	if s.state == streamEnding {
		fail(http.StatusConflict, "frame_conflict", "frame follows the declared final sequence")
		return
	}
	if seq > s.accepted+streamPipelineDepth {
		fail(http.StatusConflict, "sequence_gap", "frame is too far ahead of accepted_through")
		return
	}
	if held, ok := s.held[seq]; ok {
		if !bytes.Equal(held.data, data) {
			fail(http.StatusConflict, "frame_conflict", "frame differs from the pending frame")
			return
		}
		held.waiters++
	} else {
		if s.buffered+s.heldBytes+int64(len(data)) > s.maxBuffer {
			fail(http.StatusTooManyRequests, "window_full", "frame exceeds max_buffer_bytes beyond the commit")
			return
		}
		s.held[seq] = &heldFrame{data: data, waiters: 1}
		s.heldBytes += int64(len(data))
	}
	s.advanceLocked()
	deadline := now.Add(s.handler.holdTimeout)
	for {
		switch {
		case seq <= s.accepted:
			receipt := s.receiptLocked()
			s.mu.Unlock()
			writeStreamJSON(response, http.StatusOK, receipt)
			return
		case s.state == streamEnded:
			fail(http.StatusGone, "session_ended", "the stream lease has ended")
			return
		case !now.Before(deadline):
			s.releaseHeldLocked(seq)
			fail(http.StatusConflict, "sequence_gap", "missing predecessors did not arrive in time")
			return
		}
		if !s.wait(ctx, deadline) {
			s.releaseHeldLocked(seq)
			s.mu.Unlock()
			return
		}
		now = s.handler.now()
	}
}

// wait releases the lock until the session changes, until is reached, or the
// request ends; it returns with the lock held and reports whether the request
// is still live.
func (s *streamSession) wait(ctx context.Context, until time.Time) bool {
	changed := s.notify
	s.mu.Unlock()
	timer := time.NewTimer(max(0, until.Sub(s.handler.now())))
	select {
	case <-changed:
	case <-timer.C:
	case <-ctx.Done():
	}
	timer.Stop()
	s.mu.Lock()
	return ctx.Err() == nil
}

func (s *streamSession) sourceReceipt(response http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	after, hasAfter, err := queryUint(query, "committed_after")
	wait, waitErr := queryWait(query)
	if err != nil || waitErr != nil {
		writeStreamError(response, http.StatusBadRequest, "invalid_request", "committed_after and wait_ms must be non-negative integers", nil)
		return
	}
	s.mu.Lock()
	if s.state == streamEnded {
		receipt := s.receiptLocked()
		s.mu.Unlock()
		writeStreamError(response, http.StatusGone, "session_ended", "the stream lease has ended", &streamErrorExtra{receipt: receipt})
		return
	}
	version := s.version
	deadline := s.handler.now().Add(wait)
	for hasAfter && s.committed <= after && s.version == version && s.handler.now().Before(deadline) {
		if !s.wait(request.Context(), deadline) {
			s.mu.Unlock()
			return
		}
	}
	receipt := s.receiptLocked()
	s.mu.Unlock()
	writeStreamJSON(response, http.StatusOK, receipt)
}

func (s *streamSession) sourceEnd(response http.ResponseWriter, request *http.Request) {
	var body struct {
		FinalSequence *uint64 `json:"final_sequence"`
		Reason        string  `json:"reason"`
	}
	if !decodeStreamBody(request, &body) || (body.Reason != "eof" && body.Reason != "renewed" && body.Reason != "aborted") ||
		(body.Reason != "aborted" && body.FinalSequence == nil) {
		writeStreamError(response, http.StatusBadRequest, "invalid_request", "end requires reason eof, renewed or aborted and a final_sequence", nil)
		return
	}
	base, ok := declaredBase(request)
	if !ok {
		writeStreamError(response, http.StatusBadRequest, "invalid_request", streamBaseHeader+" must be a positive integer", nil)
		return
	}
	s.mu.Lock()
	fail := func(status int, code, detail string) {
		receipt := s.receiptLocked()
		s.mu.Unlock()
		writeStreamError(response, status, code, detail, &streamErrorExtra{receipt: receipt})
	}
	if s.state == streamEnded {
		fail(http.StatusGone, "session_ended", "the stream lease has ended")
		return
	}
	if !s.declareBaseLocked(base, s.handler.now()) {
		fail(http.StatusConflict, "base_mismatch", "this attempt already has another base sequence")
		return
	}
	if body.Reason == "aborted" {
		s.failOpenTargetsLocked(contracts.RoomStreamReasonSourceAborted)
		s.endLocked(contracts.RoomStreamSourceAborted)
		receipt := s.receiptLocked()
		s.mu.Unlock()
		writeStreamJSON(response, http.StatusOK, receipt)
		return
	}
	// A lease that carried no frames ends with final_sequence base-1.
	final := *body.FinalSequence
	if final != s.accepted {
		fail(http.StatusConflict, "frames_missing", "final_sequence must equal accepted_through")
		return
	}
	if s.state == streamOpen {
		s.state, s.finalKnown, s.final = streamEnding, true, final
		s.changeLocked()
		for _, member := range s.members {
			if target := s.targets[member]; liveTarget(target.state) && target.delivered >= final {
				s.setTargetLocked(target, contracts.RoomStreamTargetCompleted, nil)
			}
		}
		s.settleLocked()
	}
	receipt := s.receiptLocked()
	s.mu.Unlock()
	writeStreamJSON(response, http.StatusOK, receipt)
}

func (s *streamSession) targetFrames(response http.ResponseWriter, request *http.Request, member string) {
	query := request.URL.Query()
	after, hasAfter, afterErr := queryUint(query, "after")
	maxBytes, hasMaxBytes, maxErr := queryUint(query, "max_bytes")
	wait, waitErr := queryWait(query)
	if afterErr != nil || !hasAfter || maxErr != nil || (hasMaxBytes && maxBytes == 0) || waitErr != nil {
		writeStreamError(response, http.StatusBadRequest, "invalid_request", "after is required; max_bytes and wait_ms must be valid", nil)
		return
	}
	limit := uint64(streamDefaultBatchBytes)
	if hasMaxBytes {
		limit = min(maxBytes, streamMaxBatchBytes)
	}
	s.mu.Lock()
	target := s.targets[member]
	deadline := s.handler.now().Add(wait)
	first := true
	for {
		if closed, reply := s.targetClosedLocked(target); closed {
			s.mu.Unlock()
			reply(response)
			return
		}
		if s.baseKnown && after < s.base-1 {
			base := s.base
			s.mu.Unlock()
			writeStreamError(response, http.StatusConflict, "before_base",
				"this attempt starts after the requested sequence", &streamErrorExtra{baseSequence: &base})
			return
		}
		if first {
			first = false
			s.activateLocked(target)
			s.advanceTargetLocked(target, after)
			continue
		}
		if s.baseKnown && s.state != streamEnded {
			if start := max(after, s.committed) + 1; start <= s.accepted {
				body := s.batchLocked(target, start, limit)
				headers := s.frameHeadersLocked()
				s.mu.Unlock()
				headers.write(response.Header())
				response.Header().Set("Content-Type", streamFramesContentType)
				response.Header().Set("Content-Length", strconv.Itoa(len(body)))
				response.WriteHeader(http.StatusOK)
				_, _ = response.Write(body)
				return
			}
		}
		if target.state == contracts.RoomStreamTargetCompleted || (s.finalKnown && after >= s.final) ||
			!s.handler.now().Before(deadline) {
			headers := s.frameHeadersLocked()
			s.mu.Unlock()
			headers.write(response.Header())
			response.WriteHeader(http.StatusNoContent)
			return
		}
		if !s.wait(request.Context(), deadline) {
			s.mu.Unlock()
			return
		}
	}
}

// batchLocked encodes consecutive frames from start as
// seq u64 BE | len u32 BE | bytes records, up to limit bytes and at least one.
func (s *streamSession) batchLocked(target *streamTarget, start, limit uint64) []byte {
	var body []byte
	for seq := start; seq <= s.accepted; seq++ {
		frame := s.frames[seq]
		if len(body) > 0 && uint64(len(body)+streamRecordHeaderBytes+len(frame)) > limit {
			break
		}
		body = binary.BigEndian.AppendUint64(body, seq)
		body = binary.BigEndian.AppendUint32(body, uint32(len(frame)))
		body = append(body, frame...)
		target.served = max(target.served, seq)
	}
	return body
}

type streamFrameHeaders struct {
	base, accepted uint64
	final          *uint64
}

func (s *streamSession) frameHeadersLocked() streamFrameHeaders {
	headers := streamFrameHeaders{accepted: s.accepted, final: s.finalLocked()}
	if s.baseKnown {
		headers.base = s.base
	}
	return headers
}

func (h streamFrameHeaders) write(header http.Header) {
	header.Set("X-Beam-Stream-Base-Sequence", strconv.FormatUint(h.base, 10))
	header.Set("X-Beam-Stream-Accepted-Through", strconv.FormatUint(h.accepted, 10))
	if h.final != nil {
		header.Set("X-Beam-Stream-Final-Sequence", strconv.FormatUint(*h.final, 10))
	}
}

// targetClosedLocked answers requests from a dropped or failed target, and
// from a still-live target once the session itself has ended.
func (s *streamSession) targetClosedLocked(target *streamTarget) (bool, func(http.ResponseWriter)) {
	if target.state == contracts.RoomStreamTargetDropped || target.state == contracts.RoomStreamTargetFailed {
		state, reason := target.state, target.reason
		return true, func(response http.ResponseWriter) {
			writeStreamError(response, http.StatusGone, "target_closed", "this target is "+state, &streamErrorExtra{reason: reason})
		}
	}
	if s.state == streamEnded && liveTarget(target.state) {
		return true, func(response http.ResponseWriter) {
			writeStreamError(response, http.StatusGone, "session_ended", "the stream lease has ended", nil)
		}
	}
	return false, nil
}

func (s *streamSession) targetAck(response http.ResponseWriter, request *http.Request, member string) {
	var body struct {
		DeliveredThrough *uint64 `json:"delivered_through"`
	}
	if !decodeStreamBody(request, &body) || body.DeliveredThrough == nil {
		writeStreamError(response, http.StatusBadRequest, "invalid_request", "ack requires delivered_through", nil)
		return
	}
	delivered := *body.DeliveredThrough
	s.mu.Lock()
	target := s.targets[member]
	if closed, reply := s.targetClosedLocked(target); closed {
		s.mu.Unlock()
		reply(response)
		return
	}
	if delivered < target.delivered || delivered > max(target.served, target.delivered) {
		s.mu.Unlock()
		writeStreamError(response, http.StatusBadRequest, "ack_beyond_served",
			"delivered_through must not decrease or pass the highest served sequence", nil)
		return
	}
	s.activateLocked(target)
	s.advanceTargetLocked(target, delivered)
	reply := struct {
		DeliveredThrough uint64  `json:"delivered_through"`
		CommittedThrough uint64  `json:"committed_through"`
		FinalSequence    *uint64 `json:"final_sequence"`
		State            string  `json:"state"`
	}{target.delivered, s.committed, s.finalLocked(), target.state}
	s.mu.Unlock()
	writeStreamJSON(response, http.StatusOK, reply)
}

func (s *streamSession) targetClose(response http.ResponseWriter, request *http.Request, member string) {
	var body struct {
		State            string  `json:"state"`
		DeliveredThrough *uint64 `json:"delivered_through"`
		Reason           string  `json:"reason"`
	}
	if !decodeStreamBody(request, &body) || body.DeliveredThrough == nil ||
		(body.State != contracts.RoomStreamTargetDropped && body.State != contracts.RoomStreamTargetFailed) ||
		body.Reason == "" || len(body.Reason) > 512 {
		writeStreamError(response, http.StatusBadRequest, "invalid_request",
			"close requires state dropped or failed, delivered_through, and a reason of at most 512 bytes", nil)
		return
	}
	s.mu.Lock()
	target := s.targets[member]
	if !liveTarget(target.state) {
		s.mu.Unlock()
		response.WriteHeader(http.StatusNoContent)
		return
	}
	if closed, reply := s.targetClosedLocked(target); closed {
		s.mu.Unlock()
		reply(response)
		return
	}
	if *body.DeliveredThrough > max(target.served, target.delivered) {
		s.mu.Unlock()
		writeStreamError(response, http.StatusBadRequest, "ack_beyond_served", "delivered_through passes the highest served sequence", nil)
		return
	}
	target.delivered = max(target.delivered, *body.DeliveredThrough)
	reason := body.Reason
	s.setTargetLocked(target, body.State, &reason)
	s.settleLocked()
	s.mu.Unlock()
	response.WriteHeader(http.StatusNoContent)
}

type streamErrorExtra struct {
	receipt      *streamReceipt
	baseSequence *uint64
	reason       *string
}

func writeStreamError(response http.ResponseWriter, status int, code, detail string, extra *streamErrorExtra) {
	if len(detail) > streamDetailLimit {
		detail = detail[:streamDetailLimit]
	}
	body := struct {
		Error        string         `json:"error"`
		Detail       string         `json:"detail"`
		Receipt      *streamReceipt `json:"receipt,omitempty"`
		BaseSequence *uint64        `json:"base_sequence,omitempty"`
		Reason       *string        `json:"reason,omitempty"`
	}{Error: code, Detail: detail}
	if extra != nil {
		body.Receipt, body.BaseSequence, body.Reason = extra.receipt, extra.baseSequence, extra.reason
	}
	writeStreamJSON(response, status, body)
}

func writeStreamJSON(response http.ResponseWriter, status int, value any) {
	encoded, _ := json.Marshal(value)
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	response.WriteHeader(status)
	_, _ = response.Write(encoded)
}

func requireMethod(response http.ResponseWriter, request *http.Request, method string) bool {
	if request.Method == method {
		return true
	}
	response.Header().Set("Allow", method)
	writeStreamError(response, http.StatusMethodNotAllowed, "method_not_allowed", "use "+method, nil)
	return false
}

func decodeStreamBody(request *http.Request, value any) bool {
	decoder := json.NewDecoder(io.LimitReader(request.Body, streamRequestBodyLimit))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return false
	}
	return !decoder.More()
}

func queryUint(query url.Values, name string) (uint64, bool, error) {
	raw, present := query[name]
	if !present {
		return 0, false, nil
	}
	if len(raw) != 1 {
		return 0, true, errors.New("repeated query parameter")
	}
	value, err := strconv.ParseUint(raw[0], 10, 64)
	return value, true, err
}

func queryWait(query url.Values) (time.Duration, error) {
	value, present, err := queryUint(query, "wait_ms")
	if err != nil {
		return 0, err
	}
	if !present {
		return streamDefaultWait, nil
	}
	return time.Duration(min(value, uint64(streamMaxWait/time.Millisecond))) * time.Millisecond, nil
}
