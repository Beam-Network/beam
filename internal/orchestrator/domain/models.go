package domain

import (
	"errors"
	"strings"
	"time"

	workload "github.com/Beam-Network/beam/internal/workload/domain"
)

type Orchestrator struct {
	OrchestratorID  string
	Hotkey          string
	NetUID          uint32
	Status          string
	ManifestVersion uint64
	CreatedAt       time.Time
}

func (b Orchestrator) Validate() error {
	if strings.TrimSpace(b.OrchestratorID) == "" {
		return errors.New("orchestrator_id is required")
	}
	return nil
}

type Membership struct {
	OrchestratorID string
	WorkerID       string
	NodeID         string
	Role           string
	Status         string
	Delegation     []byte
	JoinedAt       time.Time
	ExpiresAt      time.Time
}

func (m Membership) Validate(now time.Time) error {
	if strings.TrimSpace(m.OrchestratorID) == "" || strings.TrimSpace(m.WorkerID) == "" {
		return errors.New("orchestrator_id and worker_id are required")
	}
	if !m.ExpiresAt.IsZero() && !now.Before(m.ExpiresAt) {
		return errors.New("orchestrator membership has expired")
	}
	return nil
}

type WorkerObservation struct {
	WorkerID        string
	NodeID          string
	Region          string
	Status          string
	Capabilities    []string
	Total           workload.Resources
	Available       workload.Resources
	ObservedAt      time.Time
	PlanVersion     uint64
	CircuitEndpoint string
}

type Manifest struct {
	OrchestratorID string
	Version        uint64
	Regions        []string
	Capabilities   []string
	Total          workload.Resources
	Available      workload.Resources
	Gateways       []string
	ExpiresAt      time.Time
	Signature      []byte
}
