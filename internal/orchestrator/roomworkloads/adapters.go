package roomworkloads

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Beam-Network/beam/internal/orchestrator/roomworkload"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type pathProvisioner[T, R any] struct{ manager *Manager }

func (p pathProvisioner[T, R]) Redeem(ctx context.Context, request roomworkload.RedemptionRequest[contracts.RoomWorkloadDefinition[T], contracts.RoomWorkloadDefinition[T], contracts.RoomPathIntent, contracts.RoomPathLease]) (contracts.RoomPathLease, error) {
	provisioner := p.manager.currentProvisioner()
	if provisioner == nil {
		return contracts.RoomPathLease{}, errors.New("generic room path provisioner is not connected")
	}
	identity := request.Definition.Workload.Identity
	lease, err := provisioner.RedeemRoomPath(ctx, contracts.RoomPathRedemptionRequest{Identity: identity,
		WorkerID: request.WorkerID, NodeID: request.NodeID, Path: request.Path.Intent})
	if sink := p.manager.currentSink(); sink != nil {
		var reason *string
		if err != nil {
			value := err.Error()
			if len(value) > 1024 {
				value = value[:1024]
			}
			reason = &value
		}
		_ = sink.SubmitRoomWorkloadProvisioningResult(ctx, contracts.RoomWorkloadProvisioningResultWire{
			Type: contracts.RoomWorkloadProvisioningResult, SchemaVersion: contracts.RoomWorkloadSchema, Kind: identity.Kind,
			WorkloadID: identity.WorkloadID, UnitID: identity.UnitID, Epoch: identity.Epoch, Attempt: identity.Attempt,
			ProvisioningID: request.Path.ID, WorkerID: request.WorkerID, Admitted: err == nil, Reason: reason, ReportedAt: p.manager.now().UTC()})
	}
	return lease, err
}

type resultSink[T, R any] struct{ manager *Manager }

func (s resultSink[T, R]) DeliverRoomWorkloadResult(ctx context.Context, value contracts.RoomGenericResult) error {
	sink := s.manager.currentSink()
	if sink == nil {
		return errors.New("generic room workload sink is not connected")
	}
	if err := sink.SubmitRoomWorkloadResult(ctx, value); err != nil {
		return err
	}
	return s.manager.publishResultStatus(ctx, value)
}

type progressSink[T, R any] struct{ manager *Manager }

func (s progressSink[T, R]) DeliverRoomWorkloadProgress(ctx context.Context, value contracts.RoomGenericProgress) error {
	sink := s.manager.currentSink()
	if sink == nil {
		return nil
	}
	return sink.SubmitRoomWorkloadProgress(ctx, value)
}

func (m *Manager) publishResultStatus(ctx context.Context, value contracts.RoomGenericResult) error {
	status := m.resultStatus(value)
	sink := m.currentSink()
	if sink == nil {
		return nil
	}
	encoded, err := json.Marshal(contracts.RoomWorkloadStatusWire[any]{Type: contracts.RoomWorkloadStatusType,
		SchemaVersion: contracts.RoomWorkloadSchema, Kind: value.Identity.Kind, WorkloadID: value.Identity.WorkloadID,
		Status: status.state, Epoch: value.Identity.Epoch, Destinations: status.destinations,
		Details: status.details, UpdatedAt: m.now().UTC()})
	if err != nil {
		return err
	}
	return sink.SubmitRoomWorkloadStatus(ctx, encoded)
}

type resultStatus struct {
	state        contracts.RoomLifecycleState
	details      any
	destinations []contracts.RoomDestinationStatusWire
}

func (m *Manager) resultStatus(value contracts.RoomGenericResult) resultStatus {
	status := resultStatus{state: value.State, details: map[string]any{}, destinations: make([]contracts.RoomDestinationStatusWire, 0)}
	switch value.Identity.Kind {
	case domain.KindRoomDatagram:
		var result contracts.DatagramResultDetails
		_ = json.Unmarshal(value.Details, &result)
		status.details = result
	case domain.KindRoomMessage:
		status = messageResultStatus(status, value.Details)
	case domain.KindRoomCommand:
		status = m.commandResultStatus(status, value)
	case domain.KindRoomStream:
		status = m.streamResultStatus(status, value)
	case domain.KindRoomMedia:
		status = mediaResultStatus(status, value.Details)
	}
	return status
}

