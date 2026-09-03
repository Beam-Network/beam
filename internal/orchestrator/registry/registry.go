package registry

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
)

var (
	ErrMemberNotFound = errors.New("orchestrator member not found")
	ErrNodeConflict   = errors.New("node identity is already attached to another worker")
)

type Registry struct {
	mu           sync.RWMutex
	orchestrator orchestratordomain.Orchestrator
	memberships  map[string]orchestratordomain.Membership
	observations map[string]orchestratordomain.WorkerObservation
	version      uint64
	store        StateStore
}

func New(orchestrator orchestratordomain.Orchestrator) (*Registry, error) {
	return NewWithStore(orchestrator, nil)
}

func NewWithStore(orchestrator orchestratordomain.Orchestrator, store StateStore) (*Registry, error) {
	if err := orchestrator.Validate(); err != nil {
		return nil, err
	}
	result := &Registry{memberships: make(map[string]orchestratordomain.Membership),
		observations: make(map[string]orchestratordomain.WorkerObservation), store: store}
	if store == nil {
		result.orchestrator = orchestrator
		return result, nil
	}
	state, err := store.Load()
	if err != nil {
		return nil, err
	}
	if state.Orchestrator.OrchestratorID != "" && state.Orchestrator.OrchestratorID != orchestrator.OrchestratorID {
		return nil, fmt.Errorf("persisted orchestrator %q does not match %q", state.Orchestrator.OrchestratorID, orchestrator.OrchestratorID)
	}
	if state.Orchestrator.OrchestratorID != "" {
		if orchestrator.Hotkey == "" {
			orchestrator.Hotkey = state.Orchestrator.Hotkey
		}
		if orchestrator.NetUID == 0 {
			orchestrator.NetUID = state.Orchestrator.NetUID
		}
		if !state.Orchestrator.CreatedAt.IsZero() {
			orchestrator.CreatedAt = state.Orchestrator.CreatedAt
		}
		if orchestrator.ManifestVersion == 0 {
			orchestrator.ManifestVersion = state.Orchestrator.ManifestVersion
		}
	}
	result.orchestrator = orchestrator
	result.version = state.Version
	for _, membership := range state.Memberships {
		result.memberships[membership.WorkerID] = membership
	}
	for _, observation := range state.Observations {
		result.observations[observation.WorkerID] = observation
	}
	if state.Orchestrator.OrchestratorID == "" {
		if err := result.persistLocked(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (r *Registry) Join(membership orchestratordomain.Membership, now time.Time) error {
	if err := membership.Validate(now); err != nil {
		return err
	}
	if membership.OrchestratorID != r.orchestrator.OrchestratorID {
		return fmt.Errorf("membership orchestrator %q does not match %q", membership.OrchestratorID, r.orchestrator.OrchestratorID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for workerID, existing := range r.memberships {
		if membership.NodeID != "" && existing.NodeID == membership.NodeID && workerID != membership.WorkerID {
			return ErrNodeConflict
		}
	}
	previous, existed := r.memberships[membership.WorkerID]
	if existed {
		membership.JoinedAt = previous.JoinedAt
	}
	if membership.JoinedAt.IsZero() {
		membership.JoinedAt = now
	}
	if membership.Status == "" {
		membership.Status = "active"
	}
	r.memberships[membership.WorkerID] = membership
	r.version++
	if err := r.persistLocked(); err != nil {
		r.version--
		if existed {
			r.memberships[membership.WorkerID] = previous
		} else {
			delete(r.memberships, membership.WorkerID)
		}
		return err
	}
	return nil
}

func (r *Registry) UpdateObservation(observation orchestratordomain.WorkerObservation, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	membership, exists := r.memberships[observation.WorkerID]
	if !exists {
		return ErrMemberNotFound
	}
	if membership.Status != "active" {
		return fmt.Errorf("worker %s membership is %s", observation.WorkerID, membership.Status)
	}
	if err := membership.Validate(now); err != nil {
		return err
	}
	if membership.NodeID != "" && observation.NodeID != membership.NodeID {
		return fmt.Errorf("node identity mismatch for worker %s", observation.WorkerID)
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = now
	}
	if previous, ok := r.observations[observation.WorkerID]; ok && observation.PlanVersion < previous.PlanVersion {
		return errors.New("observation plan version rolled back")
	}
	previous, existed := r.observations[observation.WorkerID]
	r.observations[observation.WorkerID] = observation
	r.version++
	if err := r.persistLocked(); err != nil {
		r.version--
		if existed {
			r.observations[observation.WorkerID] = previous
		} else {
			delete(r.observations, observation.WorkerID)
		}
		return err
	}
	return nil
}

func (r *Registry) Observation(workerID string) (orchestratordomain.WorkerObservation, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	observation, ok := r.observations[workerID]
	return observation, ok
}

func (r *Registry) Membership(workerID string) (orchestratordomain.Membership, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	membership, ok := r.memberships[workerID]
	return membership, ok
}

func (r *Registry) Observations() []orchestratordomain.WorkerObservation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]orchestratordomain.WorkerObservation, 0, len(r.observations))
	for _, observation := range r.observations {
		result = append(result, observation)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].WorkerID < result[j].WorkerID })
	return result
}

func (r *Registry) Manifest(now time.Time, ttl time.Duration, gateways []string) orchestratordomain.Manifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	regions := make(map[string]struct{})
	capabilities := make(map[string]struct{})
	manifest := orchestratordomain.Manifest{OrchestratorID: r.orchestrator.OrchestratorID, Version: r.version, Gateways: slices.Clone(gateways), ExpiresAt: now.Add(ttl)}
	for workerID, observation := range r.observations {
		membership, ok := r.memberships[workerID]
		if !ok || membership.Status != "active" || observation.Status != "active" ||
			(!membership.ExpiresAt.IsZero() && !now.Before(membership.ExpiresAt)) {
			continue
		}
		manifest.Total = manifest.Total.Add(observation.Total)
		manifest.Available = manifest.Available.Add(observation.Available)
		if observation.Region != "" {
			regions[observation.Region] = struct{}{}
		}
		for _, capability := range observation.Capabilities {
			if capability != "" {
				capabilities[capability] = struct{}{}
			}
		}
	}
	manifest.Regions = sortedKeys(regions)
	manifest.Capabilities = sortedKeys(capabilities)
	return manifest
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (r *Registry) persistLocked() error {
	if r.store == nil {
		return nil
	}
	state := State{Version: r.version, Orchestrator: r.orchestrator}
	state.Memberships = make([]orchestratordomain.Membership, 0, len(r.memberships))
	for _, membership := range r.memberships {
		state.Memberships = append(state.Memberships, membership)
	}
	state.Observations = make([]orchestratordomain.WorkerObservation, 0, len(r.observations))
	for _, observation := range r.observations {
		state.Observations = append(state.Observations, observation)
	}
	return r.store.Save(state)
}
