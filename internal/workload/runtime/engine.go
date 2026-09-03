package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/resources"
	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

var (
	ErrWrongWorker       = errors.New("workload is assigned to another worker")
	ErrInvalidState      = errors.New("invalid workload state")
	ErrStalePlan         = errors.New("stale workload plan")
	ErrCapabilityMissing = errors.New("required capability is unavailable")
)

type Decision struct {
	WorkloadID string
	AttemptID  string
	Accepted   bool
	Reason     string
}

type Engine struct {
	workerID     string
	capabilities map[string]struct{}
	registry     *Registry
	governor     *resources.Governor
	store        Store
	now          func() time.Time

	mu          sync.Mutex
	cancels     map[string]context.CancelFunc
	results     chan domain.Result
	progress    chan domain.Progress
	checkpoints chan domain.Checkpoint
}

func NewEngine(workerID string, capabilities []string, registry *Registry, governor *resources.Governor, store Store) (*Engine, error) {
	if workerID == "" {
		return nil, errors.New("worker_id is required")
	}
	if registry == nil || governor == nil || store == nil {
		return nil, errors.New("registry, governor, and store are required")
	}
	capabilitySet := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if capability == "" {
			return nil, errors.New("capabilities cannot contain an empty value")
		}
		capabilitySet[capability] = struct{}{}
	}
	return &Engine{
		workerID: workerID, capabilities: capabilitySet, registry: registry,
		governor: governor, store: store, now: time.Now,
		cancels: make(map[string]context.CancelFunc), results: make(chan domain.Result, 64),
		progress: make(chan domain.Progress, 128), checkpoints: make(chan domain.Checkpoint, 128),
	}, nil
}

func (e *Engine) Results() <-chan domain.Result         { return e.results }
func (e *Engine) Progress() <-chan domain.Progress      { return e.progress }
func (e *Engine) Checkpoints() <-chan domain.Checkpoint { return e.checkpoints }

func (e *Engine) Reconcile(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.expireOffers(e.now())
		}
	}
}

func (e *Engine) expireOffers(now time.Time) {
	for _, record := range e.store.List() {
		if record.State != domain.StateOffered && record.State != domain.StateReserved {
			continue
		}
		if now.Before(record.Spec.Lease.OfferExpiresAt) {
			continue
		}
		record.State = domain.StateExpired
		record.UpdatedAt = now
		if e.store.Save(record) == nil {
			e.governor.Release(record.Spec.Key())
		}
	}
}

// Recover rebuilds resource reservations and resumes committed work after a
// process restart. Stale offers and expired leases are transitioned durably.
func (e *Engine) Recover(parent context.Context) error {
	now := e.now()
	for _, record := range e.store.List() {
		switch record.State {
		case domain.StateOffered, domain.StateReserved:
			if !now.Before(record.Spec.Lease.OfferExpiresAt) {
				record.State = domain.StateExpired
				record.UpdatedAt = now
				if err := e.store.Save(record); err != nil {
					return err
				}
				continue
			}
			if err := e.governor.Reserve(record.Spec.Key(), record.Spec.Resources); err != nil {
				return fmt.Errorf("recover reservation %s: %w", record.Spec.Key(), err)
			}
			if record.State == domain.StateOffered {
				record.State = domain.StateReserved
				record.UpdatedAt = now
				if err := e.store.Save(record); err != nil {
					e.governor.Release(record.Spec.Key())
					return err
				}
			}
		case domain.StateCommitted, domain.StateStarting, domain.StateRunning:
			if !now.Before(record.Spec.Lease.AssignmentExpiresAt) {
				record.State = domain.StateExpired
				record.UpdatedAt = now
				if err := e.store.Save(record); err != nil {
					return err
				}
				continue
			}
			if err := e.governor.Reserve(record.Spec.Key(), record.Spec.Resources); err != nil {
				return fmt.Errorf("recover committed reservation %s: %w", record.Spec.Key(), err)
			}
			if err := e.governor.Commit(record.Spec.Key()); err != nil {
				return err
			}
			ctx, cancel := context.WithDeadline(context.WithoutCancel(parent), record.Spec.Lease.AssignmentExpiresAt)
			e.mu.Lock()
			e.cancels[record.Spec.Key()] = cancel
			e.mu.Unlock()
			go e.execute(ctx, record)
		}
	}
	return nil
}

