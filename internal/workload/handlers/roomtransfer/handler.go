package roomtransfer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

const (
	maxReceiptBytes        = 64 << 10
	maxTargets             = 10_000
	maxChunkBytes          = 64 << 20
	protectedChunkOverhead = 1 + 8 + 16
)

type Config struct {
	ListenAddress string
	AdvertiseURL  string
}

type Handler struct {
	config   Config
	now      func() time.Time
	serverMu sync.Mutex
	server   *sharedServer
}

type sharedServer struct {
	listener net.Listener
	http     *http.Server
	baseURL  string
	mu       sync.RWMutex
	sessions map[string]*session
	done     chan struct{}
	err      error
}

type session struct {
	transfer  contracts.RoomTransfer
	token     string
	now       func() time.Time
	mu        sync.Mutex
	chunks    map[int64]*chunk
	completed map[int64]*completedChunk
	expected  int64
	failure   *contracts.SourceFailureReceipt
	changed   chan struct{}
}

type chunk struct {
	payload []byte
	source  contracts.SourceRangeReceipt
	targets map[string]contracts.TargetRangeReceipt
	finals  map[string]contracts.FinalTargetReceipt
}

type completedChunk struct {
	source  contracts.SourceRangeReceipt
	targets map[string]contracts.TargetRangeReceipt
	finals  map[string]contracts.FinalTargetReceipt
}

type checkpointValue struct {
	BatchID        string                                  `json:"batch_id"`
	TransferID     string                                  `json:"transfer_id"`
	LaneID         string                                  `json:"lane_id"`
	SourceReceipts map[int64]contracts.SourceRangeReceipt  `json:"source_receipts"`
	TargetReceipts map[string]contracts.TargetRangeReceipt `json:"target_receipts"`
	FinalReceipts  map[string]contracts.FinalTargetReceipt `json:"final_receipts"`
}

type targetResponse struct {
	RangeReceipt contracts.TargetRangeReceipt  `json:"range_receipt"`
	FinalReceipt *contracts.FinalTargetReceipt `json:"final_receipt,omitempty"`
}

func NewHandler(config Config) *Handler {
	if strings.TrimSpace(config.ListenAddress) == "" {
		config.ListenAddress = "127.0.0.1:0"
	}
	return &Handler{config: config, now: time.Now}
}

func (*Handler) Kind() domain.Kind { return domain.KindRoomTransfer }

func (h *Handler) Validate(spec domain.Spec) error {
	transfer, err := decodeTransfer(spec)
	if err != nil {
		return err
	}
	if _, _, err := net.SplitHostPort(h.config.ListenAddress); err != nil {
		return errors.New("room transfer listen address must be host:port")
	}
	if h.config.AdvertiseURL != "" {
		parsed, err := url.Parse(strings.ReplaceAll(h.config.AdvertiseURL, "{port}", "1"))
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return errors.New("room transfer advertise URL must be absolute")
		}
	}
	return validateTransfer(transfer, h.now().UTC())
}

