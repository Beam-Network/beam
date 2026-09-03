package connectors

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/orchestrator/roomtransfer"
	"github.com/Beam-Network/beam/internal/orchestrator/roomworkloads"
	tunneladapter "github.com/Beam-Network/beam/internal/workload/adapters/tunnel"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type TunnelMessage struct {
	EventID        string                        `json:"event_id"`
	Type           string                        `json:"type"`
	Assignment     *tunneladapter.Assignment     `json:"assignment,omitempty"`
	RoomAssignment *tunneladapter.RoomAssignment `json:"room_assignment,omitempty"`
}

type TunnelResult struct {
	Type       string            `json:"type"`
	EventID    string            `json:"event_id"`
	WorkloadID string            `json:"workload_id"`
	AttemptID  string            `json:"attempt_id"`
	WorkerID   string            `json:"worker_id"`
	Status     string            `json:"status"`
	Outputs    map[string]string `json:"outputs,omitempty"`
	Error      string            `json:"error,omitempty"`
}

type TunnelConnector struct {
	config        NATSConfig
	orchestrator  *dispatch.Service
	nats          *natsConnector
	rooms         *roomtransfer.Service
	roomWorkloads *roomworkloads.Manager
}

func (s *TunnelConnector) AttachRoomWorkloads(service *roomworkloads.Manager) {
	s.roomWorkloads = service
}

func NewTunnelConnector(config NATSConfig, orchestrator *dispatch.Service, rooms ...*roomtransfer.Service) (*TunnelConnector, error) {
	if orchestrator == nil {
		return nil, errors.New("Orchestrator orchestration service is required")
	}
	if config.Name == "" {
		config.Name = "beam-orchestrator-tunnel"
	}
	if config.ResultSubject == "" {
		return nil, errors.New("Tunnel result NATS subject is required")
	}
	connector := &TunnelConnector{config: config, orchestrator: orchestrator}
	if len(rooms) > 0 {
		connector.rooms = rooms[0]
		if connector.rooms != nil && config.ProvisionSubject == "" {
			return nil, errors.New("Tunnel provisioning NATS subject is required for room transfers")
		}
	}
	return connector, nil
}

func (s *TunnelConnector) Run(ctx context.Context) error {
	session, err := connectNATS(s.config)
	if err != nil {
		return err
	}
	s.nats = session
	s.orchestrator.RegisterSink(dispatch.SourceTunnel, s)
	if s.rooms != nil {
		s.rooms.RegisterProvisioner(s)
		s.rooms.Replay(ctx)
	}
	if s.roomWorkloads != nil {
		s.roomWorkloads.RegisterProvisioner(s)
		s.roomWorkloads.Replay(ctx)
	}
	defer func() {
		s.orchestrator.RegisterSink(dispatch.SourceTunnel, nil)
		if s.rooms != nil {
			s.rooms.RegisterProvisioner(nil)
		}
		if s.roomWorkloads != nil {
			s.roomWorkloads.RegisterProvisioner(nil)
		}
		session.close()
	}()
	s.orchestrator.ReplayResults(ctx)
	return session.consume(ctx, s.handle)
}

func (s *TunnelConnector) Redeem(ctx context.Context, request roomtransfer.RedeemRequest) (contracts.TunnelLease, error) {
	payload, err := json.Marshal(struct {
		Type string `json:"type"`
		roomtransfer.RedeemRequest
	}{Type: "room_tunnel_lease_redeem", RedeemRequest: request})
	if err != nil {
		return contracts.TunnelLease{}, err
	}
	response, err := s.nats.request(ctx, s.config.ProvisionSubject, payload)
	if err != nil {
		return contracts.TunnelLease{}, err
	}
	var result struct {
		Accepted     bool                  `json:"accepted"`
		Acknowledged bool                  `json:"acknowledged"`
		Lease        contracts.TunnelLease `json:"lease"`
		Reason       string                `json:"reason"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return contracts.TunnelLease{}, err
	}
	if !result.Accepted && !result.Acknowledged {
		return contracts.TunnelLease{}, errors.New(fallback(result.Reason, "Tunnel coordinator rejected room lease intent"))
	}
	return result.Lease, nil
}

var _ roomtransfer.Provisioner = (*TunnelConnector)(nil)
var _ roomworkloads.Provisioner = (*TunnelConnector)(nil)

func (s *TunnelConnector) RedeemRoomPath(ctx context.Context, request contracts.RoomPathRedemptionRequest) (contracts.RoomPathLease, error) {
	payload, err := json.Marshal(struct {
		Type          string `json:"type"`
		SchemaVersion string `json:"schema_version"`
		contracts.RoomPathRedemptionRequest
	}{Type: "room_workload_path_redeem", SchemaVersion: contracts.RoomWorkloadSchema, RoomPathRedemptionRequest: request})
	if err != nil {
		return contracts.RoomPathLease{}, err
	}
	response, err := s.nats.request(ctx, s.config.ProvisionSubject, payload)
	if err != nil {
		return contracts.RoomPathLease{}, err
	}
	var result struct {
		Admitted bool                    `json:"admitted"`
		Accepted bool                    `json:"accepted"`
		Lease    contracts.RoomPathLease `json:"lease"`
		Reason   string                  `json:"reason"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return contracts.RoomPathLease{}, err
	}
	if !result.Admitted && !result.Accepted {
		return contracts.RoomPathLease{}, errors.New(fallback(result.Reason, "Tunnel rejected room workload path"))
	}
	return result.Lease, nil
}

func (s *TunnelConnector) handle(ctx context.Context, encoded []byte) error {
	var message TunnelMessage
	if err := json.Unmarshal(encoded, &message); err != nil {
		return err
	}
	identity := domain.Identity{}
	var spec domain.Spec
	var err error
	switch message.Type {
	case "tunnel_assignment":
		if message.Assignment == nil {
			return errors.New("Tunnel assignment payload is missing")
		}
		spec, err = tunneladapter.ToWorkload(*message.Assignment, identity)
	case "room_assignment":
		if message.RoomAssignment == nil {
			return errors.New("room assignment payload is missing")
		}
		spec, err = tunneladapter.ToRoomWorkload(*message.RoomAssignment, identity)
	default:
		return errors.New("Tunnel NATS message must contain tunnel_assignment or room_assignment")
	}
	if err != nil {
		return err
	}
	externalID := message.EventID
	if externalID == "" {
		externalID = spec.AttemptID
	}
	_, err = s.orchestrator.Dispatch(ctx, dispatch.DispatchRequest{Source: dispatch.SourceTunnel,
		ExternalID: externalID, Spec: spec})
	return err
}

func (s *TunnelConnector) DeliverResult(ctx context.Context, record dispatch.Record, result domain.Result) error {
	status := "failed"
	if result.State == domain.StateCompleted {
		status = "completed"
	} else if result.State == domain.StateCancelled || result.State == domain.StateExpired {
		status = "cancelled"
	}
	payload, err := json.Marshal(TunnelResult{Type: "assignment_result", EventID: record.ExternalID,
		WorkloadID: result.WorkloadID, AttemptID: result.AttemptID, WorkerID: record.WorkerID,
		Status: status, Outputs: result.Outputs, Error: result.ErrorMessage})
	if err != nil {
		return err
	}
	return acknowledged(ctx, s.nats, s.config.ResultSubject, payload, "Tunnel coordinator")
}
