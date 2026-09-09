package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/orchestrator/scheduling"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

type Control interface {
	Offer(context.Context, string, domain.Spec) (runtime.Decision, error)
	Commit(context.Context, string, domain.Commit) error
	Cancel(context.Context, string, string, string) error
	Connected(string) bool
	UpsertCircuit(context.Context, circuit.Plan) error
	RevokeCircuit(context.Context, circuit.Revocation) error
}

type ResultSink interface {
	DeliverResult(context.Context, Record, domain.Result) error
}

// TerminalDeliveryError records a final upstream disposition that must not be
// replayed. The error text remains on the durable dispatch record for operator
// visibility even though delivery has reached a terminal state.
type TerminalDeliveryError struct {
	cause error
}

func (e *TerminalDeliveryError) Error() string { return e.cause.Error() }
func (e *TerminalDeliveryError) Unwrap() error { return e.cause }

func TerminalDelivery(cause error) error {
	if cause == nil {
		return nil
	}
	return &TerminalDeliveryError{cause: cause}
}

// ProgressSink is optional. Existing dispatch sinks remain result-only, while
// composite workloads may validate and persist workload-specific progress.
type ProgressSink interface {
	DeliverProgress(context.Context, Record, domain.Progress) error
}

type Config struct {
	OrchestratorID    string
	AssignmentTTL     time.Duration
	MaxObservationAge time.Duration
	Now               func() time.Time
}

type Service struct {
	config      Config
	registry    *registry.Registry
	scheduler   *scheduling.Scheduler
	control     Control
	store       Store
	recordLocks [64]sync.Mutex

	mu          sync.RWMutex
	sinks       map[Source]ResultSink
	subscribers map[uint64]chan Event
	nextSub     uint64
	sequence    atomic.Uint64
}

