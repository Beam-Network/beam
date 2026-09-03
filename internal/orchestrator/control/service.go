package control

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	workload "github.com/Beam-Network/beam/internal/workload/domain"
)

const ProtocolVersion = 1

type WorkerHello struct {
	Identity               workload.Identity
	OrchestratorDelegation []byte
	SoftwareVersion        string
	ProtocolVersion        uint32
	Capabilities           []string
	CapabilityManifest     *contracts.CapabilityManifest
	TotalResources         workload.Resources
	LastPlanVersion        uint64
	LastEventCursor        uint64
}

type OrchestratorWelcome struct {
	OrchestratorID     string
	SessionID          string
	HeartbeatInterval  time.Duration
	ConfigEpoch        uint64
	CurrentPlanVersion uint64
}

type Service struct {
	orchestratorID    string
	registry          *registry.Registry
	heartbeatInterval time.Duration
	configEpoch       uint64
	now               func() time.Time
}

func NewService(orchestratorID string, registry *registry.Registry, configEpoch uint64) (*Service, error) {
	if orchestratorID == "" || registry == nil || configEpoch == 0 {
		return nil, errors.New("orchestrator_id, registry, and positive config epoch are required")
	}
	return &Service{orchestratorID: orchestratorID, registry: registry, heartbeatInterval: 10 * time.Second, configEpoch: configEpoch, now: time.Now}, nil
}

func (s *Service) Accept(hello WorkerHello) (OrchestratorWelcome, error) {
	if hello.ProtocolVersion != ProtocolVersion {
		return OrchestratorWelcome{}, errors.New("unsupported Worker control protocol version")
	}
	if err := hello.Identity.Validate(); err != nil {
		return OrchestratorWelcome{}, err
	}
	if hello.Identity.OrchestratorID != s.orchestratorID {
		return OrchestratorWelcome{}, errors.New("Worker identity belongs to another Orchestrator")
	}
	membership, exists := s.registry.Membership(hello.Identity.WorkerID)
	if !exists || membership.Status != "active" {
		return OrchestratorWelcome{}, errors.New("worker is not an active member of this Orchestrator")
	}
	if err := membership.Validate(s.now()); err != nil {
		return OrchestratorWelcome{}, err
	}
	if membership.OrchestratorID != s.orchestratorID {
		return OrchestratorWelcome{}, errors.New("worker belongs to another Orchestrator")
	}
	if membership.NodeID != "" && membership.NodeID != hello.Identity.NodeID {
		return OrchestratorWelcome{}, errors.New("worker node identity does not match its membership")
	}
	observation, exists := s.registry.Observation(hello.Identity.WorkerID)
	if exists && observation.NodeID != "" && observation.NodeID != hello.Identity.NodeID {
		return OrchestratorWelcome{}, errors.New("worker node identity changed without membership update")
	}
	currentPlanVersion := hello.LastPlanVersion
	if exists && observation.PlanVersion > currentPlanVersion {
		currentPlanVersion = observation.PlanVersion
	}
	if _, err := workerCapabilities(hello.Identity, hello.CapabilityManifest, hello.Capabilities, s.now()); err != nil {
		return OrchestratorWelcome{}, err
	}
	sessionBytes := make([]byte, 16)
	if _, err := rand.Read(sessionBytes); err != nil {
		return OrchestratorWelcome{}, err
	}
	return OrchestratorWelcome{
		OrchestratorID: s.orchestratorID, SessionID: "session_" + hex.EncodeToString(sessionBytes),
		HeartbeatInterval: s.heartbeatInterval, ConfigEpoch: s.configEpoch,
		CurrentPlanVersion: currentPlanVersion,
	}, nil
}

func (s *Service) Heartbeat(identity workload.Identity, status, region, circuitEndpoint string,
	capabilities []string, capabilityManifest *contracts.CapabilityManifest,
	total, available workload.Resources, planVersion uint64) error {
	canonicalCapabilities, err := workerCapabilities(identity, capabilityManifest, capabilities, s.now())
	if err != nil {
		return err
	}
	return s.registry.UpdateObservation(orchestratordomain.WorkerObservation{
		WorkerID: identity.WorkerID, NodeID: identity.NodeID, Region: region,
		Status: status, Capabilities: canonicalCapabilities, Total: total, Available: available,
		ObservedAt: s.now(), PlanVersion: planVersion, CircuitEndpoint: circuitEndpoint,
	}, s.now())
}

func workerCapabilities(identity workload.Identity, manifest *contracts.CapabilityManifest, legacy []string, now time.Time) ([]string, error) {
	if manifest == nil {
		for _, capability := range contracts.NormalizeCapabilities(legacy) {
			if capability == contracts.TransferMultipartCapability {
				return []string{contracts.TransferMultipartCapability}, nil
			}
		}
		return nil, nil
	}
	if manifest.SchemaVersion != contracts.RoomTransferSchemaVersion || manifest.ActorType != "worker" ||
		strings.TrimSpace(manifest.ActorID) != identity.WorkerID || strings.TrimSpace(manifest.SoftwareVersion) == "" ||
		manifest.ObservedAt.IsZero() || manifest.ExpiresAt.IsZero() || !now.Before(manifest.ExpiresAt) {
		return nil, errors.New("invalid worker capability manifest")
	}
	supported := make([]string, 0, len(manifest.Capabilities))
	for _, capability := range contracts.NormalizeCapabilities(manifest.Capabilities) {
		if contracts.SupportsCapability(*manifest, capability, now) {
			supported = append(supported, capability)
		}
	}
	return supported, nil
}
