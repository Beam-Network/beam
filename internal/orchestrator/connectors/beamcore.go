package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/orchestrator/roomtransfer"
	"github.com/Beam-Network/beam/internal/orchestrator/roomworkloads"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type BeamCoreConnector struct {
	config        NATSConfig
	orchestrator  *dispatch.Service
	nats          *natsConnector
	rooms         *roomtransfer.Service
	roomWorkloads *roomworkloads.Manager
	roomControl   *roomControl
	relays        StorageProbeRelayLink
	running       atomic.Bool
}

func NewBeamCoreConnector(config NATSConfig, orchestrator *dispatch.Service) (*BeamCoreConnector, error) {
	if orchestrator == nil {
		return nil, errors.New("Orchestrator orchestration service is required")
	}
	if config.Name == "" {
		config.Name = "beam-orchestrator-beamcore"
	}
	return &BeamCoreConnector{config: config, orchestrator: orchestrator}, nil
}

func (s *BeamCoreConnector) AttachRoomTransfers(service *roomtransfer.Service) { s.rooms = service }
func (s *BeamCoreConnector) AttachRoomWorkloads(service *roomworkloads.Manager) {
	s.roomWorkloads = service
}

func (s *BeamCoreConnector) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("BeamCore control session is already running")
	}
	defer s.running.Store(false)

	session, err := connectNATS(s.config)
	if err != nil {
		return err
	}
	control := newRoomControl(s.config, session.conn, s.rooms, s.roomWorkloads, s.orchestrator)
	control.relays = s.relays
	var controlSession *roomControlSession
	if control.enabled() {
		controlSession, err = control.bind(ctx)
		if err != nil {
			session.close()
			return err
		}
	}

	s.nats = session
	s.roomControl = control
	s.orchestrator.RegisterSink(dispatch.SourceBeamCore, s)
	if s.rooms != nil {
		s.rooms.RegisterSink(s)
	}
	if s.roomWorkloads != nil {
		s.roomWorkloads.RegisterSink(s.roomControl)
	}
	defer func() {
		s.orchestrator.RegisterSink(dispatch.SourceBeamCore, nil)
		if s.rooms != nil {
			s.rooms.RegisterSink(nil)
		}
		if s.roomWorkloads != nil {
			s.roomWorkloads.RegisterSink(nil)
		}
		if controlSession != nil {
			controlSession.close()
		}
		s.nats = nil
		s.roomControl = nil
		session.close()
	}()
	go s.orchestrator.ReplayResults(ctx)
	if s.rooms != nil {
		go s.rooms.Replay(ctx)
	}
	if controlSession != nil {
		if s.config.TaskSubject == "" {
			return controlSession.run(ctx)
		}
		errors := make(chan error, 1)
		go func() { errors <- controlSession.run(ctx) }()
		go func() { errors <- session.consume(ctx, s.handle) }()
		return <-errors
	}
	if s.config.TaskSubject == "" {
		return errors.New("BeamCore orchestrator control is not configured")
	}
	if s.roomWorkloads != nil {
		s.roomWorkloads.Replay(ctx)
	}
	return session.consume(ctx, s.handle)
}

func (s *BeamCoreConnector) handle(ctx context.Context, encoded []byte) error {
	var generic struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return err
	}
	if generic.Type == contracts.RoomWorkloadSubmit || generic.Type == contracts.RoomWorkloadOffer {
		if s.roomWorkloads == nil {
			return errors.New("BeamCore generic room workload service is missing")
		}
		return s.roomWorkloads.Submit(ctx, encoded)
	}
	if generic.Type == contracts.RoomWorkloadCancel {
		if s.roomWorkloads == nil {
			return errors.New("BeamCore generic room workload service is missing")
		}
		var cancel contracts.RoomWorkloadCancelWire
		if err := decodeStrictRoomWire(encoded, &cancel); err != nil {
			return err
		}
		return s.roomWorkloads.Cancel(ctx, cancel)
	}
	return fmt.Errorf("unsupported BeamCore JetStream message type %q", generic.Type)
}

func decodeStrictRoomWire(encoded []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("room workload wire message contains trailing JSON")
	}
	return nil
}

func (s *BeamCoreConnector) SubmitRoomWorkloadProgress(ctx context.Context, value contracts.RoomGenericProgress) error {
	payload, err := encodeRoomWorkloadProgress(value)
	if err != nil {
		return err
	}
	if s.roomControl == nil || !s.roomControl.enabled() {
		return errors.New("BeamCore room workload control is not configured")
	}
	return s.roomControl.submitRoomWorkload(ctx, "room_workload_progress", payload)
}