func (h *Handler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	transfer, err := decodeTransfer(spec)
	if err != nil {
		return domain.Result{}, err
	}
	if err := h.Validate(spec); err != nil {
		return domain.Result{}, err
	}
	resume := checkpointValue{BatchID: transfer.BatchID, TransferID: transfer.TransferID, LaneID: transfer.LaneID,
		SourceReceipts: map[int64]contracts.SourceRangeReceipt{}, TargetReceipts: map[string]contracts.TargetRangeReceipt{},
		FinalReceipts: map[string]contracts.FinalTargetReceipt{}}
	if _, ok, checkpointErr := workloadcheckpoint.Current(ctx, contracts.RoomTransferSchemaVersion, &resume); checkpointErr != nil {
		return domain.Result{}, checkpointErr
	} else if ok && (resume.BatchID != transfer.BatchID || resume.TransferID != transfer.TransferID || resume.LaneID != transfer.LaneID) {
		return domain.Result{}, errors.New("room transfer checkpoint belongs to another assignment")
	}
	initializeCheckpoint(&resume)
	if err := verifyCheckpoint(transfer, resume, h.now().UTC()); err != nil {
		return domain.Result{}, err
	}

	shared, err := h.sharedServer()
	if err != nil {
		return domain.Result{}, err
	}
	runtimeToken, err := randomToken(32)
	if err != nil {
		return domain.Result{}, err
	}
	sessionID := fmt.Sprintf("%s-%s-%d", transfer.TransferID, transfer.LaneID, transfer.Attempt)
	active := &session{transfer: transfer, token: runtimeToken, now: h.now,
		chunks: make(map[int64]*chunk), completed: make(map[int64]*completedChunk),
		expected: transfer.ChunkStart, changed: make(chan struct{}, 1)}
	if err := shared.add(sessionID, active); err != nil {
		return domain.Result{}, err
	}
	defer shared.remove(sessionID)
	expiresAt := transfer.SourceLease.ExpiresAt
	for _, target := range transfer.Targets {
		if target.Lease.ExpiresAt.Before(expiresAt) {
			expiresAt = target.Lease.ExpiresAt
		}
	}
	directRuntime := contracts.DirectRoomTransferRuntime{LaneID: transfer.LaneID, Attempt: transfer.Attempt,
		WorkerID: spec.Identity.WorkerID, Capability: contracts.RoomTransferDirectCapability,
		Transport: "worker_http", BaseURL: strings.TrimRight(shared.baseURL, "/") + "/v1/room-transfers/" + url.PathEscape(sessionID),
		AccessToken: runtimeToken, ExpiresAt: expiresAt}
	reportRuntime(ctx, directRuntime)

	var bytesProcessed int64
	for chunkIndex := transfer.ChunkStart; chunkIndex <= transfer.ChunkEnd; chunkIndex++ {
		current, waitErr := active.waitForSource(ctx, chunkIndex)
		if waitErr != nil {
			return result(transfer, resume, sourceFailure(waitErr, chunkIndex), bytesProcessed)
		}
		resume.SourceReceipts[chunkIndex] = current.source
		bytesProcessed += current.source.Length
		for _, target := range transfer.Targets {
			response, waitErr := active.waitForTarget(ctx, chunkIndex, target.MemberID)
			if waitErr != nil {
				failure := contracts.RoomFailure{Origin: "target_agent", Code: "target_write_failed", Retryable: true,
					TargetMemberID: target.MemberID, ChunkIndices: []int64{chunkIndex}, Detail: waitErr.Error()}
				return result(transfer, resume, []contracts.RoomFailure{failure}, bytesProcessed)
			}
			key := receiptKey(target.MemberID, chunkIndex)
			resume.TargetReceipts[key] = response.RangeReceipt
			if response.FinalReceipt != nil {
				resume.FinalReceipts[target.MemberID] = *response.FinalReceipt
			}
			if err := saveCheckpoint(ctx, resume, key); err != nil {
				return domain.Result{}, err
			}
			workloadprogress.Report(ctx, map[string]string{"phase": "delivered", "lane_id": transfer.LaneID,
				"target_member_id": target.MemberID, "chunk_index": strconv.FormatInt(chunkIndex, 10)})
		}
		active.release(chunkIndex)
	}
	return result(transfer, resume, nil, bytesProcessed)
}

func (h *Handler) sharedServer() (*sharedServer, error) {
	h.serverMu.Lock()
	defer h.serverMu.Unlock()
	if h.server != nil {
		select {
		case <-h.server.done:
			return nil, h.server.failure()
		default:
			return h.server, nil
		}
	}
	listener, err := net.Listen("tcp", h.config.ListenAddress)
	if err != nil {
		return nil, err
	}
	baseURL, err := advertisedURL(h.config.AdvertiseURL, listener.Addr())
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	shared := &sharedServer{listener: listener, baseURL: baseURL, sessions: make(map[string]*session), done: make(chan struct{})}
	shared.http = &http.Server{Handler: shared, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Minute, WriteTimeout: 30 * time.Minute, IdleTimeout: time.Minute}
	h.server = shared
	go shared.serve()
	return shared, nil
}