func messageResultStatus(status resultStatus, encoded json.RawMessage) resultStatus {
	var result contracts.MessageProgressDetails
	_ = json.Unmarshal(encoded, &result)
	details := contracts.MessageStatusDetails{}
	for _, delivery := range result.Deliveries {
		destinationState := contracts.DestinationCompleted
		if delivery.State == "failed" {
			details.Failed++
			destinationState = contracts.DestinationFailed
		} else {
			details.Delivered++
		}
		status.destinations = append(status.destinations, contracts.RoomDestinationStatusWire{
			TargetMemberID: delivery.TargetMemberID, Status: destinationState, Reason: delivery.Reason})
	}
	if status.state == contracts.RoomCompleted && details.Failed > 0 {
		status.state = contracts.RoomPartial
		if details.Delivered == 0 {
			status.state = contracts.RoomFailed
		}
	}
	status.details = details
	return status
}

func (m *Manager) commandResultStatus(status resultStatus, value contracts.RoomGenericResult) resultStatus {
	var result contracts.CommandProgressDetails
	_ = json.Unmarshal(value.Details, &result)
	details := contracts.CommandStatusDetails{}
	for _, outcome := range result.Outcomes {
		destinationState := contracts.DestinationActive
		switch outcome.State {
		case "acknowledged":
			details.Acknowledged++
		case "responded":
			details.Responded++
			destinationState = contracts.DestinationCompleted
		case "failed":
			details.Failed++
			destinationState = contracts.DestinationFailed
		}
		status.destinations = append(status.destinations, contracts.RoomDestinationStatusWire{
			TargetMemberID: outcome.TargetMemberID, Status: destinationState, Reason: truncatedReason(outcome.Reason)})
	}
	details.Pending = max(0, int64(len(recordStatus(m.command, value.Identity.WorkloadID).targets))-details.Responded-details.Failed)
	if status.state == contracts.RoomCompleted && (details.Failed > 0 || details.Pending > 0) {
		status.state = contracts.RoomPartial
		if details.Responded == 0 && details.Failed > 0 && details.Pending == 0 {
			status.state = contracts.RoomFailed
		}
	}
	status.details = details
	return status
}

func (m *Manager) streamResultStatus(status resultStatus, value contracts.RoomGenericResult) resultStatus {
	var result contracts.StreamResultDetails
	_ = json.Unmarshal(value.Details, &result)
	policy := "block"
	if record, ok := recordForIdentity(m.stream, value.Identity); ok {
		policy = record.Definition.Workload.Details.BackpressurePolicy
	}
	status.details = contracts.StreamStatusDetails{Sequence: result.Sequence, Bytes: result.Bytes,
		BufferedBytes: result.BufferedBytes, Dropped: result.Dropped, BackpressurePolicy: policy}
	status.state = terminalState(status.state, result.TerminalReason)
	return status
}

func mediaResultStatus(status resultStatus, encoded json.RawMessage) resultStatus {
	var result contracts.MediaResultDetails
	_ = json.Unmarshal(encoded, &result)
	status.details = contracts.MediaProgressDetails{Sequence: result.Sequence, Packets: result.Packets,
		Bytes: result.Bytes, Dropped: result.Dropped, Tracks: result.Tracks}
	status.state = terminalState(status.state, result.TerminalReason)
	return status
}

func terminalState(state contracts.RoomLifecycleState, reason string) contracts.RoomLifecycleState {
	if state == contracts.RoomCompleted && reason == "worker_failed" {
		return contracts.RoomFailed
	}
	if state == contracts.RoomCompleted && reason == "expired" {
		return contracts.RoomExpired
	}
	return state
}

func truncatedReason(reason string) *string {
	if reason == "" {
		return nil
	}
	if len(reason) > 512 {
		reason = reason[:512]
	}
	return &reason
}

func (m *Manager) currentSink() Sink {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sink
}

func (m *Manager) currentProvisioner() Provisioner {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.provisioner
}
