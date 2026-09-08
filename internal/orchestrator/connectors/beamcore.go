package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/orchestrator/payment"
	"github.com/Beam-Network/beam/internal/orchestrator/roomtransfer"
	"github.com/Beam-Network/beam/internal/orchestrator/roomworkloads"
	beamcoreadapter "github.com/Beam-Network/beam/internal/workload/adapters/beamcore"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type BeamCoreMessage struct {
	EventID string                    `json:"event_id"`
	Type    string                    `json:"type"`
	Offer   beamcoreadapter.TaskOffer `json:"offer"`
}

type BeamCoreResult struct {
	Type             string `json:"type"`
	TaskID           string `json:"task_id"`
	OfferID          string `json:"offer_id"`
	WorkerID         string `json:"worker_id"`
	Success          bool   `json:"success"`
	BytesTransferred int64  `json:"bytes_transferred"`
	ChunkHash        string `json:"chunk_hash,omitempty"`
	ETag             string `json:"etag,omitempty"`
	Error            string `json:"error,omitempty"`
}

type BeamCoreConnector struct {
	config        NATSConfig
	orchestrator  *dispatch.Service
	nats          *natsConnector
	payments      *payment.Service
	rooms         *roomtransfer.Service
	roomWorkloads *roomworkloads.Manager
	roomControl   *roomControl
}

func NewBeamCoreConnector(config NATSConfig, orchestrator *dispatch.Service, payments ...*payment.Service) (*BeamCoreConnector, error) {
	if orchestrator == nil {
		return nil, errors.New("Orchestrator orchestration service is required")
	}
	if config.Name == "" {
		config.Name = "beam-orchestrator-beamcore"
	}
	if config.TaskSubject != "" && config.ResultSubject == "" {
		return nil, errors.New("BeamCore result NATS subject is required")
	}
	connector := &BeamCoreConnector{config: config, orchestrator: orchestrator}
	if len(payments) > 0 {
		connector.payments = payments[0]
	}
	if connector.payments != nil && config.TaskSubject != "" && config.EvidenceSubject == "" {
		return nil, errors.New("BeamCore payment evidence NATS subject is required")
	}
	return connector, nil
}

func (s *BeamCoreConnector) AttachRoomTransfers(service *roomtransfer.Service) { s.rooms = service }
func (s *BeamCoreConnector) AttachRoomWorkloads(service *roomworkloads.Manager) {
	s.roomWorkloads = service
}

func (s *BeamCoreConnector) Run(ctx context.Context) error {
	session, err := connectNATS(s.config)
	if err != nil {
		return err
	}
	s.nats = session
	s.orchestrator.RegisterSink(dispatch.SourceBeamCore, s)
	if s.payments != nil && s.config.TaskSubject != "" {
		s.payments.RegisterSink(s)
	}
	if s.rooms != nil {
		s.rooms.RegisterSink(s)
	}
	s.roomControl = newRoomControl(s.config, session.conn, s.rooms, s.roomWorkloads, s.orchestrator)
	if s.roomWorkloads != nil {
		s.roomWorkloads.RegisterSink(s.roomControl)
	}
	defer func() {
		s.orchestrator.RegisterSink(dispatch.SourceBeamCore, nil)
		if s.payments != nil && s.config.TaskSubject != "" {
			s.payments.RegisterSink(nil)
		}
		if s.rooms != nil {
			s.rooms.RegisterSink(nil)
		}
		if s.roomWorkloads != nil {
			s.roomWorkloads.RegisterSink(nil)
		}
		session.close()
	}()
	go s.orchestrator.ReplayResults(ctx)
	if s.payments != nil && s.config.TaskSubject != "" {
		go s.payments.Replay(ctx)
	}
	if s.rooms != nil {
		go s.rooms.Replay(ctx)
	}
	if s.roomControl != nil && s.roomControl.enabled() {
		if s.config.TaskSubject == "" {
			return s.roomControl.run(ctx)
		}
		errors := make(chan error, 1)
		go func() { errors <- s.roomControl.run(ctx) }()
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

func (s *BeamCoreConnector) SubmitPaymentEvidence(ctx context.Context, proof payment.Proof) error {
	if err := proof.Verify(); err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Type string `json:"type"`
		payment.Proof
	}{Type: "worker_payment_evidence", Proof: proof})
	if err != nil {
		return err
	}
	response, err := s.nats.request(ctx, s.config.EvidenceSubject, payload)
	if err != nil {
		return err
	}
	if len(response) == 0 {
		return errors.New("BeamCore returned an empty payment evidence acknowledgement")
	}
	var acknowledgement struct {
		Accepted   bool   `json:"accepted"`
		Received   bool   `json:"received"`
		EvidenceID string `json:"evidence_id"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(response, &acknowledgement); err != nil {
		return err
	}
	if !acknowledgement.Accepted && !acknowledgement.Received {
		return errors.New(fallback(acknowledgement.Reason, "BeamCore rejected payment evidence"))
	}
	if acknowledgement.EvidenceID != "" && acknowledgement.EvidenceID != proof.EvidenceID {
		return errors.New("BeamCore acknowledged another payment evidence id")
	}
	return nil
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
	var message BeamCoreMessage
	if err := json.Unmarshal(encoded, &message); err != nil {
		return err
	}
	if message.Type != "task_offer" || message.Offer.TaskID == "" {
		return errors.New("BeamCore NATS message must contain task_offer")
	}
	externalID := message.EventID
	if externalID == "" {
		externalID = message.Offer.OfferID
	}
	spec, err := beamcoreadapter.ToWorkload(message.Offer, domain.Identity{}, time.Now())
	if err != nil {
		return err
	}
	_, err = s.orchestrator.Dispatch(ctx, dispatch.DispatchRequest{Source: dispatch.SourceBeamCore,
		ExternalID: externalID, Spec: spec})
	return err
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
	if s.config.TaskSubject == "" {
		if s.roomControl == nil || !s.roomControl.enabled() {
			return errors.New("BeamCore orchestrator control is not configured")
		}
		return s.roomControl.submitTaskResult(ctx, record, result)
	}
	payload, err := json.Marshal(BeamCoreResult{Type: "task_result", TaskID: record.Spec.WorkloadID,
		OfferID: record.Spec.AttemptID, WorkerID: record.WorkerID, Success: result.State == domain.StateCompleted,
		BytesTransferred: result.BytesProcessed, ChunkHash: resultOutput(result.Outputs, "sha256"), ETag: resultOutput(result.Outputs, "etag"),
		Error: result.ErrorMessage})
	if err != nil {
		return err
	}
	response, err := s.nats.request(ctx, s.config.ResultSubject, payload)
	if err != nil {
		return err
	}
	if len(response) == 0 {
		return errors.New("BeamCore returned an empty task result acknowledgement")
	}
	var ack struct {
		Received  bool   `json:"received"`
		Completed bool   `json:"completed"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(response, &ack); err != nil {
		return err
	}
	if !ack.Received {
		return errors.New(fallback(ack.Reason, "BeamCore did not acknowledge task result"))
	}
	return nil
}