func (s *BeamCoreConnector) SubmitRoomWorkloadRuntime(ctx context.Context, value contracts.RoomWorkloadRuntimeWire) error {
	if s.roomControl == nil || !s.roomControl.enabled() {
		return errors.New("BeamCore room workload control is not configured")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.roomControl.submitRoomWorkload(ctx, contracts.RoomWorkloadRuntimeType, encoded)
}

func encodeRoomWorkloadProgress(value contracts.RoomGenericProgress) ([]byte, error) {
	if !validRoomWireIdentity(value.Identity) || value.At.IsZero() || !json.Valid(value.Details) {
		return nil, errors.New("room workload progress timestamp or typed details are invalid")
	}
	return json.Marshal(contracts.RoomWorkloadProgressWire[json.RawMessage]{Type: contracts.RoomWorkloadProgressType,
		SchemaVersion: contracts.RoomWorkloadSchema, Kind: value.Identity.Kind, WorkloadID: value.Identity.WorkloadID,
		UnitID: value.Identity.UnitID, Epoch: value.Identity.Epoch, Attempt: value.Identity.Attempt,
		WorkerID: value.Identity.WorkerID, ProgressID: fmt.Sprintf("%s/%d", value.Identity.UnitID, value.At.UnixNano()),
		ReportedAt: value.At.UTC(), Details: value.Details})
}

func (s *BeamCoreConnector) SubmitRoomWorkloadResult(ctx context.Context, value contracts.RoomGenericResult) error {
	payload, err := encodeRoomWorkloadResult(value, time.Now().UTC())
	if err != nil {
		return err
	}
	if s.roomControl == nil || !s.roomControl.enabled() {
		return errors.New("BeamCore room workload control is not configured")
	}
	return s.roomControl.submitRoomWorkload(ctx, "room_workload_result", payload)
}

func encodeRoomWorkloadResult(value contracts.RoomGenericResult, reportedAt time.Time) ([]byte, error) {
	if !validRoomWireIdentity(value.Identity) || reportedAt.IsZero() || !json.Valid(value.Details) {
		return nil, errors.New("room workload result timestamp or typed details are invalid")
	}
	failures := append(make([]contracts.RoomWorkloadFailure, 0, len(value.Failures)), value.Failures...)
	for index := range failures {
		if failures[index].Detail != nil && len(*failures[index].Detail) > 1024 {
			truncated := (*failures[index].Detail)[:1024]
			failures[index].Detail = &truncated
		}
	}
	return json.Marshal(contracts.RoomWorkloadResultWire[json.RawMessage]{Type: contracts.RoomWorkloadResultType,
		SchemaVersion: contracts.RoomWorkloadSchema, Kind: value.Identity.Kind, WorkloadID: value.Identity.WorkloadID,
		UnitID: value.Identity.UnitID, Epoch: value.Identity.Epoch, Attempt: value.Identity.Attempt,
		WorkerID: value.Identity.WorkerID, ResultID: fmt.Sprintf("%s/result/%d", value.Identity.UnitID, value.Identity.Attempt),
		ReportedAt: reportedAt.UTC(), Details: value.Details, Failures: failures})
}

func validRoomWireIdentity(identity contracts.RoomWorkloadIdentity) bool {
	return contracts.IsLogicalRoomWorkloadKind(identity.Kind) && identity.WorkloadID != "" && identity.UnitID != "" &&
		identity.Epoch > 0 && identity.Attempt > 0 && identity.WorkerID != ""
}

func (s *BeamCoreConnector) SubmitRoomWorkloadStatus(ctx context.Context, payload json.RawMessage) error {
	// BeamCore owns and publishes canonical room_workload_status snapshots from
	// accepted progress/results. The Orchestrator control plane has no status
	// ingress subject, so this local derived snapshot is intentionally a no-op.
	return nil
}

func (s *BeamCoreConnector) SubmitRoomWorkloadProvisioningResult(ctx context.Context, value contracts.RoomWorkloadProvisioningResultWire) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if s.roomControl == nil || !s.roomControl.enabled() {
		return errors.New("BeamCore room workload control is not configured")
	}
	return s.roomControl.submitRoomWorkload(ctx, "room_workload_provisioning_result", payload)
}

func (s *BeamCoreConnector) SubmitRoomTaskResult(ctx context.Context, result contracts.RoomTaskResult) error {
	if s.roomControl == nil || !s.roomControl.enabled() {
		return errors.New("BeamCore room control is not configured")
	}
	return s.roomControl.submitResult(ctx, result)
}

func (s *BeamCoreConnector) DeliverResult(ctx context.Context, record dispatch.Record, result domain.Result) error {
	if s.roomControl == nil || !s.roomControl.enabled() {
		return errors.New("BeamCore orchestrator control is not configured")
	}
	return s.roomControl.submitTaskResult(ctx, record, result)
}
