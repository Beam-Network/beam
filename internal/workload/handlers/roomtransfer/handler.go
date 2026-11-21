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
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

const (
	maxReceiptBytes        = 64 << 10
	maxTargets             = 10_000
	protectedChunkOverhead = 1 + 8 + 16
)

type Config struct {
	ListenAddress        string
	AdvertiseURL         string
	StorageListenAddress string
	StorageAdvertiseURL  string
}

type Handler struct {
	config        Config
	storageClient *http.Client
	now           func() time.Time
	serverMu      sync.Mutex
	server        *sharedServer
	storageServer *sharedServer
}

type sharedServer struct {
	listener     net.Listener
	http         *http.Server
	baseURL      string
	certificates *workerCertificates
	mu           sync.RWMutex
	sessions     map[string]*session
	done         chan struct{}
	err          error
}

type session struct {
	transfer  contracts.RoomTransfer
	token     string
	now       func() time.Time
	mu        sync.Mutex
	chunks    map[int64]*chunk
	completed map[int64]*completedChunk
	expected  int64
	relay     *relayChunk
	failure   *contracts.SourceFailureReceipt
	// targetFailures holds each member's signed persist failure; that member's
	// delivery stops for this attempt while the other members continue.
	targetFailures  map[string]contracts.TargetFailureReceipt
	changed         chan struct{}
	notify          chan struct{}
	closed          chan struct{}
	hold            time.Duration
	sourceExpiresAt atomic.Int64
	targetExpiresAt map[string]*atomic.Int64
	reportExpiry    func(time.Time)
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
	SourceReads    map[int64]contracts.SourceReadEvidence  `json:"source_reads,omitempty"`
	BatchID        string                                  `json:"batch_id"`
	TransferID     string                                  `json:"transfer_id"`
	LaneID         string                                  `json:"lane_id"`
	SourceReceipts map[int64]contracts.SourceRangeReceipt  `json:"source_receipts"`
	TargetReceipts map[string]contracts.TargetRangeReceipt `json:"target_receipts"`
	FinalReceipts  map[string]contracts.FinalTargetReceipt `json:"final_receipts"`
	StorageResults map[string]contracts.StorageRangeResult `json:"storage_results,omitempty"`
}

type targetResponse struct {
	RangeReceipt contracts.TargetRangeReceipt  `json:"range_receipt"`
	FinalReceipt *contracts.FinalTargetReceipt `json:"final_receipt,omitempty"`
}

func NewHandler(config Config) *Handler {
	if strings.TrimSpace(config.ListenAddress) == "" {
		config.ListenAddress = "127.0.0.1:0"
	}
	return &Handler{config: config, now: time.Now, storageClient: storageHTTPClient()}
}

func (*Handler) Kind() domain.Kind { return domain.KindRoomTransfer }