func NewService(config Config, orchestratorRegistry *registry.Registry, control Control, store Store) (*Service, error) {
	if config.OrchestratorID == "" || orchestratorRegistry == nil || control == nil || store == nil {
		return nil, errors.New("orchestrator_id, registry, WCP control, and orchestration store are required")
	}
	if config.AssignmentTTL <= 0 {
		config.AssignmentTTL = time.Hour
	}
	if config.MaxObservationAge <= 0 {
		config.MaxObservationAge = 30 * time.Second
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Service{config: config, registry: orchestratorRegistry, scheduler: scheduling.New(orchestratorRegistry),
		control: control, store: store, sinks: make(map[Source]ResultSink), subscribers: make(map[uint64]chan Event)}, nil
}

func (s *Service) RegisterSink(source Source, sink ResultSink) {
	s.mu.Lock()
	if sink == nil {
		delete(s.sinks, source)
	} else {
		s.sinks[source] = sink
	}
	s.mu.Unlock()
}

func (s *Service) Dispatch(ctx context.Context, request DispatchRequest) (Record, error) {
	if request.ExternalID == "" || request.Spec.WorkloadID == "" || request.Spec.AttemptID == "" {
		return Record{}, errors.New("external id and workload identity are required")
	}
	key := request.Spec.Key()
	lock := s.recordLock(key)
	lock.Lock()
	defer lock.Unlock()
	var record Record
	if existing, ok := s.store.Get(key); ok {
		if existing.Source != request.Source || existing.ExternalID != request.ExternalID {
			return Record{}, errors.New("workload idempotency key belongs to another external task")
		}
		if existing.State == StateRejected || existing.State == StateFailed || existing.State == StateCancelled {
			return existing, fmt.Errorf("external task is already terminal: %s", existing.UpstreamError)
		}
		if existing.State == StateCommitted || existing.State == StateRunning || existing.State == StateCompleted {
			return existing, nil
		}
		record = existing
	} else {
		now := s.config.Now().UTC()
		record = Record{Source: request.Source, ExternalID: request.ExternalID, WorkloadKey: key,
			State: StateReceived, Spec: request.Spec, CreatedAt: now, UpdatedAt: now}
		if err := s.save(record, "external task received"); err != nil {
			return Record{}, err
		}
	}

	workerID := record.WorkerID
	nodeID := record.Spec.Identity.NodeID
	if request.WorkerID != "" {
		if workerID != "" && workerID != request.WorkerID {
			return record, errors.New("durable workload is already assigned to another Worker")
		}
		workerID = request.WorkerID
		nodeID = request.NodeID
	}
	if workerID == "" {
		now := s.config.Now().UTC()
		placement, err := s.scheduler.Select(scheduling.Request{RequiredCapabilities: record.Spec.RequiredCapabilities,
			Resources: record.Spec.Resources, MaxObservationAge: s.config.MaxObservationAge}, now)
		if err != nil {
			return s.terminal(record, StateRejected, "no compatible connected Worker: "+err.Error(), err)
		}
		workerID = placement.WorkerID
		nodeID = placement.NodeID
	}
	if !s.control.Connected(workerID) {
		return record, errors.New("selected Worker is disconnected; dispatch will be retried")
	}
	record.WorkerID = workerID
	record.Spec.Identity = domain.Identity{OrchestratorID: s.config.OrchestratorID, WorkerID: workerID, NodeID: nodeID}
	record.State = StateOffered
	record.UpstreamError = ""
	record.UpdatedAt = s.config.Now().UTC()
	if err := s.save(record, "Orchestrator selected Worker and sent workload offer"); err != nil {
		return record, err
	}
	decision, err := s.control.Offer(ctx, workerID, record.Spec)
	if err != nil {
		record.UpstreamError = "WCP offer failed: " + err.Error()
		record.UpdatedAt = s.config.Now().UTC()
		_ = s.save(record, "WCP offer failed; dispatch will be retried")
		return record, err
	}
	if !decision.Accepted {
		reason := decision.Reason
		if reason == "" {
			reason = "Worker rejected workload"
		}
		return s.terminal(record, StateRejected, reason, fmt.Errorf("Worker admission rejected: %s", reason))
	}
	record.State = StateReserved
	record.UpdatedAt = s.config.Now().UTC()
	if err := s.save(record, "Worker reserved resources"); err != nil {
		return record, err
	}
	if request.BeforeCommit != nil {
		if err := request.BeforeCommit(ctx, record); err != nil {
			_ = s.control.Cancel(context.Background(), record.WorkerID, record.Spec.WorkloadID, record.Spec.AttemptID)
			return s.terminal(record, StateCancelled, "external authority refused commit: "+err.Error(), err)
		}
	}
	token, err := randomToken(24)
	if err != nil {
		return record, err
	}
	expiresAt := s.config.Now().Add(s.config.AssignmentTTL).UTC()
	if !record.Spec.Lease.AssignmentExpiresAt.IsZero() && record.Spec.Lease.AssignmentExpiresAt.Before(expiresAt) {
		expiresAt = record.Spec.Lease.AssignmentExpiresAt
	}
	commit := domain.Commit{WorkloadID: record.Spec.WorkloadID, AttemptID: record.Spec.AttemptID,
		PlanVersion: 1, AssignmentToken: token, AssignmentExpiresAt: expiresAt}
	if err := s.control.Commit(ctx, record.WorkerID, commit); err != nil {
		_ = s.control.Cancel(context.Background(), record.WorkerID, record.Spec.WorkloadID, record.Spec.AttemptID)
		record.UpstreamError = "WCP commit failed: " + err.Error()
		record.UpdatedAt = s.config.Now().UTC()
		_ = s.save(record, "WCP commit failed; dispatch will be retried")
		return record, err
	}
	record.State = StateCommitted
	record.AssignmentExpiresAt = expiresAt
	record.Spec.Lease.AssignmentExpiresAt = expiresAt
	record.UpdatedAt = s.config.Now().UTC()
	if err := s.save(record, "workload committed over WCP"); err != nil {
		return record, err
	}
	return record, nil
}

func (s *Service) HandleProgress(progress domain.Progress) error {
	return s.HandleProgressContext(context.Background(), progress)
}

func (s *Service) HandleProgressContext(ctx context.Context, progress domain.Progress) error {
	key := progress.WorkloadID + "/" + progress.AttemptID
	lock := s.recordLock(key)
	lock.Lock()
	defer lock.Unlock()
	record, ok := s.store.Get(key)
	if !ok {
		return errors.New("progress does not match an external task")
	}
	if record.State == StateCompleted || record.State == StateFailed || record.State == StateCancelled || record.State == StateRejected {
		return nil
	}
	record.State = StateRunning
	record.Progress = &progress
	record.UpdatedAt = s.config.Now().UTC()
	if err := s.save(record, "Worker reported progress"); err != nil {
		return err
	}
	s.mu.RLock()
	sink, ok := s.sinks[record.Source].(ProgressSink)
	s.mu.RUnlock()
	if ok {
		return sink.DeliverProgress(ctx, record, progress)
	}
	return nil
}

func (s *Service) HandleResult(ctx context.Context, result domain.Result) error {
	key := result.WorkloadID + "/" + result.AttemptID
	lock := s.recordLock(key)
	lock.Lock()
	defer lock.Unlock()
	record, ok := s.store.Get(key)
	if !ok {
		return errors.New("result does not match an external task")
	}
	if record.Result != nil {
		if record.UpstreamDelivered {
			return nil
		}
		return s.deliver(ctx, record, *record.Result)
	}
	record.Result = &result
	record.UpdatedAt = s.config.Now().UTC()
	switch result.State {
	case domain.StateCompleted, domain.StateReceiptCommitted:
		record.State = StateCompleted
	case domain.StateCancelled, domain.StateExpired:
		record.State = StateCancelled
	default:
		record.State = StateFailed
	}
	if err := s.save(record, "Worker produced terminal result"); err != nil {
		return err
	}
	return s.deliver(ctx, record, result)
}

func (s *Service) ReplayResults(ctx context.Context) {
	for _, record := range s.store.List() {
		if record.Result != nil && !record.UpstreamDelivered {
			lock := s.recordLock(record.WorkloadKey)
			lock.Lock()
			current, ok := s.store.Get(record.WorkloadKey)
			if ok && current.Result != nil && !current.UpstreamDelivered {
				_ = s.deliver(ctx, current, *current.Result)
			}
			lock.Unlock()
		}
	}
}

func (s *Service) Records() []Record { return s.store.List() }

func (s *Service) CancelWorkload(ctx context.Context, workloadKey, reason string) error {
	lock := s.recordLock(workloadKey)
	lock.Lock()
	defer lock.Unlock()
	record, ok := s.store.Get(workloadKey)
	if !ok {
		return errors.New("room transfer cancellation does not match a dispatched workload")
	}
	if record.State == StateCompleted || record.State == StateFailed || record.State == StateCancelled || record.State == StateRejected {
		return nil
	}
	if err := s.control.Cancel(ctx, record.WorkerID, record.Spec.WorkloadID, record.Spec.AttemptID); err != nil {
		return err
	}
	record.State, record.UpstreamError, record.UpdatedAt = StateCancelled, reason, s.config.Now().UTC()
	return s.save(record, "BeamCore cancelled room transfer workload")
}

// SelectWorker exposes the same placement policy used by Dispatch to composite
// orchestration services which must provision Worker-bound external resources
// before sending a workload offer.
func (s *Service) SelectWorker(required []string, resources domain.Resources, excluded []string) (scheduling.Placement, error) {
	return s.scheduler.Select(scheduling.Request{RequiredCapabilities: required, Resources: resources,
		MaxObservationAge: s.config.MaxObservationAge, ExcludedWorkerIDs: excluded}, s.config.Now().UTC())
}

func (s *Service) CapabilityAvailable(capability string, resources domain.Resources) bool {
	_, err := s.SelectWorker([]string{capability}, resources, nil)
	return err == nil
}

func (s *Service) Record(key string) (Record, bool) { return s.store.Get(key) }

func (s *Service) Cancel(ctx context.Context, key, reason string) error {
	lock := s.recordLock(key)
	lock.Lock()
	defer lock.Unlock()
	record, ok := s.store.Get(key)
	if !ok {
		return errors.New("workload cancellation does not match an external task")
	}
	if record.State == StateCompleted || record.State == StateFailed || record.State == StateCancelled || record.State == StateRejected {
		return nil
	}
	if err := s.control.Cancel(ctx, record.WorkerID, record.Spec.WorkloadID, record.Spec.AttemptID); err != nil {
		return err
	}
	record.State = StateCancelled
	record.UpstreamError = reason
	record.UpdatedAt = s.config.Now().UTC()
	return s.save(record, "external authority cancelled workload")
}

func (s *Service) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 32
	}
	channel := make(chan Event, buffer)
	s.mu.Lock()
	s.nextSub++
	id := s.nextSub
	s.subscribers[id] = channel
	s.mu.Unlock()
	return channel, func() {
		s.mu.Lock()
		if current := s.subscribers[id]; current != nil {
			delete(s.subscribers, id)
			close(current)
		}
		s.mu.Unlock()
	}
}

