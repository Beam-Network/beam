package roomworkload

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type Config struct{ Now func() time.Time }

const maxRoomPathRedemptions = 16

type RoomWorkloadService[D, U, I, C, P, R, O any] struct {
	mu           sync.RWMutex
	locks        KeyedLocks
	config       Config
	dispatcher   *dispatch.Service
	store        Store[D, U, I, C, P, R]
	strategy     Strategy[D, U, I, C, P, R, O]
	provisioner  Provisioner[D, U, I, C]
	sink         ResultSink[O]
	progressSink ProgressSink[P]
}

func NewRoomWorkloadService[D, U, I, C, P, R, O any](config Config, dispatcher *dispatch.Service,
	store Store[D, U, I, C, P, R], strategy Strategy[D, U, I, C, P, R, O]) (*RoomWorkloadService[D, U, I, C, P, R, O], error) {
	if dispatcher == nil || store == nil || strategy == nil {
		return nil, errors.New("room workload dispatcher, store, and strategy are required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	service := &RoomWorkloadService[D, U, I, C, P, R, O]{config: config, dispatcher: dispatcher, store: store, strategy: strategy}
	dispatcher.RegisterSink(strategy.DispatchSource(), service)
	return service, nil
}

func (s *RoomWorkloadService[D, U, I, C, P, R, O]) RegisterProvisioner(value Provisioner[D, U, I, C]) {
	s.mu.Lock()
	s.provisioner = value
	s.mu.Unlock()
}

func (s *RoomWorkloadService[D, U, I, C, P, R, O]) RegisterSink(value ResultSink[O]) {
	s.mu.Lock()
	s.sink = value
	s.mu.Unlock()
}

func (s *RoomWorkloadService[D, U, I, C, P, R, O]) RegisterProgressSink(value ProgressSink[P]) {
	s.mu.Lock()
	s.progressSink = value
	s.mu.Unlock()
}

func (s *RoomWorkloadService[D, U, I, C, P, R, O]) Submit(ctx context.Context, definition RoomWorkloadDefinition[D]) error {
	if definition.ID == "" || definition.RoomID == "" {
		return errors.New("room workload definition identity is required")
	}
	defer s.locks.Lock(definition.ID)()
	now := s.config.Now().UTC()
	if err := s.strategy.ValidateDefinition(definition, now); err != nil {
		return err
	}
	record, exists := s.store.Get(definition.ID)
	if exists {
		if !s.strategy.SameDefinition(record.Definition.Workload, definition.Workload) {
			return errors.New("room workload idempotency key belongs to another definition")
		}
	} else {
		units, err := s.strategy.ExecutionUnits(definition)
		if err != nil {
			return err
		}
		record = Record[D, U, I, C, P, R]{Definition: definition,
			Attempts: make(map[string]RoomAttempt[U, I, C, P, R]), CreatedAt: now, UpdatedAt: now, Lifecycle: "submitted"}
		for _, unit := range units {
			if unit.ID == "" {
				return errors.New("room execution unit id is required")
			}
			if _, duplicate := record.Attempts[unit.ID]; duplicate {
				return fmt.Errorf("duplicate room execution unit %s", unit.ID)
			}
			source, targets, err := s.strategy.Paths(definition, unit)
			if err != nil {
				return err
			}
			if source.ID == "" {
				return fmt.Errorf("room execution unit %s source path id is required", unit.ID)
			}
			targetByID := make(map[string]RoomPath[I, C], len(targets))
			for _, target := range targets {
				if target.ID == "" {
					return fmt.Errorf("room execution unit %s target path id is required", unit.ID)
				}
				if _, duplicate := targetByID[target.ID]; duplicate {
					return fmt.Errorf("room execution unit %s has duplicate target path %s", unit.ID, target.ID)
				}
				targetByID[target.ID] = target
			}
			record.Attempts[unit.ID] = RoomAttempt[U, I, C, P, R]{Unit: unit, State: AttemptPending,
				Source: source, Targets: targetByID}
		}
		if err := s.store.Put(record); err != nil {
			return err
		}
	}
	if record.UpstreamDelivered || record.Lifecycle == "cancelled" {
		return nil
	}
	excluded := make([]string, 0, len(record.Attempts))
	record.Lifecycle = "planning"
	record.UpdatedAt = now
	if err := s.store.Put(record); err != nil {
		return err
	}
	for _, attempt := range record.Attempts {
		if attempt.WorkerID != "" {
			excluded = append(excluded, attempt.WorkerID)
		}
	}
	units, err := s.strategy.ExecutionUnits(record.Definition)
	if err != nil {
		return err
	}
	for _, unit := range units {
		attempt := record.Attempts[unit.ID]
		if attempt.State == AttemptCompleted || attempt.State == AttemptFailed || attempt.State == AttemptCancelled || attempt.State == AttemptDispatched {
			continue
		}
		if attempt.WorkerID == "" {
			placement, err := s.dispatcher.SelectWorker(s.strategy.RequiredCapabilities(record.Definition, unit),
				s.strategy.Resources(record.Definition, unit), excluded)
			if err != nil {
				return fmt.Errorf("select Worker for room execution unit %s: %w", unit.ID, err)
			}
			attempt.WorkerID, attempt.NodeID = placement.WorkerID, placement.NodeID
			attempt.Source.State = "selected"
			for pathID, target := range attempt.Targets {
				target.State = "selected"
				attempt.Targets[pathID] = target
			}
			excluded = append(excluded, placement.WorkerID)
			record.Attempts[unit.ID] = attempt
			record.UpdatedAt = now
			if err := s.store.Put(record); err != nil {
				return err
			}
		}
		if attempt.Source.Credential == nil {
			record.Lifecycle = "provisioning"
			attempt.Source.State = "provisioning"
			credential, err := s.redeem(ctx, record.Definition, unit, attempt, attempt.Source, now)
			if err != nil {
				return fmt.Errorf("redeem source path for room execution unit %s: %w", unit.ID, err)
			}
			attempt.Source.Credential = &credential
			attempt.Source.State = "ready"
			record.Attempts[unit.ID] = attempt
			record.UpdatedAt = now
			if err := s.store.Put(record); err != nil {
				return err
			}
		}
		_, targetPaths, err := s.strategy.Paths(record.Definition, unit)
		if err != nil {
			return err
		}
		credentials := make([]C, len(targetPaths))
		redemptionErrors := make([]error, len(targetPaths))
		pending := make([]bool, len(targetPaths))
		var redemptions sync.WaitGroup
		redemptionSlots := make(chan struct{}, maxRoomPathRedemptions)
		for index, planned := range targetPaths {
			pathID := planned.ID
			target, ok := attempt.Targets[pathID]
			if !ok {
				return fmt.Errorf("room execution unit %s is missing target path %s", unit.ID, pathID)
			}
			if target.Credential != nil {
				continue
			}
			target.State = "provisioning"
			attempt.Targets[pathID] = target
			pending[index] = true
			redemptionSlots <- struct{}{}
			redemptions.Add(1)
			go func(index int, target RoomPath[I, C]) {
				defer redemptions.Done()
				defer func() { <-redemptionSlots }()
				credentials[index], redemptionErrors[index] = s.redeem(ctx, record.Definition, unit, attempt, target, now)
			}(index, target)
		}
		redemptions.Wait()
		var redemptionErr error
		for index, planned := range targetPaths {
			if !pending[index] {
				continue
			}
			if err := redemptionErrors[index]; err != nil {
				if redemptionErr == nil {
					redemptionErr = fmt.Errorf("redeem target path %s for room execution unit %s: %w", planned.ID, unit.ID, err)
				}
				continue
			}
			target := attempt.Targets[planned.ID]
			target.Credential = &credentials[index]
			target.State = "ready"
			attempt.Targets[planned.ID] = target
		}
		record.Attempts[unit.ID] = attempt
		record.UpdatedAt = now
		if err := s.store.Put(record); err != nil {
			return err
		}
		if redemptionErr != nil {
			return redemptionErr
		}
		attempt.State = AttemptProvisioned
		spec, err := s.strategy.BuildWorkerSpec(record.Definition, attempt, now)
		if err != nil {
			return err
		}
		// The Worker may report progress as soon as Dispatch commits. Bind its
		// deterministic key durably before the Worker can start.
		attempt.WorkloadKey = spec.Key()
		record.Lifecycle = "ready"
		record.Attempts[unit.ID] = attempt
		record.UpdatedAt = now
		if err := s.store.Put(record); err != nil {
			return err
		}
		dispatched, err := s.dispatcher.Dispatch(ctx, dispatch.DispatchRequest{Source: s.strategy.DispatchSource(),
			ExternalID: record.Definition.ID + "/" + unit.ID, Spec: spec, WorkerID: attempt.WorkerID, NodeID: attempt.NodeID})
		if err != nil {
			return fmt.Errorf("dispatch room execution unit %s: %w", unit.ID, err)
		}
		if dispatched.WorkloadKey != attempt.WorkloadKey {
			return errors.New("dispatched room workload key differs from durable attempt")
		}
		attempt.State = AttemptDispatched
		attempt.Source.State = "active"
		for pathID, target := range attempt.Targets {
			target.State = "active"
			attempt.Targets[pathID] = target
		}
		record.Lifecycle = "running"
		record.Attempts[unit.ID] = attempt
		record.UpdatedAt = s.config.Now().UTC()
		if err := s.store.Put(record); err != nil {
			return err
		}
	}
	return nil
}

func (s *RoomWorkloadService[D, U, I, C, P, R, O]) redeem(ctx context.Context, definition RoomWorkloadDefinition[D],
	unit RoomExecutionUnit[U], attempt RoomAttempt[U, I, C, P, R], path RoomPath[I, C], now time.Time) (C, error) {
	var zero C
	s.mu.RLock()
	provisioner := s.provisioner
	s.mu.RUnlock()
	if provisioner == nil {
		return zero, errors.New("room path provisioner is not connected")
	}
	credential, err := provisioner.Redeem(ctx, RedemptionRequest[D, U, I, C]{Definition: definition,
		Unit: unit, WorkerID: attempt.WorkerID, NodeID: attempt.NodeID, Path: path})
	if err != nil {
		return zero, err
	}
	if err := s.strategy.ValidateCredential(path, credential, now); err != nil {
		return zero, err
	}
	return credential, nil
}

func (s *RoomWorkloadService[D, U, I, C, P, R, O]) DeliverProgress(ctx context.Context, task dispatch.Record, progress domain.Progress) error {
	record, unitID, found := s.findByWorkload(task.WorkloadKey)
	if !found {
		return errors.New("room workload progress does not match a durable attempt")
	}
	defer s.locks.Lock(record.Definition.ID)()
	record, found = s.store.Get(record.Definition.ID)
	if !found || record.Attempts[unitID].WorkloadKey != task.WorkloadKey {
		return errors.New("room workload progress does not match a durable attempt")
	}
	attempt := record.Attempts[unitID]
	if attempt.State == AttemptCancelled || attempt.State == AttemptCompleted || attempt.State == AttemptFailed {
		return nil
	}
	value, err := s.strategy.ValidateProgress(record.Definition, attempt, progress)
	if err != nil {
		return err
	}
	attempt.Progress = &RoomProgress[P]{ObservedAt: progress.ObservedAt, Progress: value}
	record.Attempts[unitID] = attempt
	record.Lifecycle = "running"
	record.UpdatedAt = s.config.Now().UTC()
	if err := s.store.Put(record); err != nil {
		return err
	}
	s.mu.RLock()
	progressSink := s.progressSink
	s.mu.RUnlock()
	if progressSink != nil {
		return progressSink.DeliverRoomWorkloadProgress(ctx, value)
	}
	return nil
}

func (s *RoomWorkloadService[D, U, I, C, P, R, O]) DeliverResult(ctx context.Context, task dispatch.Record, result domain.Result) error {
	record, unitID, found := s.findByWorkload(task.WorkloadKey)
	if !found {
		return errors.New("room workload result does not match a durable attempt")
	}
	defer s.locks.Lock(record.Definition.ID)()
	record, found = s.store.Get(record.Definition.ID)
	if !found || record.Attempts[unitID].WorkloadKey != task.WorkloadKey {
		return errors.New("room workload result does not match a durable attempt")
	}
	attempt := record.Attempts[unitID]
	if attempt.State == AttemptCancelled {
		return s.deliverTerminal(ctx, record)
	}
	if result.State != domain.StateCompleted && result.State != domain.StateReceiptCommitted {
		if result.State == domain.StateCancelled || result.State == domain.StateExpired {
			attempt.State = AttemptCancelled
			record.Lifecycle = string(result.State)
		} else {
			attempt.State = AttemptFailed
			record.Lifecycle = "failed"
		}
		attempt.Error = fallback(result.ErrorMessage, "room workload Worker failed")
	} else {
		value, err := s.strategy.ValidateResult(record.Definition, attempt, result)
		if err != nil {
			return err
		}
		attempt.Result = &RoomResult[R]{CompletedAt: result.CompletedAt, Result: value}
		attempt.State = AttemptCompleted
		record.Lifecycle = "draining"
	}
	record.Attempts[unitID] = attempt
	record.UpdatedAt = s.config.Now().UTC()
	if err := s.store.Put(record); err != nil {
		return err
	}
	return s.deliverTerminal(ctx, record)
}

func (s *RoomWorkloadService[D, U, I, C, P, R, O]) Cancel(ctx context.Context, definitionID, reason string) error {
	unlock := s.locks.Lock(definitionID)
	record, ok := s.store.Get(definitionID)
	if !ok {
		unlock()
		return errors.New("room workload definition was not found")
	}
	workloadKeys := make([]string, 0, len(record.Attempts))
	for _, attempt := range record.Attempts {
		if attempt.WorkloadKey != "" && attempt.State != AttemptCompleted && attempt.State != AttemptFailed && attempt.State != AttemptCancelled {
			workloadKeys = append(workloadKeys, attempt.WorkloadKey)
		}
	}
	unlock()

	// Dispatch cancellation may synchronously race a Worker result. Do not hold
	// the definition lock while taking the dispatcher's per-record lock: result
	// delivery takes those locks in the opposite order.
	for _, workloadKey := range workloadKeys {
		if err := s.dispatcher.Cancel(ctx, workloadKey, reason); err != nil {
			return err
		}
	}

	defer s.locks.Lock(definitionID)()
	record, ok = s.store.Get(definitionID)
	if !ok {
		return errors.New("room workload definition was not found")
	}
	cancelled := false
	for unitID, attempt := range record.Attempts {
		if attempt.State == AttemptCompleted || attempt.State == AttemptFailed || attempt.State == AttemptCancelled {
			continue
		}
		cancelled = true
		attempt.State = AttemptCancelled
		attempt.Error = reason
		for pathID, target := range attempt.Targets {
			target.State = "dropped"
			attempt.Targets[pathID] = target
		}
		record.Attempts[unitID] = attempt
	}
	if !cancelled {
		return nil
	}
	record.Lifecycle = "cancelled"
	record.UpdatedAt = s.config.Now().UTC()
	return s.store.Put(record)
}

func (s *RoomWorkloadService[D, U, I, C, P, R, O]) Replay(ctx context.Context) {
	for _, record := range s.store.List() {
		if record.UpstreamDelivered {
			continue
		}
		if _, terminal := s.strategy.Aggregate(record.Definition, record.Attempts); terminal {
			unlock := s.locks.Lock(record.Definition.ID)
			current, ok := s.store.Get(record.Definition.ID)
			if ok {
				_ = s.deliverTerminal(ctx, current)
			}
			unlock()
			continue
		}
		_ = s.Submit(ctx, record.Definition)
	}
}

func (s *RoomWorkloadService[D, U, I, C, P, R, O]) deliverTerminal(ctx context.Context, record Record[D, U, I, C, P, R]) error {
	if record.UpstreamDelivered {
		return nil
	}
	output, terminal := s.strategy.Aggregate(record.Definition, record.Attempts)
	if !terminal {
		return nil
	}
	s.mu.RLock()
	sink := s.sink
	s.mu.RUnlock()
	if sink == nil {
		record.UpstreamError = "room workload result sink is not connected"
		_ = s.store.Put(record)
		return errors.New(record.UpstreamError)
	}
	if err := sink.DeliverRoomWorkloadResult(ctx, output); err != nil {
		record.UpstreamError = err.Error()
		_ = s.store.Put(record)
		return err
	}
	record.Lifecycle = terminalLifecycle(record.Attempts)
	record.UpstreamDelivered, record.UpstreamError, record.UpdatedAt = true, "", s.config.Now().UTC()
	return s.store.Put(record)
}

func terminalLifecycle[U, I, C, P, R any](attempts map[string]RoomAttempt[U, I, C, P, R]) string {
	state := "completed"
	for _, attempt := range attempts {
		switch attempt.State {
		case AttemptFailed:
			return "failed"
		case AttemptCancelled:
			state = "cancelled"
		}
	}
	return state
}

func (s *RoomWorkloadService[D, U, I, C, P, R, O]) findByWorkload(key string) (Record[D, U, I, C, P, R], string, bool) {
	for _, record := range s.store.List() {
		for unitID, attempt := range record.Attempts {
			if attempt.WorkloadKey == key {
				return record, unitID, true
			}
		}
	}
	var zero Record[D, U, I, C, P, R]
	return zero, "", false
}

func (s *RoomWorkloadService[D, U, I, C, P, R, O]) Record(definitionID string) (Record[D, U, I, C, P, R], bool) {
	return s.store.Get(definitionID)
}

// Records returns an immutable snapshot for logical-workload operations that
// may span more than one execution attempt. Callers must still use the
// definition ID for mutations.
func (s *RoomWorkloadService[D, U, I, C, P, R, O]) Records() []Record[D, U, I, C, P, R] {
	return s.store.List()
}

func fallback(value, other string) string {
	if value == "" {
		return other
	}
	return value
}

var _ dispatch.ResultSink = (*RoomWorkloadService[struct{}, struct{}, struct{}, struct{}, struct{}, struct{}, struct{}])(nil)
var _ dispatch.ProgressSink = (*RoomWorkloadService[struct{}, struct{}, struct{}, struct{}, struct{}, struct{}, struct{}])(nil)