func (h *Handler) Validate(spec domain.Spec) error {
	transfer, err := decodeTransfer(spec)
	if err != nil {
		return err
	}
	needed := contracts.RoomBufferMemoryBytes(transfer.FileSizeBytes, transfer.ChunkSizeBytes)
	if needed == 0 || spec.Resources.MemoryBytes < needed {
		return errors.New("room source buffer exceeds reserved worker memory")
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
	if transfer.Protection.Storage() {
		parsed, err := url.Parse(strings.ReplaceAll(h.config.StorageAdvertiseURL, "{port}", "1"))
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("hybrid room transfers require an HTTPS worker listener")
		}
		if h.config.StorageListenAddress != "" {
			if _, _, err := net.SplitHostPort(h.config.StorageListenAddress); err != nil {
				return errors.New("hybrid worker listen address must be host:port")
			}
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
	if _, ok, checkpointErr := workloadcheckpoint.Current(ctx, transfer.SchemaVersion, &resume); checkpointErr != nil {
		return domain.Result{}, checkpointErr
	} else if ok && (resume.BatchID != transfer.BatchID || resume.TransferID != transfer.TransferID || resume.LaneID != transfer.LaneID) {
		return domain.Result{}, errors.New("room transfer checkpoint belongs to another assignment")
	}
	initializeCheckpoint(&resume)
	if err := verifyCheckpoint(transfer, resume, h.now().UTC()); err != nil {
		return domain.Result{}, err
	}

	shared, err := h.sharedServer(transfer.Protection.Storage())
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
		targetFailures: make(map[string]contracts.TargetFailureReceipt), expected: transfer.ChunkStart, changed: make(chan struct{}, 1),
		closed: make(chan struct{})}
	active.restoreCompleted(resume)
	active.trackLeases()
	if err := shared.add(sessionID, active); err != nil {
		return domain.Result{}, err
	}
	defer shared.remove(sessionID)
	defer close(active.closed)
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
	if transfer.Protection.Storage() {
		if shared.certificates == nil {
			return domain.Result{}, errors.New("hybrid room assignment requires TLS")
		}
		fingerprint, err := shared.certificates.ForLease(expiresAt)
		if err != nil {
			return domain.Result{}, err
		}
		directRuntime.Capability = contracts.RoomStorageCapability
		directRuntime.Transport = "worker_https"
		directRuntime.TLSCertificateSHA256 = fingerprint
	}
	reportRuntime(ctx, directRuntime)
	active.reportExpiry = func(expiresAt time.Time) {
		next := directRuntime
		next.ExpiresAt = expiresAt
		if next.Capability == contracts.RoomStorageCapability {
			fingerprint, err := shared.certificates.ForLease(expiresAt)
			if err != nil {
				return
			}
			next.TLSCertificateSHA256 = fingerprint
		}
		reportRuntime(ctx, next)
	}

	checkpoints := newLaneCheckpoints(ctx, transfer.SchemaVersion, resume)
	defer checkpoints.close()
	readAhead := transfer.SourceLease.Storage != nil &&
		spec.Resources.MemoryBytes >= contracts.RoomStorageSourceMemoryBytes(transfer.FileSizeBytes, transfer.ChunkSizeBytes)
	var ahead *sourceAhead
	defer func() { ahead.stop() }()
	var bytesProcessed int64
	for chunkIndex := transfer.ChunkStart; chunkIndex <= transfer.ChunkEnd; chunkIndex++ {
		if allTargetsDelivered(resume, transfer.Targets, chunkIndex) {
			continue
		}
		if active.allTargetsFailed() {
			break
		}
		if transfer.SourceLease.Storage != nil {
			payload, evidence, err := h.sourceChunk(ctx, active, spec.Identity.WorkerID, chunkIndex, ahead)
			ahead = nil
			if next, ok := nextUndelivered(resume, transfer, chunkIndex+1); ok && err == nil {
				ahead = h.startSourceAhead(ctx, active, spec.Identity.WorkerID, next, readAhead)
			}
			if err != nil {
				return result(transfer, resume, active.withTargetFailures([]contracts.RoomFailure{{Origin: "storage_provider", Code: storageFailureCode(err), Retryable: storageFailureCode(err) != "room_storage_source_mutated", ChunkIndices: []int64{chunkIndex}, Detail: storageFailureClass(err)}}), bytesProcessed)
			}
			resume.StorageResults[storageResultKey(evidence)] = evidence
			checkpoints.queue(recordStorageResult(evidence))
			active.mu.Lock()
			active.chunks[chunkIndex] = &chunk{payload: payload,
				source: contracts.SourceRangeReceipt{TransferID: transfer.TransferID, LaneID: transfer.LaneID, ChunkIndex: chunkIndex,
					Offset: evidence.Offset, Length: evidence.Length, RangeSHA256: evidence.RangeSHA256},
				targets: make(map[string]contracts.TargetRangeReceipt), finals: make(map[string]contracts.FinalTargetReceipt)}
			active.signalLocked()
			active.mu.Unlock()
		}
		current, waitErr := active.waitForSource(ctx, chunkIndex)
		if waitErr != nil {
			return result(transfer, resume, active.withTargetFailures(sourceFailure(waitErr, chunkIndex)), bytesProcessed)
		}
		if transfer.SourceLease.Storage == nil {
			source := current.source
			resume.SourceReceipts[chunkIndex] = source
			checkpoints.queue(func(value *checkpointValue) { value.SourceReceipts[chunkIndex] = source })
		}
		bytesProcessed += current.source.Length
		read := resume.SourceReads[chunkIndex]
		read.ChunkIndex = chunkIndex
		read.ReadCount++
		read.PayloadBytes += current.source.Length
		read.WireBytes += int64(len(current.payload))
		resume.SourceReads[chunkIndex] = read
		checkpoints.queue(func(value *checkpointValue) { value.SourceReads[chunkIndex] = read })
		failures := h.deliverChunk(ctx, spec.Identity.WorkerID, active, current, chunkIndex, &resume, checkpoints)
		if err := checkpoints.failure(); err != nil {
			return domain.Result{}, err
		}
		if len(failures) != 0 {
			return result(transfer, resume, active.withTargetFailures(failures), bytesProcessed)
		}
		active.release(chunkIndex)
	}
	return result(transfer, resume, active.withTargetFailures(nil), bytesProcessed)
}

// PrepareStorageListener binds the TLS endpoint before the worker advertises
// hybrid capability. Configuration or port conflicts fail startup closed.
func (h *Handler) PrepareStorageListener() error {
	parsed, err := url.Parse(strings.ReplaceAll(h.config.StorageAdvertiseURL, "{port}", "1"))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("hybrid room transfers require an HTTPS worker listener")
	}
	_, err = h.sharedServer(true)
	return err
}