func (e *Engine) Offer(ctx context.Context, spec domain.Spec) (Decision, error) {
	now := e.now()
	if err := spec.Validate(now); err != nil {
		return Decision{WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID, Reason: err.Error()}, err
	}
	if spec.Identity.WorkerID != e.workerID {
		return Decision{WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID, Reason: ErrWrongWorker.Error()}, ErrWrongWorker
	}
	for _, capability := range spec.RequiredCapabilities {
		if _, ok := e.capabilities[capability]; !ok {
			err := fmt.Errorf("%w: %s", ErrCapabilityMissing, capability)
			return Decision{WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID, Reason: err.Error()}, err
		}
	}
	handler, err := e.registry.Handler(spec.Kind)
	if err != nil {
		return Decision{WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID, Reason: err.Error()}, err
	}
	if err := handler.Validate(spec); err != nil {
		return Decision{WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID, Reason: err.Error()}, err
	}

	record := Record{Spec: spec, State: domain.StateOffered, CreatedAt: now, UpdatedAt: now}
	if err := e.store.Create(record); err != nil {
		if !errors.Is(err, ErrRecordExists) {
			return Decision{}, err
		}
		existing, getErr := e.store.Get(spec.Key())
		if getErr != nil {
			return Decision{}, getErr
		}
		return decisionFor(existing), nil
	}

	if err := e.governor.Reserve(spec.Key(), spec.Resources); err != nil {
		record.State = domain.StateRejected
		record.Reason = err.Error()
		record.UpdatedAt = e.now()
		_ = e.store.Save(record)
		return decisionFor(record), err
	}
	record.State = domain.StateReserved
	record.UpdatedAt = e.now()
	if err := e.store.Save(record); err != nil {
		e.governor.Release(spec.Key())
		return Decision{}, err
	}
	return decisionFor(record), nil
}

func (e *Engine) Commit(parent context.Context, commit domain.Commit) error {
	now := e.now()
	if err := commit.Validate(now); err != nil {
		return err
	}
	record, err := e.store.Get(commit.Key())
	if err != nil {
		return err
	}
	if commit.PlanVersion == record.PlanVersion &&
		record.Spec.Lease.AssignmentExpiresAt.Equal(commit.AssignmentExpiresAt) &&
		(record.State == domain.StateCommitted || record.State == domain.StateStarting || record.State == domain.StateRunning ||
			record.State == domain.StateCompleted || record.State == domain.StateFailed || record.State == domain.StateCancelled ||
			record.State == domain.StateExpired || record.State == domain.StateReceiptCommitted) {
		return nil
	}
	if commit.PlanVersion <= record.PlanVersion {
		return ErrStalePlan
	}
	if record.State != domain.StateReserved {
		return fmt.Errorf("%w: cannot commit from %s", ErrInvalidState, record.State)
	}
	if err := e.governor.Commit(commit.Key()); err != nil {
		return err
	}
	record.State = domain.StateCommitted
	record.PlanVersion = commit.PlanVersion
	record.Spec.Lease.AssignmentExpiresAt = commit.AssignmentExpiresAt
	record.UpdatedAt = now
	if err := e.store.Save(record); err != nil {
		e.governor.Release(commit.Key())
		return err
	}

	// The workload outlives the transport request that delivered the commit.
	// Preserve its values while detaching cancellation from that request.
	ctx, cancel := context.WithDeadline(context.WithoutCancel(parent), commit.AssignmentExpiresAt)
	e.mu.Lock()
	e.cancels[commit.Key()] = cancel
	e.mu.Unlock()
	go e.execute(ctx, record)
	return nil
}

func (e *Engine) Cancel(key string) error {
	record, err := e.store.Get(key)
	if err != nil {
		return err
	}
	if record.State == domain.StateReserved {
		record.State = domain.StateCancelled
		record.UpdatedAt = e.now()
		if err := e.store.Save(record); err != nil {
			return err
		}
		e.governor.Release(key)
		return nil
	}
	e.mu.Lock()
	cancel := e.cancels[key]
	e.mu.Unlock()
	if cancel == nil {
		return fmt.Errorf("%w: cannot cancel from %s", ErrInvalidState, record.State)
	}
	cancel()
	return nil
}

