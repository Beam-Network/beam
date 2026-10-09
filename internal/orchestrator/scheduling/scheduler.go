package scheduling

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	workload "github.com/Beam-Network/beam/internal/workload/domain"
)

var ErrNoCandidate = errors.New("no compatible Worker candidate")

type Request struct {
	RequiredCapabilities []string
	Resources            workload.Resources
	MaxObservationAge    time.Duration
	ExcludedWorkerIDs    []string
}

type Rejection struct {
	WorkerID string
	Reason   string
}

type Placement struct {
	WorkerID string
	NodeID   string
	Score    int64
	Rejected []Rejection
}

type Scheduler struct {
	registry *registry.Registry

	mu      sync.Mutex
	pending map[string][]reservation
}

type reservation struct {
	key       string
	resources workload.Resources
	at        time.Time
}

const reservationSettle = 2 * time.Second

const maxReservationAge = time.Minute

func New(registry *registry.Registry) *Scheduler {
	return &Scheduler{registry: registry, pending: make(map[string][]reservation)}
}

func (s *Scheduler) Select(request Request, now time.Time) (Placement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selectCountingPending(request, now)
}

func (s *Scheduler) selectCountingPending(request Request, now time.Time) (Placement, error) {
	placement, err := s.selectLocked(request, now, true)
	if errors.Is(err, ErrNoCandidate) {
		return s.selectLocked(request, now, false)
	}
	return placement, err
}

func (s *Scheduler) Reserve(key string, request Request, now time.Time) (Placement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetLocked(key, now)
	placement, err := s.selectCountingPending(request, now)
	if err == nil {
		s.pending[placement.WorkerID] = append(s.pending[placement.WorkerID],
			reservation{key: key, resources: request.Resources, at: now})
	}
	return placement, err
}

func (s *Scheduler) Hold(workerID, key string, resources workload.Resources, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetLocked(key, now)
	s.pending[workerID] = append(s.pending[workerID], reservation{key: key, resources: resources, at: now})
}

func (s *Scheduler) Release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetLocked(key, time.Time{})
}

func (s *Scheduler) forgetLocked(key string, now time.Time) {
	for workerID, reservations := range s.pending {
		reservations = slices.DeleteFunc(reservations, func(current reservation) bool {
			return current.key == key || (!now.IsZero() && now.Sub(current.at) > maxReservationAge)
		})
		if len(reservations) == 0 {
			delete(s.pending, workerID)
		} else {
			s.pending[workerID] = reservations
		}
	}
}

func (s *Scheduler) unreported(observation orchestratordomain.WorkerObservation, now time.Time) workload.Resources {
	reservations := slices.DeleteFunc(s.pending[observation.WorkerID], func(current reservation) bool {
		return observation.ObservedAt.After(current.at.Add(reservationSettle)) || now.Sub(current.at) > maxReservationAge
	})
	if len(reservations) == 0 {
		delete(s.pending, observation.WorkerID)
		return workload.Resources{}
	}
	s.pending[observation.WorkerID] = reservations
	var total workload.Resources
	for _, current := range reservations {
		total = total.Add(current.resources)
	}
	return total
}

func (s *Scheduler) selectLocked(request Request, now time.Time, countPending bool) (Placement, error) {
	if request.MaxObservationAge <= 0 {
		request.MaxObservationAge = 30 * time.Second
	}
	type candidate struct {
		observation orchestratordomain.WorkerObservation
		score       int64
	}
	var candidates []candidate
	placement := Placement{}
	for _, observation := range s.registry.Observations() {
		if countPending {
			observation.Available = observation.Available.SubFloor(s.unreported(observation, now))
		}
		reason := rejectionReason(observation, request, now)
		if reason != "" {
			placement.Rejected = append(placement.Rejected, Rejection{WorkerID: observation.WorkerID, Reason: reason})
			continue
		}
		score := observation.Available.BandwidthMbps*1_000_000 + observation.Available.MemoryBytes/(1<<20)
		candidates = append(candidates, candidate{observation: observation, score: score})
	}
	if len(candidates) == 0 {
		return placement, ErrNoCandidate
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].observation.WorkerID < candidates[j].observation.WorkerID
		}
		return candidates[i].score > candidates[j].score
	})
	selected := candidates[0]
	placement.WorkerID = selected.observation.WorkerID
	placement.NodeID = selected.observation.NodeID
	placement.Score = selected.score
	return placement, nil
}

func rejectionReason(observation orchestratordomain.WorkerObservation, request Request, now time.Time) string {
	if slices.Contains(request.ExcludedWorkerIDs, observation.WorkerID) {
		return "excluded by placement request"
	}
	if observation.Status != "active" {
		return "not active"
	}
	if observation.ObservedAt.IsZero() || now.Sub(observation.ObservedAt) > request.MaxObservationAge {
		return "stale observation"
	}
	for _, capability := range request.RequiredCapabilities {
		if !slices.Contains(observation.Capabilities, capability) {
			return fmt.Sprintf("missing capability %s", capability)
		}
	}
	if !observation.Available.Fits(request.Resources) {
		return "insufficient resources"
	}
	return ""
}