func (h *Handler) Close() error {
	h.serverMu.Lock()
	defer h.serverMu.Unlock()
	var failures []error
	for _, server := range []*sharedServer{h.server, h.storageServer} {
		if server != nil {
			failures = append(failures, server.http.Close())
		}
	}
	return errors.Join(failures...)
}

func (h *Handler) sharedServer(storage bool) (*sharedServer, error) {
	h.serverMu.Lock()
	defer h.serverMu.Unlock()
	server := h.server
	listen, advertise := h.config.ListenAddress, h.config.AdvertiseURL
	if storage {
		server, listen, advertise = h.storageServer, h.config.StorageListenAddress, h.config.StorageAdvertiseURL
	}
	if server != nil {
		select {
		case <-server.done:
			return nil, server.failure()
		default:
			return server, nil
		}
	}
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	baseURL, err := advertisedURL(advertise, listener.Addr())
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	shared := &sharedServer{listener: listener, baseURL: baseURL, sessions: make(map[string]*session), done: make(chan struct{})}
	if storage {
		shared.listener, shared.certificates = secureWorkerListener(listener, h.now)
	}
	shared.http = &http.Server{Handler: shared, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Minute, WriteTimeout: 30 * time.Minute, IdleTimeout: time.Minute}
	if storage {
		h.storageServer = shared
	} else {
		h.server = shared
	}
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
	if len(parts) == 4 && parts[0] == "targets" && parts[2] == "failures" && request.Method == http.MethodPost {
		s.acceptTargetFailure(response, request, parts[1], parts[3])
		return
	}
	http.NotFound(response, request)
}

func (s *session) acceptSource(response http.ResponseWriter, request *http.Request, rawIndex string) {
	index, err := s.authorizeSource(request, rawIndex)
	if err != nil {
		refuseUnreadBody(response, err.Error(), http.StatusForbidden)
		return
	}
	offset, length := contracts.ChunkRange(s.transfer.FileSizeBytes, s.transfer.ChunkSizeBytes, index)
	receipt, err := decodeHeader[contracts.SourceRangeReceipt](request.Header.Get("X-Beam-Source-Receipt"))
	reason := ""
	if err != nil {
		reason = "decode"
	} else if receipt.TransferID != s.transfer.TransferID {
		reason = "transfer_id"
	} else if receipt.LaneID != s.transfer.LaneID {
		reason = "lane_id"
	} else if receipt.ChunkIndex != index {
		reason = "chunk_index"
	} else if receipt.Offset != offset {
		reason = "offset"
	} else if receipt.Length != length {
		reason = "length"
	} else if receipt.LeaseID != s.transfer.SourceLease.LeaseID {
		reason = "lease_id"
	} else if verifyErr := receipt.Verify(s.transfer.SourceLease.AgentPublicKey, s.now().UTC()); verifyErr != nil {
		reason = "signature_or_public_key"
	}
	if reason != "" {
		log.Printf("room transfer rejected source receipt: reason=%s transfer_id=%s lane_id=%s chunk=%d expected_key=%s receipt_key=%s",
			reason, s.transfer.TransferID, s.transfer.LaneID, index, publicKeyFingerprint(s.transfer.SourceLease.AgentPublicKey), publicKeyFingerprint(receipt.AgentPublicKey))
		refuseUnreadBody(response, "invalid signed source receipt", http.StatusBadRequest)
		return
	}
	wireLength := length
	if s.transfer.Protection.Valid() {
		wireLength += protectedChunkOverhead
	}
	hold := time.NewTimer(s.holdLimit())
	defer hold.Stop()
	for {
		s.mu.Lock()
		stored := s.chunks[index]
		if completed := s.completed[index]; completed != nil {
			stored = &chunk{source: completed.source}
		}
		if stored != nil {
			s.mu.Unlock()
			if stored.source.RangeSHA256 != receipt.RangeSHA256 {
				refuseUnreadBody(response, "source retry hash mismatch", http.StatusConflict)
				return
			}
			refuseUnreadBody(response, "", http.StatusNoContent)
			return
		}
		if index == s.expected && s.relay == nil {
			relay := newRelayChunk(index, receipt, wireLength)
			s.relay = relay
			s.signalLocked()
			s.mu.Unlock()
			s.receiveSource(response, request, relay, length)
			return
		}
		wait := s.waitLocked()
		s.mu.Unlock()
		select {
		case <-wait:
		case <-request.Context().Done():
			return
		case <-s.closed:
			refuseUnreadBody(response, "room transfer session ended", http.StatusNotFound)
			return
		case <-hold.C:
			refuseUnreadBody(response, "source chunk is not ready", http.StatusTooEarly)
			return
		}
	}
}

