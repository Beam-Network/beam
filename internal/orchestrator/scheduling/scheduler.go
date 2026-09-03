package scheduling

import (
	"errors"
	"fmt"
	"slices"
	"sort"
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
}

func New(registry *registry.Registry) *Scheduler { return &Scheduler{registry: registry} }

func (s *Scheduler) Select(request Request, now time.Time) (Placement, error) {
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