func (s *sharedServer) serve() {
	err := s.http.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		err = errors.New("room transfer direct server stopped")
	}
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	close(s.done)
}

func (s *sharedServer) add(id string, value *session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if _, exists := s.sessions[id]; exists {
		return errors.New("room transfer session is already active")
	}
	s.sessions[id] = value
	return nil
}

func (s *sharedServer) remove(id string) { s.mu.Lock(); delete(s.sessions, id); s.mu.Unlock() }
func (s *sharedServer) failure() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.err != nil {
		return s.err
	}
	return errors.New("room transfer direct server stopped")
}

func (s *sharedServer) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "room-transfers" {
		http.NotFound(response, request)
		return
	}
	sessionID, err := url.PathUnescape(parts[2])
	if err != nil {
		http.NotFound(response, request)
		return
	}
	s.mu.RLock()
	active := s.sessions[sessionID]
	s.mu.RUnlock()
	if active == nil {
		http.NotFound(response, request)
		return
	}
	active.ServeHTTP(response, request, parts[3:])
}

func (s *session) ServeHTTP(response http.ResponseWriter, request *http.Request, parts []string) {
	if !secureEqual(bearer(request), s.token) {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	if len(parts) == 3 && parts[0] == "source" && parts[1] == "chunks" && request.Method == http.MethodPost {
		s.acceptSource(response, request, parts[2])
		return
	}
	if len(parts) == 3 && parts[0] == "source" && parts[1] == "failures" && request.Method == http.MethodPost {
		s.acceptSourceFailure(response, request, parts[2])
		return
	}
	if len(parts) == 4 && parts[0] == "targets" && parts[2] == "chunks" && request.Method == http.MethodGet {
		s.sendTargetChunk(response, request, parts[1], parts[3])
		return
	}
	if len(parts) == 5 && parts[0] == "targets" && parts[2] == "chunks" && parts[4] == "receipt" && request.Method == http.MethodPost {
		s.acceptTargetReceipt(response, request, parts[1], parts[3])
		return
	}
	http.NotFound(response, request)
}

func (s *session) acceptSource(response http.ResponseWriter, request *http.Request, rawIndex string) {
	index, err := s.authorizeSource(request, rawIndex)
	if err != nil {
		http.Error(response, err.Error(), http.StatusForbidden)
		return
	}
	offset, length := contracts.ChunkRange(s.transfer.FileSizeBytes, s.transfer.ChunkSizeBytes, index)
	payload, err := io.ReadAll(io.LimitReader(request.Body, length+protectedChunkOverhead+1))
	if err != nil || !validProtectedChunk(payload, s.transfer.Protection, length) {
		http.Error(response, "invalid protected source chunk", http.StatusBadRequest)
		return
	}
	receipt, err := decodeHeader[contracts.SourceRangeReceipt](request.Header.Get("X-Beam-Source-Receipt"))
	digest := sha256.Sum256(payload)
	if err != nil || receipt.TransferID != s.transfer.TransferID || receipt.LaneID != s.transfer.LaneID ||
		receipt.ChunkIndex != index || receipt.Offset != offset || receipt.Length != length ||
		receipt.LeaseID != s.transfer.SourceLease.LeaseID || receipt.RangeSHA256 != hex.EncodeToString(digest[:]) ||
		receipt.Verify(s.transfer.SourceLease.AgentPublicKey, s.now().UTC()) != nil {
		http.Error(response, "invalid signed source receipt", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if completed := s.completed[index]; completed != nil {
		if completed.source.RangeSHA256 != receipt.RangeSHA256 {
			s.mu.Unlock()
			http.Error(response, "source retry hash mismatch", http.StatusConflict)
			return
		}
		s.mu.Unlock()
		response.WriteHeader(http.StatusNoContent)
		return
	}
	if index != s.expected {
		s.mu.Unlock()
		http.Error(response, "source chunk is not ready", http.StatusTooEarly)
		return
	}
	if existing := s.chunks[index]; existing != nil {
		if existing.source.RangeSHA256 != receipt.RangeSHA256 {
			s.mu.Unlock()
			http.Error(response, "source retry hash mismatch", http.StatusConflict)
			return
		}
	} else {
		s.chunks[index] = &chunk{payload: payload, source: receipt,
			targets: make(map[string]contracts.TargetRangeReceipt), finals: make(map[string]contracts.FinalTargetReceipt)}
	}
	s.signalLocked()
	s.mu.Unlock()
	response.WriteHeader(http.StatusNoContent)
}

func (s *session) acceptSourceFailure(response http.ResponseWriter, request *http.Request, rawIndex string) {
	index, err := s.authorizeSource(request, rawIndex)
	if err != nil {
		http.Error(response, err.Error(), http.StatusForbidden)
		return
	}
	var receipt contracts.SourceFailureReceipt
	if err := decodeJSON(request, &receipt); err != nil || receipt.TransferID != s.transfer.TransferID ||
		receipt.LaneID != s.transfer.LaneID || receipt.ChunkIndex != index || receipt.LeaseID != s.transfer.SourceLease.LeaseID ||
		receipt.Verify(s.transfer.SourceLease.AgentPublicKey, s.now().UTC()) != nil {
		http.Error(response, "invalid signed source failure", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.failure = &receipt
	s.signalLocked()
	s.mu.Unlock()
	response.WriteHeader(http.StatusNoContent)
}

func (s *session) sendTargetChunk(response http.ResponseWriter, request *http.Request, rawMember, rawIndex string) {
	memberID, index, _, err := s.authorizeTarget(request, rawMember, rawIndex)
	if err != nil {
		http.Error(response, err.Error(), http.StatusForbidden)
		return
	}
	s.mu.Lock()
	current := s.chunks[index]
	if current != nil {
		payload := append([]byte(nil), current.payload...)
		digest := current.source.RangeSHA256
		s.mu.Unlock()
		response.Header().Set("Content-Type", "application/octet-stream")
		response.Header().Set("X-Beam-Range-SHA256", digest)
		response.Header().Set("X-Beam-Target-Member-ID", memberID)
		_, _ = response.Write(payload)
		return
	}
	s.mu.Unlock()
	http.Error(response, "source chunk is not ready", http.StatusTooEarly)
}

func (s *session) acceptTargetReceipt(response http.ResponseWriter, request *http.Request, rawMember, rawIndex string) {
	memberID, index, target, err := s.authorizeTarget(request, rawMember, rawIndex)
	if err != nil {
		http.Error(response, err.Error(), http.StatusForbidden)
		return
	}
	var value targetResponse
	if err := decodeJSON(request, &value); err != nil {
		http.Error(response, "invalid target receipt", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	current := s.chunks[index]
	if current == nil {
		completed := s.completed[index]
		if completed == nil || !validTargetResponse(value, memberID, index, s.transfer, target, completed.source.RangeSHA256, s.now().UTC()) {
			s.mu.Unlock()
			http.Error(response, "target receipt does not match delivered bytes", http.StatusBadRequest)
			return
		}
		s.mu.Unlock()
		response.WriteHeader(http.StatusNoContent)
		return
	}
	if !validTargetResponse(value, memberID, index, s.transfer, target, current.source.RangeSHA256, s.now().UTC()) {
		s.mu.Unlock()
		http.Error(response, "target receipt does not match delivered bytes", http.StatusBadRequest)
		return
	}
	if value.FinalReceipt != nil {
		current.finals[memberID] = *value.FinalReceipt
	}
	current.targets[memberID] = value.RangeReceipt
	s.signalLocked()
	s.mu.Unlock()
	response.WriteHeader(http.StatusNoContent)
}

func (s *session) authorizeSource(request *http.Request, rawIndex string) (int64, error) {
	index, err := strconv.ParseInt(rawIndex, 10, 64)
	if err != nil || index < s.transfer.ChunkStart || index > s.transfer.ChunkEnd ||
		!s.now().UTC().Before(s.transfer.SourceLease.ExpiresAt) ||
		!secureEqual(request.Header.Get("X-Beam-Path-Token"), pathToken(s.transfer.SourceLease)) {
		return 0, errors.New("source path is not authorized")
	}
	return index, nil
}

func (s *session) authorizeTarget(request *http.Request, rawMember, rawIndex string) (string, int64, contracts.RoomTransferDestination, error) {
	memberID, err := url.PathUnescape(rawMember)
	if err != nil {
		return "", 0, contracts.RoomTransferDestination{}, errors.New("invalid target member")
	}
	index, err := strconv.ParseInt(rawIndex, 10, 64)
	if err != nil || index < s.transfer.ChunkStart || index > s.transfer.ChunkEnd {
		return "", 0, contracts.RoomTransferDestination{}, errors.New("invalid target chunk")
	}
	for _, target := range s.transfer.Targets {
		if target.MemberID == memberID && s.now().UTC().Before(target.Lease.ExpiresAt) &&
			secureEqual(request.Header.Get("X-Beam-Path-Token"), pathToken(target.Lease)) {
			return memberID, index, target, nil
		}
	}
	return "", 0, contracts.RoomTransferDestination{}, errors.New("target path is not authorized")
}

func (s *session) waitForSource(ctx context.Context, index int64) (*chunk, error) {
	for {
		s.mu.Lock()
		if s.failure != nil {
			failure := *s.failure
			s.mu.Unlock()
			return nil, &sourceFailureError{receipt: failure}
		}
		if current := s.chunks[index]; current != nil {
			s.mu.Unlock()
			return current, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-s.changed:
		}
	}
}

func (s *session) waitForTarget(ctx context.Context, index int64, memberID string) (targetResponse, error) {
	for {
		s.mu.Lock()
		current := s.chunks[index]
		if current != nil {
			if receipt, ok := current.targets[memberID]; ok {
				value := targetResponse{RangeReceipt: receipt}
				if final, ok := current.finals[memberID]; ok {
					copy := final
					value.FinalReceipt = &copy
				}
				s.mu.Unlock()
				return value, nil
			}
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return targetResponse{}, context.Cause(ctx)
		case <-s.changed:
		}
	}
}

func (s *session) release(index int64) {
	s.mu.Lock()
	if current := s.chunks[index]; current != nil {
		s.completed[index] = &completedChunk{source: current.source, targets: current.targets, finals: current.finals}
		delete(s.chunks, index)
		if index == s.expected {
			s.expected++
		}
		s.signalLocked()
	}
	s.mu.Unlock()
}

func validTargetResponse(value targetResponse, memberID string, index int64, transfer contracts.RoomTransfer,
	target contracts.RoomTransferDestination, sourceHash string, now time.Time) bool {
	if value.RangeReceipt.TargetMemberID != memberID || value.RangeReceipt.TransferID != transfer.TransferID ||
		value.RangeReceipt.LaneID != transfer.LaneID || value.RangeReceipt.ChunkIndex != index ||
		value.RangeReceipt.RangeSHA256 != sourceHash || value.RangeReceipt.LeaseID != target.Lease.LeaseID ||
		value.RangeReceipt.Verify(target.Lease.AgentPublicKey, now) != nil {
		return false
	}
	return value.FinalReceipt == nil || (value.FinalReceipt.TransferID == transfer.TransferID &&
		value.FinalReceipt.TargetMemberID == memberID && value.FinalReceipt.FileSizeBytes == transfer.FileSizeBytes &&
		value.FinalReceipt.Verify(target.Lease.AgentPublicKey, now) == nil)
}
func (s *session) signalLocked() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

type sourceFailureError struct {
	receipt contracts.SourceFailureReceipt
}

func (e *sourceFailureError) Error() string { return e.receipt.Code }

func sourceFailure(err error, index int64) []contracts.RoomFailure {
	failure := contracts.RoomFailure{Origin: "source_agent", Code: "source_disconnected", Retryable: true,
		ChunkIndices: []int64{index}, Detail: err.Error()}
	var signed *sourceFailureError
	if errors.As(err, &signed) {
		failure.Code, failure.Retryable, failure.SourceFailureReceipt = signed.receipt.Code, false, &signed.receipt
	}
	return []contracts.RoomFailure{failure}
}

func validateTransfer(transfer contracts.RoomTransfer, now time.Time) error {
	if transfer.SchemaVersion != contracts.RoomTransferSchemaVersion || transfer.BatchID == "" || transfer.RoomID == "" ||
		transfer.ChannelID == "" || transfer.PublicationID == "" || transfer.TransferID == "" || transfer.LaneID == "" ||
		transfer.SnapshotVersion == 0 || transfer.Attempt <= 0 || !transfer.Protection.Valid() {
		return errors.New("room transfer identity or schema is invalid")
	}
	if transfer.FileSizeBytes <= 0 || transfer.ChunkSizeBytes <= 0 || transfer.ChunkSizeBytes > maxChunkBytes ||
		transfer.ChunkCount != (transfer.FileSizeBytes+transfer.ChunkSizeBytes-1)/transfer.ChunkSizeBytes ||
		transfer.ChunkStart < 0 || transfer.ChunkEnd < transfer.ChunkStart || transfer.ChunkEnd >= transfer.ChunkCount {
		return errors.New("room transfer file or lane range is invalid")
	}
	if len(transfer.Targets) == 0 || len(transfer.Targets) > maxTargets {
		return errors.New("room transfer target count is invalid")
	}
	if err := transfer.SourceLease.Validate(contracts.TunnelLeaseRoleSourceRead, "", now); err != nil || pathToken(transfer.SourceLease) == "" {
		return fmt.Errorf("source lease: %w", err)
	}
	seen := make(map[string]bool, len(transfer.Targets))
	for _, target := range transfer.Targets {
		if target.MemberID == "" || seen[target.MemberID] {
			return errors.New("room transfer target identity is invalid")
		}
		seen[target.MemberID] = true
		if err := target.Lease.Validate(contracts.TunnelLeaseRoleTargetWrite, target.MemberID, now); err != nil || pathToken(target.Lease) == "" {
			return fmt.Errorf("target %s lease: %w", target.MemberID, err)
		}
	}
	return nil
}

func validProtectedChunk(payload []byte, protection contracts.RoomTransferProtection, plaintextLength int64) bool {
	return protection.Valid() && int64(len(payload)) == plaintextLength+protectedChunkOverhead && payload[0] == 1 &&
		binary.BigEndian.Uint64(payload[1:9]) == protection.KeyEpoch
}

func verifyCheckpoint(transfer contracts.RoomTransfer, resume checkpointValue, now time.Time) error {
	targets := make(map[string]contracts.RoomTransferDestination, len(transfer.Targets))
	for _, target := range transfer.Targets {
		targets[target.MemberID] = target
	}
	for key, receipt := range resume.TargetReceipts {
		target, ok := targets[receipt.TargetMemberID]
		if !ok || key != receiptKey(receipt.TargetMemberID, receipt.ChunkIndex) || receipt.Verify(target.Lease.AgentPublicKey, now) != nil {
			return errors.New("room transfer checkpoint contains invalid target evidence")
		}
	}
	return nil
}

func result(transfer contracts.RoomTransfer, resume checkpointValue, failures []contracts.RoomFailure, bytesProcessed int64) (domain.Result, error) {
	sources := make([]contracts.SourceRangeReceipt, 0, len(resume.SourceReceipts))
	for _, receipt := range resume.SourceReceipts {
		sources = append(sources, receipt)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ChunkIndex < sources[j].ChunkIndex })
	targets := make([]contracts.TargetRangeReceipt, 0, len(resume.TargetReceipts))
	for _, receipt := range resume.TargetReceipts {
		targets = append(targets, receipt)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].TargetMemberID == targets[j].TargetMemberID {
			return targets[i].ChunkIndex < targets[j].ChunkIndex
		}
		return targets[i].TargetMemberID < targets[j].TargetMemberID
	})
	finals := make([]contracts.FinalTargetReceipt, 0, len(resume.FinalReceipts))
	missing := make([]contracts.RoomMissingCells, 0)
	for _, target := range transfer.Targets {
		if final, ok := resume.FinalReceipts[target.MemberID]; ok {
			finals = append(finals, final)
		}
		entry := contracts.RoomMissingCells{TargetMemberID: target.MemberID}
		for index := transfer.ChunkStart; index <= transfer.ChunkEnd; index++ {
			if _, ok := resume.TargetReceipts[receiptKey(target.MemberID, index)]; !ok {
				entry.ChunkIndices = append(entry.ChunkIndices, index)
			}
		}
		if len(entry.ChunkIndices) > 0 {
			missing = append(missing, entry)
		}
	}
	outputs := map[string]string{}
	for key, value := range map[string]any{"source_receipts": sources, "target_receipts": targets,
		"final_target_receipts": finals, "missing": missing, "room_failures": failures} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return domain.Result{}, err
		}
		outputs[key] = string(encoded)
	}
	return domain.Result{BytesProcessed: bytesProcessed, Outputs: outputs}, nil
}

func initializeCheckpoint(value *checkpointValue) {
	if value.SourceReceipts == nil {
		value.SourceReceipts = map[int64]contracts.SourceRangeReceipt{}
	}
	if value.TargetReceipts == nil {
		value.TargetReceipts = map[string]contracts.TargetRangeReceipt{}
	}
	if value.FinalReceipts == nil {
		value.FinalReceipts = map[string]contracts.FinalTargetReceipt{}
	}
}

func saveCheckpoint(ctx context.Context, value checkpointValue, lastRange string) error {
	err := workloadcheckpoint.Save(ctx, contracts.RoomTransferSchemaVersion, map[string]string{
		"batch_id": value.BatchID, "transfer_id": value.TransferID, "lane_id": value.LaneID,
		"last_range": lastRange, "delivered_ranges": strconv.Itoa(len(value.TargetReceipts)),
	}, value)
	if errors.Is(err, workloadcheckpoint.ErrUnavailable) {
		return nil
	}
	return err
}

func reportRuntime(ctx context.Context, runtime contracts.DirectRoomTransferRuntime) {
	encoded, _ := json.Marshal(runtime)
	workloadprogress.Report(ctx, map[string]string{"phase": "ready", "room_transfer_runtime": string(encoded)})
}

func decodeTransfer(spec domain.Spec) (contracts.RoomTransfer, error) {
	var transfer contracts.RoomTransfer
	if err := json.Unmarshal(spec.Payload, &transfer); err != nil {
		return transfer, fmt.Errorf("decode room transfer: %w", err)
	}
	return transfer, nil
}

func decodeJSON(request *http.Request, destination any) error {
	return json.NewDecoder(io.LimitReader(request.Body, maxReceiptBytes+1)).Decode(destination)
}

func decodeHeader[T any](value string) (T, error) {
	var result T
	encoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxReceiptBytes {
		return result, errors.New("invalid receipt header")
	}
	err = json.Unmarshal(encoded, &result)
	return result, err
}

func advertisedURL(configured string, address net.Addr) (string, error) {
	_, port, err := net.SplitHostPort(address.String())
	if err != nil {
		return "", err
	}
	if configured == "" {
		return "http://127.0.0.1:" + port, nil
	}
	return strings.TrimRight(strings.ReplaceAll(configured, "{port}", port), "/"), nil
}

func pathToken(lease contracts.TunnelLease) string {
	for _, endpoint := range lease.Endpoints {
		if token := strings.TrimSpace(endpoint.Headers["X-Beam-Path-Token"]); token != "" {
			return token
		}
	}
	return ""
}

func bearer(request *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
}
func secureEqual(left, right string) bool {
	return left != "" && right != "" && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
func receiptKey(memberID string, index int64) string {
	return memberID + ":" + strconv.FormatInt(index, 10)
}