func (s *session) receiveSource(response http.ResponseWriter, request *http.Request, relay *relayChunk, plaintextLength int64) {
	verified := relay.receive(request.Body, s.transfer.Protection, plaintextLength)
	s.mu.Lock()
	s.relay = nil
	if verified {
		s.chunks[relay.index] = &chunk{payload: relay.payload, source: relay.receipt,
			targets: make(map[string]contracts.TargetRangeReceipt), finals: make(map[string]contracts.FinalTargetReceipt)}
	}
	relay.finish(verified)
	s.signalLocked()
	s.mu.Unlock()
	if !verified {
		http.Error(response, "invalid protected source chunk", http.StatusBadRequest)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func refuseUnreadBody(response http.ResponseWriter, message string, status int) {
	response.Header().Set("Connection", "close")
	if message == "" {
		response.WriteHeader(status)
		return
	}
	http.Error(response, message, status)
}

func publicKeyFingerprint(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:6])
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
	hold := time.NewTimer(s.holdLimit())
	defer hold.Stop()
	for {
		s.mu.Lock()
		if current := s.chunks[index]; current != nil {
			// The immutable chunk remains alive while Write holds this slice.
			payload := current.payload
			digest := current.source.RangeSHA256
			s.mu.Unlock()
			writeChunkHeaders(response, len(payload), digest, memberID)
			_, _ = response.Write(payload)
			return
		}
		if relay := s.relay; relay != nil && relay.index == index {
			s.mu.Unlock()
			relay.stream(response, request, memberID)
			return
		}
		if s.completed[index] != nil || index < s.expected {
			s.mu.Unlock()
			http.Error(response, "source chunk is not ready", http.StatusTooEarly)
			return
		}
		wait := s.waitLocked()
		s.mu.Unlock()
		select {
		case <-wait:
		case <-request.Context().Done():
			return
		case <-s.closed:
			http.Error(response, "room transfer session ended", http.StatusNotFound)
			return
		case <-hold.C:
			http.Error(response, "source chunk is not ready", http.StatusTooEarly)
			return
		}
	}
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

// acceptTargetFailure records a target agent's signed persist failure. Only the
// first receipt per member counts; that member receives nothing further.
func (s *session) acceptTargetFailure(response http.ResponseWriter, request *http.Request, rawMember, rawIndex string) {
	memberID, index, target, err := s.authorizeTarget(request, rawMember, rawIndex)
	if err != nil {
		http.Error(response, err.Error(), http.StatusForbidden)
		return
	}
	var receipt contracts.TargetFailureReceipt
	if err := decodeJSON(request, &receipt); err != nil || target.Lease.Storage != nil || receipt.TransferID != s.transfer.TransferID ||
		receipt.LaneID != s.transfer.LaneID || receipt.ChunkIndex != index || receipt.TargetMemberID != memberID ||
		receipt.LeaseID != target.Lease.LeaseID || receipt.Verify(target.Lease.AgentPublicKey, s.now().UTC()) != nil {
		http.Error(response, "invalid signed target failure", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if _, exists := s.targetFailures[memberID]; !exists {
		s.targetFailures[memberID] = receipt
		s.signalLocked()
	}
	s.mu.Unlock()
	response.WriteHeader(http.StatusNoContent)
}

func (s *session) authorizeSource(request *http.Request, rawIndex string) (int64, error) {
	index, err := strconv.ParseInt(rawIndex, 10, 64)
	if err != nil || index < s.transfer.ChunkStart || index > s.transfer.ChunkEnd ||
		!s.now().UTC().Before(s.sourceExpiry()) ||
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
		if target.MemberID == memberID && s.now().UTC().Before(s.targetExpiry(memberID)) &&
			secureEqual(request.Header.Get("X-Beam-Path-Token"), pathToken(target.Lease)) {
			return memberID, index, target, nil
		}
	}
	return "", 0, contracts.RoomTransferDestination{}, errors.New("target path is not authorized")
}

func (s *session) trackLeases() {
	s.sourceExpiresAt.Store(s.transfer.SourceLease.ExpiresAt.UnixNano())
	s.targetExpiresAt = make(map[string]*atomic.Int64, len(s.transfer.Targets))
	for _, target := range s.transfer.Targets {
		expiry := &atomic.Int64{}
		expiry.Store(target.Lease.ExpiresAt.UnixNano())
		s.targetExpiresAt[target.MemberID] = expiry
	}
}

func (s *session) current() contracts.RoomTransfer {
	transfer := s.transfer
	transfer.SourceLease.ExpiresAt = s.sourceExpiry()
	transfer.Targets = append([]contracts.RoomTransferDestination(nil), s.transfer.Targets...)
	for index := range transfer.Targets {
		transfer.Targets[index].Lease.ExpiresAt = s.targetExpiry(transfer.Targets[index].MemberID)
	}
	return transfer
}

func (s *session) sourceExpiry() time.Time {
	if expiry := s.sourceExpiresAt.Load(); expiry != 0 {
		return time.Unix(0, expiry).UTC()
	}
	return s.transfer.SourceLease.ExpiresAt
}

func (s *session) targetExpiry(memberID string) time.Time {
	if expiry := s.targetExpiresAt[memberID]; expiry != nil {
		return time.Unix(0, expiry.Load()).UTC()
	}
	for _, target := range s.transfer.Targets {
		if target.MemberID == memberID {
			return target.Lease.ExpiresAt
		}
	}
	return time.Time{}
}

func (s *session) extend(transfer contracts.RoomTransfer, expiresAt time.Time) {
	s.sourceExpiresAt.Store(transfer.SourceLease.ExpiresAt.UnixNano())
	for _, target := range transfer.Targets {
		if expiry := s.targetExpiresAt[target.MemberID]; expiry != nil {
			expiry.Store(target.Lease.ExpiresAt.UnixNano())
		}
	}
	if s.reportExpiry != nil {
		s.reportExpiry(expiresAt)
	}
}

func (h *Handler) ExtendLease(spec domain.Spec, extension domain.Extension) (domain.Spec, error) {
	transfer, err := decodeTransfer(spec)
	if err != nil {
		return domain.Spec{}, err
	}
	extend := func(lease *contracts.TunnelLease) {
		if expiresAt, ok := extension.LeaseExpiries[lease.LeaseID]; ok && expiresAt.After(lease.ExpiresAt) {
			lease.ExpiresAt = expiresAt.UTC()
		}
	}
	extend(&transfer.SourceLease)
	expiresAt := transfer.SourceLease.ExpiresAt
	for index := range transfer.Targets {
		extend(&transfer.Targets[index].Lease)
		if transfer.Targets[index].Lease.ExpiresAt.Before(expiresAt) {
			expiresAt = transfer.Targets[index].Lease.ExpiresAt
		}
	}
	if extension.AssignmentExpiresAt.Before(expiresAt) {
		expiresAt = extension.AssignmentExpiresAt
	}
	if !expiresAt.After(spec.Lease.AssignmentExpiresAt) {
		return domain.Spec{}, errors.New("room lane lease extension extends no lease of the lane")
	}
	payload, err := json.Marshal(transfer)
	if err != nil {
		return domain.Spec{}, err
	}
	spec.Payload, spec.Lease.AssignmentExpiresAt = payload, expiresAt
	if live := h.session(fmt.Sprintf("%s-%s-%d", transfer.TransferID, transfer.LaneID, transfer.Attempt)); live != nil {
		live.extend(transfer, expiresAt)
	}
	return spec, nil
}

func (h *Handler) session(id string) *session {
	h.serverMu.Lock()
	server := h.server
	h.serverMu.Unlock()
	if server == nil {
		return nil
	}
	server.mu.RLock()
	defer server.mu.RUnlock()
	return server.sessions[id]
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

func (s *session) release(index int64) {
	s.mu.Lock()
	if current := s.chunks[index]; current != nil {
		s.completed[index] = &completedChunk{source: current.source, targets: current.targets, finals: current.finals}
		delete(s.chunks, index)
		if index == s.expected {
			s.expected++
			for s.completed[s.expected] != nil {
				s.expected++
			}
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
	if s.notify != nil {
		close(s.notify)
		s.notify = nil
	}
}

func (s *session) holdLimit() time.Duration {
	if s.hold > 0 {
		return s.hold
	}
	return requestHoldLimit
}

func (s *session) waitLocked() <-chan struct{} {
	if s.notify == nil {
		s.notify = make(chan struct{})
	}
	return s.notify
}

type sourceFailureError struct {
	receipt contracts.SourceFailureReceipt
}

func (e *sourceFailureError) Error() string { return e.receipt.Code }

func (s *session) targetFailed(memberID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, failed := s.targetFailures[memberID]
	return failed
}

func (s *session) allTargetsFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, target := range s.transfer.Targets {
		if _, failed := s.targetFailures[target.MemberID]; !failed {
			return false
		}
	}
	return true
}

// withTargetFailures reports each signed target failure once, replacing any
// failure the Worker inferred for the same member.
func (s *session) withTargetFailures(failures []contracts.RoomFailure) []contracts.RoomFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.targetFailures) == 0 {
		return failures
	}
	result := make([]contracts.RoomFailure, 0, len(failures)+len(s.targetFailures))
	for _, failure := range failures {
		if _, signed := s.targetFailures[failure.TargetMemberID]; !signed {
			result = append(result, failure)
		}
	}
	members := make([]string, 0, len(s.targetFailures))
	for memberID := range s.targetFailures {
		members = append(members, memberID)
	}
	sort.Strings(members)
	for _, memberID := range members {
		receipt := s.targetFailures[memberID]
		result = append(result, contracts.RoomFailure{Origin: "target_agent", Code: receipt.Code, Retryable: true,
			TargetMemberID: memberID, ChunkIndices: []int64{receipt.ChunkIndex}, TargetFailureReceipt: &receipt})
	}
	return result
}

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
	validProtection := transfer.SchemaVersion == contracts.RoomTransferSchemaVersion && transfer.Protection.Valid()
	if transfer.SchemaVersion == contracts.RoomStorageSchemaVersion {
		validProtection = transfer.Protection.Storage()
	}
	if !validProtection || transfer.BatchID == "" || transfer.RoomID == "" ||
		transfer.ChannelID == "" || transfer.PublicationID == "" || transfer.TransferID == "" || transfer.LaneID == "" ||
		transfer.SnapshotVersion == 0 || transfer.Attempt <= 0 {
		return errors.New("room transfer identity or schema is invalid")
	}
	if transfer.FileSizeBytes <= 0 || transfer.ChunkSizeBytes <= 0 ||
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
	hasStorage := transfer.SourceLease.Storage != nil
	for _, target := range transfer.Targets {
		hasStorage = hasStorage || target.Lease.Storage != nil
		if target.MemberID == "" || seen[target.MemberID] {
			return errors.New("room transfer target identity is invalid")
		}
		seen[target.MemberID] = true
		if err := target.Lease.Validate(contracts.TunnelLeaseRoleTargetWrite, target.MemberID, now); err != nil || pathToken(target.Lease) == "" {
			return fmt.Errorf("target %s lease: %w", target.MemberID, err)
		}
	}
	if hasStorage != transfer.Protection.Storage() {
		return errors.New("room protection does not match its endpoint snapshot")
	}
	for _, lease := range append([]contracts.TunnelLease{transfer.SourceLease}, targetLeases(transfer.Targets)...) {
		expected := contracts.RoomTransferDirectCapability
		if transfer.Protection.Storage() {
			expected = contracts.RoomStorageCapability
		}
		if lease.Protocol != expected {
			return errors.New("hybrid assignment contains a non-hybrid lease")
		}
	}
	return nil
}

func validProtectedChunk(payload []byte, protection contracts.RoomTransferProtection, plaintextLength int64) bool {
	if protection.Storage() {
		return int64(len(payload)) == plaintextLength
	}
	return protection.Valid() && int64(len(payload)) == plaintextLength+protectedChunkOverhead && payload[0] == 1 &&
		binary.BigEndian.Uint64(payload[1:9]) == protection.KeyEpoch
}

func verifyCheckpoint(transfer contracts.RoomTransfer, resume checkpointValue, now time.Time) error {
	for index, read := range resume.SourceReads {
		if read.ChunkIndex != index || read.Validate(transfer.FileSizeBytes, transfer.ChunkSizeBytes, transfer.ChunkStart, transfer.ChunkEnd, transfer.Protection) != nil {
			return errors.New("source read checkpoint does not match assignment")
		}
	}
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
	for key, evidence := range resume.StorageResults {
		lease := transfer.SourceLease
		if evidence.Role == contracts.TunnelLeaseRoleTargetWrite {
			target, ok := targets[evidence.MemberID]
			if !ok {
				return errors.New("storage checkpoint target is outside the assignment")
			}
			lease = target.Lease
		}
		offset, length := storageRange(transfer, evidence.ChunkIndex)
		if lease.Storage == nil || evidence.Role != lease.Role || evidence.MemberID != lease.Storage.MemberID ||
			key != storageResultKey(evidence) || evidence.LeaseID != lease.LeaseID || evidence.ChunkIndex < transfer.ChunkStart ||
			evidence.ChunkIndex > transfer.ChunkEnd || evidence.Offset != offset || evidence.Length != length ||
			evidence.CompletedAt.IsZero() || evidence.CompletedAt.After(now.Add(time.Minute)) || len(evidence.RangeSHA256) != 64 ||
			(evidence.Role == contracts.TunnelLeaseRoleTargetWrite && (evidence.UploadID == "" || evidence.ETag == "" || evidence.PartNumber != contracts.MultipartPartNumber(evidence.ChunkIndex))) {
			return errors.New("storage checkpoint evidence does not match the assignment")
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
			if !targetDelivered(resume, target, index) {
				entry.ChunkIndices = append(entry.ChunkIndices, index)
			}
		}
		if len(entry.ChunkIndices) > 0 {
			missing = append(missing, entry)
		}
	}
	outputs := map[string]string{}
	sourceReads := make([]contracts.SourceReadEvidence, 0, len(resume.SourceReads))
	for _, read := range resume.SourceReads {
		sourceReads = append(sourceReads, read)
	}
	sort.Slice(sourceReads, func(i, j int) bool { return sourceReads[i].ChunkIndex < sourceReads[j].ChunkIndex })
	storageResults := make([]contracts.StorageRangeResult, 0, len(resume.StorageResults))
	for _, evidence := range resume.StorageResults {
		storageResults = append(storageResults, evidence)
	}
	sort.Slice(storageResults, func(i, j int) bool { return storageResultKey(storageResults[i]) < storageResultKey(storageResults[j]) })
	for key, value := range map[string]any{"source_reads": sourceReads, "source_receipts": sources, "target_receipts": targets,
		"final_target_receipts": finals, "missing": missing, "room_failures": failures, "storage_results": storageResults} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return domain.Result{}, err
		}
		outputs[key] = string(encoded)
	}
	return domain.Result{BytesProcessed: bytesProcessed, Outputs: outputs}, nil
}

func initializeCheckpoint(value *checkpointValue) {
	if value.SourceReads == nil {
		value.SourceReads = map[int64]contracts.SourceReadEvidence{}
	}
	if value.StorageResults == nil {
		value.StorageResults = map[string]contracts.StorageRangeResult{}
	}
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

func saveCheckpoint(ctx context.Context, schema string, value checkpointValue, lastRange string) error {
	err := workloadcheckpoint.Save(ctx, schema, map[string]string{
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