func (e *Engine) execute(ctx context.Context, record Record) {
	key := record.Spec.Key()
	defer func() {
		e.governor.Release(key)
		e.mu.Lock()
		if cancel := e.cancels[key]; cancel != nil {
			cancel()
		}
		delete(e.cancels, key)
		e.mu.Unlock()
	}()

	started := e.now()
	record.State = domain.StateStarting
	record.UpdatedAt = started
	_ = e.store.Save(record)
	record.State = domain.StateRunning
	record.UpdatedAt = e.now()
	_ = e.store.Save(record)

	handler, err := e.registry.Handler(record.Spec.Kind)
	var result domain.Result
	if err == nil {
		handlerContext := workloadprogress.WithReporter(ctx, func(outputs map[string]string) {
			e.reportProgress(record.Spec.Key(), outputs)
		})
		initial := record.Checkpoint
		if initial == nil {
			initial = &domain.Checkpoint{WorkloadID: record.Spec.WorkloadID, AttemptID: record.Spec.AttemptID, Kind: record.Spec.Kind}
		}
		handlerContext = workloadcheckpoint.WithManager(handlerContext, initial, func(value domain.Checkpoint) error {
			return e.reportCheckpoint(record.Spec.Key(), value)
		})
		result, err = handler.Execute(handlerContext, record.Spec)
	}
	result.WorkloadID = record.Spec.WorkloadID
	result.AttemptID = record.Spec.AttemptID
	result.StartedAt = started
	result.CompletedAt = e.now()
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			result.State = domain.StateCancelled
			result.ErrorCode = "cancelled"
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.State = domain.StateExpired
			result.ErrorCode = "lease_expired"
		} else {
			result.State = domain.StateFailed
			result.ErrorCode = "execution_failed"
		}
		result.ErrorMessage = err.Error()
	} else {
		result.State = domain.StateCompleted
	}
	record.State = result.State
	record.Result = &result
	if current, getErr := e.store.Get(key); getErr == nil {
		record.Progress = current.Progress
		record.Checkpoint = current.Checkpoint
	}
	record.UpdatedAt = result.CompletedAt
	_ = e.store.Save(record)
	select {
	case e.results <- result:
	default:
	}
}

func (e *Engine) reportCheckpoint(key string, checkpoint domain.Checkpoint) error {
	record, err := e.store.Get(key)
	if err != nil {
		return err
	}
	if strings.TrimSpace(checkpoint.Schema) == "" || checkpoint.Sequence == 0 ||
		len(checkpoint.Payload) > 8<<20 || !json.Valid(checkpoint.Payload) {
		return errors.New("invalid workload checkpoint")
	}
	if checkpoint.WorkloadID != record.Spec.WorkloadID || checkpoint.AttemptID != record.Spec.AttemptID || checkpoint.Kind != record.Spec.Kind {
		return errors.New("checkpoint identity does not match workload")
	}
	expected := uint64(1)
	if record.Checkpoint != nil {
		expected = record.Checkpoint.Sequence + 1
	}
	if checkpoint.Sequence != expected {
		return fmt.Errorf("checkpoint sequence %d does not follow %d", checkpoint.Sequence, expected-1)
	}
	checkpoint.ObservedAt = e.now().UTC()
	record.Checkpoint = &checkpoint
	record.UpdatedAt = checkpoint.ObservedAt
	if err := e.store.Save(record); err != nil {
		return err
	}
	select {
	case e.checkpoints <- checkpoint:
	default:
	}
	return nil
}

func (e *Engine) reportProgress(key string, outputs map[string]string) {
	record, err := e.store.Get(key)
	if err != nil {
		return
	}
	progress := domain.Progress{
		WorkloadID: record.Spec.WorkloadID, AttemptID: record.Spec.AttemptID,
		State: record.State, Outputs: maps.Clone(outputs), ObservedAt: e.now(),
	}
	record.Progress = &progress
	record.UpdatedAt = progress.ObservedAt
	if e.store.Save(record) != nil {
		return
	}
	select {
	case e.progress <- progress:
	default:
	}
}

func decisionFor(record Record) Decision {
	return Decision{
		WorkloadID: record.Spec.WorkloadID,
		AttemptID:  record.Spec.AttemptID,
		Accepted: record.State == domain.StateReserved || record.State == domain.StateCommitted || record.State == domain.StateStarting ||
			record.State == domain.StateRunning || record.State == domain.StateCompleted || record.State == domain.StateReceiptCommitted,
		Reason: record.Reason,
	}
}