func (s *Service) terminal(record Record, state State, message string, cause error) (Record, error) {
	record.State = state
	record.UpstreamError = message
	record.UpdatedAt = s.config.Now().UTC()
	if err := s.save(record, message); err != nil {
		return record, err
	}
	return record, cause
}

func (s *Service) deliver(ctx context.Context, record Record, result domain.Result) error {
	s.mu.RLock()
	sink := s.sinks[record.Source]
	s.mu.RUnlock()
	if sink == nil {
		record.UpstreamError = "external session is not connected"
		record.UpdatedAt = s.config.Now().UTC()
		_ = s.save(record, "terminal result waiting for external session")
		return errors.New(record.UpstreamError)
	}
	if err := sink.DeliverResult(ctx, record, result); err != nil {
		record.UpstreamError = err.Error()
		record.UpdatedAt = s.config.Now().UTC()
		var terminal *TerminalDeliveryError
		if errors.As(err, &terminal) {
			record.UpstreamDelivered = true
			return s.save(record, "external authority returned a terminal result disposition; replay stopped")
		}
		_ = s.save(record, "external result delivery failed; replay scheduled")
		return err
	}
	record.UpstreamDelivered = true
	record.UpstreamError = ""
	record.UpdatedAt = s.config.Now().UTC()
	return s.save(record, "external authority acknowledged result")
}

func (s *Service) save(record Record, message string) error {
	if err := s.store.Put(record); err != nil {
		return err
	}
	event := Event{Sequence: s.sequence.Add(1), ObservedAt: s.config.Now().UTC(), Source: record.Source,
		ExternalID: record.ExternalID, WorkloadKey: record.WorkloadKey, WorkerID: record.WorkerID,
		State: record.State, Message: message}
	s.mu.RLock()
	for _, subscriber := range s.subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
	s.mu.RUnlock()
	return nil
}

func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (s *Service) recordLock(key string) *sync.Mutex {
	var hash uint32 = 2166136261
	for index := 0; index < len(key); index++ {
		hash ^= uint32(key[index])
		hash *= 16777619
	}
	return &s.recordLocks[hash%uint32(len(s.recordLocks))]
}
