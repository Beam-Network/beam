package roomworkloads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/orchestrator/roomworkload"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type Provisioner interface {
	RedeemRoomPath(context.Context, contracts.RoomPathRedemptionRequest) (contracts.RoomPathLease, error)
}

type Sink interface {
	SubmitRoomWorkloadProgress(context.Context, contracts.RoomGenericProgress) error
	SubmitRoomWorkloadResult(context.Context, contracts.RoomGenericResult) error
	SubmitRoomWorkloadStatus(context.Context, json.RawMessage) error
	SubmitRoomWorkloadProvisioningResult(context.Context, contracts.RoomWorkloadProvisioningResultWire) error
}

type service[T, R any] = roomworkload.RoomWorkloadService[contracts.RoomWorkloadDefinition[T], contracts.RoomWorkloadDefinition[T],
	contracts.RoomPathIntent, contracts.RoomPathLease, contracts.RoomGenericProgress, R, contracts.RoomGenericResult]

type store[T, R any] = roomworkload.FileStore[contracts.RoomWorkloadDefinition[T], contracts.RoomWorkloadDefinition[T],
	contracts.RoomPathIntent, contracts.RoomPathLease, contracts.RoomGenericProgress, R]

type Manager struct {
	mu          sync.RWMutex
	now         func() time.Time
	dispatcher  *dispatch.Service
	datagram    *service[contracts.DatagramUnitDetails, contracts.DatagramResultDetails]
	message     *service[contracts.MessageUnitDetails, contracts.MessageProgressDetails]
	command     *service[contracts.CommandUnitDetails, contracts.CommandProgressDetails]
	stream      *service[contracts.StreamUnitDetails, contracts.StreamResultDetails]
	media       *service[contracts.MediaUnitDetails, contracts.MediaResultDetails]
	provisioner Provisioner
	sink        Sink
}

func OpenManager(stateDirectory string, dispatcher *dispatch.Service) (*Manager, error) {
	if stateDirectory == "" || dispatcher == nil {
		return nil, errors.New("room workload state directory and dispatcher are required")
	}
	now := time.Now
	datagramStore, err := roomworkload.OpenFileStore[contracts.RoomWorkloadDefinition[contracts.DatagramUnitDetails], contracts.RoomWorkloadDefinition[contracts.DatagramUnitDetails], contracts.RoomPathIntent, contracts.RoomPathLease, contracts.RoomGenericProgress, contracts.DatagramResultDetails](filepath.Join(stateDirectory, "datagram.json"))
	if err != nil {
		return nil, err
	}
	messageStore, err := roomworkload.OpenFileStore[contracts.RoomWorkloadDefinition[contracts.MessageUnitDetails], contracts.RoomWorkloadDefinition[contracts.MessageUnitDetails], contracts.RoomPathIntent, contracts.RoomPathLease, contracts.RoomGenericProgress, contracts.MessageProgressDetails](filepath.Join(stateDirectory, "message.json"))
	if err != nil {
		return nil, err
	}
	commandStore, err := roomworkload.OpenFileStore[contracts.RoomWorkloadDefinition[contracts.CommandUnitDetails], contracts.RoomWorkloadDefinition[contracts.CommandUnitDetails], contracts.RoomPathIntent, contracts.RoomPathLease, contracts.RoomGenericProgress, contracts.CommandProgressDetails](filepath.Join(stateDirectory, "command.json"))
	if err != nil {
		return nil, err
	}
	streamStore, err := roomworkload.OpenFileStore[contracts.RoomWorkloadDefinition[contracts.StreamUnitDetails], contracts.RoomWorkloadDefinition[contracts.StreamUnitDetails], contracts.RoomPathIntent, contracts.RoomPathLease, contracts.RoomGenericProgress, contracts.StreamResultDetails](filepath.Join(stateDirectory, "stream.json"))
	if err != nil {
		return nil, err
	}
	mediaStore, err := roomworkload.OpenFileStore[contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails], contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails], contracts.RoomPathIntent, contracts.RoomPathLease, contracts.RoomGenericProgress, contracts.MediaResultDetails](filepath.Join(stateDirectory, "media.json"))
	if err != nil {
		return nil, err
	}

	datagram, err := roomworkload.NewRoomWorkloadService(roomworkload.Config{Now: now}, dispatcher, datagramStore, newDatagramStrategy())
	if err != nil {
		return nil, err
	}
	message, err := roomworkload.NewRoomWorkloadService(roomworkload.Config{Now: now}, dispatcher, messageStore, newMessageStrategy())
	if err != nil {
		return nil, err
	}
	command, err := roomworkload.NewRoomWorkloadService(roomworkload.Config{Now: now}, dispatcher, commandStore, newCommandStrategy())
	if err != nil {
		return nil, err
	}
	stream, err := roomworkload.NewRoomWorkloadService(roomworkload.Config{Now: now}, dispatcher, streamStore, newStreamStrategy())
	if err != nil {
		return nil, err
	}
	media, err := roomworkload.NewRoomWorkloadService(roomworkload.Config{Now: now}, dispatcher, mediaStore, newMediaStrategy())
	if err != nil {
		return nil, err
	}
	return &Manager{now: now, dispatcher: dispatcher, datagram: datagram, message: message, command: command, stream: stream, media: media}, nil
}

// CapabilityAvailable keeps capability advertisement aligned with live
// placement and provisioning. Adding a strategy does not make it routable
// until a compatible Worker and a room path provisioner are both present.
func (m *Manager) CapabilityAvailable(kind domain.Kind) bool {
	return m.CapacityCapabilityAvailable(kind, string(kind))
}

// CapacityCapabilityAvailable reports whether the Orchestrator can place a
// workload requiring a specific, optionally versioned Worker capability.
func (m *Manager) CapacityCapabilityAvailable(kind domain.Kind, capability string) bool {
	if m == nil || !contracts.IsLogicalRoomWorkloadKind(kind) || m.currentProvisioner() == nil {
		return false
	}
	required := []string{string(kind)}
	if capability != "" && capability != string(kind) {
		required = append(required, capability)
	}
	_, err := m.dispatcher.SelectWorker(required, domain.Resources{Connections: 1, Streams: 1}, nil)
	return err == nil
}

func (m *Manager) RegisterProvisioner(value Provisioner) {
	m.mu.Lock()
	m.provisioner = value
	m.mu.Unlock()
	m.datagram.RegisterProvisioner(pathProvisioner[contracts.DatagramUnitDetails, contracts.DatagramResultDetails]{manager: m})
	m.message.RegisterProvisioner(pathProvisioner[contracts.MessageUnitDetails, contracts.MessageProgressDetails]{manager: m})
	m.command.RegisterProvisioner(pathProvisioner[contracts.CommandUnitDetails, contracts.CommandProgressDetails]{manager: m})
	m.stream.RegisterProvisioner(pathProvisioner[contracts.StreamUnitDetails, contracts.StreamResultDetails]{manager: m})
	m.media.RegisterProvisioner(pathProvisioner[contracts.MediaUnitDetails, contracts.MediaResultDetails]{manager: m})
}

func (m *Manager) RegisterSink(value Sink) {
	m.mu.Lock()
	m.sink = value
	m.mu.Unlock()
	m.datagram.RegisterSink(resultSink[contracts.DatagramUnitDetails, contracts.DatagramResultDetails]{manager: m})
	m.message.RegisterSink(resultSink[contracts.MessageUnitDetails, contracts.MessageProgressDetails]{manager: m})
	m.command.RegisterSink(resultSink[contracts.CommandUnitDetails, contracts.CommandProgressDetails]{manager: m})
	m.stream.RegisterSink(resultSink[contracts.StreamUnitDetails, contracts.StreamResultDetails]{manager: m})
	m.media.RegisterSink(resultSink[contracts.MediaUnitDetails, contracts.MediaResultDetails]{manager: m})
	m.datagram.RegisterProgressSink(progressSink[contracts.DatagramUnitDetails, contracts.DatagramResultDetails]{manager: m})
	m.message.RegisterProgressSink(progressSink[contracts.MessageUnitDetails, contracts.MessageProgressDetails]{manager: m})
	m.command.RegisterProgressSink(progressSink[contracts.CommandUnitDetails, contracts.CommandProgressDetails]{manager: m})
	m.stream.RegisterProgressSink(progressSink[contracts.StreamUnitDetails, contracts.StreamResultDetails]{manager: m})
	m.media.RegisterProgressSink(progressSink[contracts.MediaUnitDetails, contracts.MediaResultDetails]{manager: m})
}

func (m *Manager) Submit(ctx context.Context, encoded []byte) error {
	var envelope struct {
		Type          string      `json:"type"`
		SchemaVersion string      `json:"schema_version"`
		Kind          domain.Kind `json:"kind"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return err
	}
	if envelope.SchemaVersion != contracts.RoomWorkloadSchema {
		return errors.New("unsupported room workload schema_version")
	}
	if envelope.Type != contracts.RoomWorkloadOffer && envelope.Type != contracts.RoomWorkloadSubmit {
		return errors.New("generic room workload message must be submit or offer")
	}
	if err := requireCanonicalDetails(encoded, envelope.Type, envelope.Kind); err != nil {
		return err
	}
	if envelope.Type == contracts.RoomWorkloadOffer {
		return m.submitOffer(ctx, envelope.Kind, encoded)
	}
	return m.submitDefinition(ctx, envelope.Kind, encoded)
}

func requireCanonicalDetails(encoded []byte, messageType string, kind domain.Kind) error {
	var envelope struct {
		Details map[string]json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return err
	}
	var fields []string
	switch kind {
	case domain.KindRoomDatagram:
		fields = []string{"ttl_ms", "max_packet_bytes"}
	case domain.KindRoomMessage:
		fields = []string{"message_id", "content_type", "size_bytes"}
	case domain.KindRoomCommand:
		fields = []string{"command_id", "command", "request", "timeout_ms"}
	case domain.KindRoomStream:
		if messageType == contracts.RoomWorkloadOffer {
			fields = []string{"session_id", "resume_from_sequence", "replay", "protocol", "backpressure_policy", "max_buffer_bytes"}
		} else {
			fields = []string{"session_id", "protocol", "backpressure_policy", "max_buffer_bytes", "heartbeat_timeout_ms"}
		}
	case domain.KindRoomMedia:
		if messageType == contracts.RoomWorkloadOffer {
			fields = []string{"session_id", "resume_from_sequence", "replay", "service", "tracks", "layers"}
		} else {
			fields = []string{"session_id", "service", "tracks", "layers", "heartbeat_timeout_ms"}
		}
	default:
		return fmt.Errorf("unsupported generic room workload kind %s", kind)
	}
	for _, field := range fields {
		value, ok := envelope.Details[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("canonical %s details require %s", kind, field)
		}
	}
	return nil
}

func (m *Manager) submitOffer(ctx context.Context, kind domain.Kind, encoded []byte) error {
	switch kind {
	case domain.KindRoomDatagram:
		var value contracts.RoomWorkloadOfferWire[contracts.DatagramUnitDetails]
		if err := decodeCanonical(encoded, &value); err != nil {
			return err
		}
		if err := value.Validate(m.now()); err != nil {
			return err
		}
		return submitTyped(ctx, m, m.datagram, fromOffer(value))
	case domain.KindRoomMessage:
		var value contracts.RoomWorkloadOfferWire[contracts.MessageUnitDetails]
		if err := decodeCanonical(encoded, &value); err != nil {
			return err
		}
		if err := value.Validate(m.now()); err != nil {
			return err
		}
		return submitTyped(ctx, m, m.message, fromOffer(value))
	case domain.KindRoomCommand:
		var value contracts.RoomWorkloadOfferWire[contracts.CommandUnitDetails]
		if err := decodeCanonical(encoded, &value); err != nil {
			return err
		}
		if err := value.Validate(m.now()); err != nil {
			return err
		}
		return submitTyped(ctx, m, m.command, fromOffer(value))
	case domain.KindRoomStream:
		var value contracts.RoomWorkloadOfferWire[contracts.StreamUnitDetails]
		if err := decodeCanonical(encoded, &value); err != nil {
			return err
		}
		if err := value.Validate(m.now()); err != nil {
			return err
		}
		return submitTyped(ctx, m, m.stream, fromOffer(value))
	case domain.KindRoomMedia:
		var value contracts.RoomWorkloadOfferWire[contracts.MediaUnitDetails]
		if err := decodeCanonical(encoded, &value); err != nil {
			return err
		}
		if err := value.Validate(m.now()); err != nil {
			return err
		}
		return submitTyped(ctx, m, m.media, fromOffer(value))
	default:
		return fmt.Errorf("unsupported generic room workload kind %s", kind)
	}
}

func (m *Manager) submitDefinition(ctx context.Context, kind domain.Kind, encoded []byte) error {
	switch kind {
	case domain.KindRoomDatagram:
		var value contracts.RoomWorkloadSubmitWire[contracts.DatagramDefinitionDetails]
		if err := decodeCanonical(encoded, &value); err != nil {
			return err
		}
		if err := value.Validate(m.now()); err != nil {
			return err
		}
		details := contracts.DatagramUnitDetails{TTLMS: value.Details.TTLMS, MaxPacketBytes: value.Details.MaxPacketBytes}
		return submitTyped(ctx, m, m.datagram, fromSubmit(value, details))
	case domain.KindRoomMessage:
		var value contracts.RoomWorkloadSubmitWire[contracts.MessageDefinitionDetails]
		if err := decodeCanonical(encoded, &value); err != nil {
			return err
		}
		if err := value.Validate(m.now()); err != nil {
			return err
		}
		details := contracts.MessageUnitDetails{MessageID: value.Details.MessageID, ContentType: value.Details.ContentType,
			SizeBytes: value.Details.SizeBytes}
		return submitTyped(ctx, m, m.message, fromSubmit(value, details))
	case domain.KindRoomCommand:
		var value contracts.RoomWorkloadSubmitWire[contracts.CommandDefinitionDetails]
		if err := decodeCanonical(encoded, &value); err != nil {
			return err
		}
		if err := value.Validate(m.now()); err != nil {
			return err
		}
		details := contracts.CommandUnitDetails{CommandID: value.Details.CommandID, Command: value.Details.Command,
			Request: value.Details.Request, TimeoutMS: value.Details.TimeoutMS}
		return submitTyped(ctx, m, m.command, fromSubmit(value, details))
	case domain.KindRoomStream:
		var value contracts.RoomWorkloadSubmitWire[contracts.StreamDefinitionDetails]
		if err := decodeCanonical(encoded, &value); err != nil {
			return err
		}
		if err := value.Validate(m.now()); err != nil {
			return err
		}
		if value.Details.HeartbeatTimeoutMS <= 0 || value.Details.HeartbeatTimeoutMS > 300_000 {
			return errors.New("invalid room.stream heartbeat_timeout_ms")
		}
		details := contracts.StreamUnitDetails{SessionID: value.Details.SessionID, Protocol: value.Details.Protocol,
			BackpressurePolicy: value.Details.BackpressurePolicy, MaxBufferBytes: value.Details.MaxBufferBytes}
		return submitTyped(ctx, m, m.stream, fromSubmit(value, details))
	case domain.KindRoomMedia:
		var value contracts.RoomWorkloadSubmitWire[contracts.MediaDefinitionDetails]
		if err := decodeCanonical(encoded, &value); err != nil {
			return err
		}
		if err := value.Validate(m.now()); err != nil {
			return err
		}
		if value.Details.HeartbeatTimeoutMS <= 0 || value.Details.HeartbeatTimeoutMS > 300_000 {
			return errors.New("invalid room.media heartbeat_timeout_ms")
		}
		details := contracts.MediaUnitDetails{SessionID: value.Details.SessionID, Service: value.Details.Service, Profile: value.Details.Profile,
			Tracks: value.Details.Tracks, Layers: value.Details.Layers, Protection: value.Details.Protection}
		return submitTyped(ctx, m, m.media, fromSubmit(value, details))
	default:
		return fmt.Errorf("unsupported generic room workload kind %s", kind)
	}
}

func submitTyped[T, R any](ctx context.Context, manager *Manager, target *service[T, R], definition contracts.RoomWorkloadDefinition[T]) error {
	if err := target.Submit(ctx, roomworkload.RoomWorkloadDefinition[contracts.RoomWorkloadDefinition[T]]{
		ID: definition.Identity.WorkloadID, RoomID: definition.Identity.RoomID,
		OfferExpiresAt: definition.Identity.ExpiresAt, Workload: definition,
	}); err != nil {
		return err
	}
	return manager.publishStatus(ctx, definition.Identity, definition.Targets, contracts.RoomRunning, initialStatusDetails(definition.Details, len(definition.Targets)))
}

func (m *Manager) Cancel(ctx context.Context, cancel contracts.RoomWorkloadCancelWire) error {
	if err := cancel.Validate(); err != nil {
		return err
	}
	var err error
	var status statusContext
	switch cancel.Kind {
	case domain.KindRoomDatagram:
		status = recordStatus(m.datagram, cancel.WorkloadID)
		err = m.datagram.Cancel(ctx, cancel.WorkloadID, "cancelled by BeamCore")
	case domain.KindRoomMessage:
		status = recordStatus(m.message, cancel.WorkloadID)
		err = m.message.Cancel(ctx, cancel.WorkloadID, "cancelled by BeamCore")
	case domain.KindRoomCommand:
		status = recordStatus(m.command, cancel.WorkloadID)
		err = m.command.Cancel(ctx, cancel.WorkloadID, "cancelled by BeamCore")
	case domain.KindRoomStream:
		status = recordStatus(m.stream, cancel.WorkloadID)
		err = m.stream.Cancel(ctx, cancel.WorkloadID, "cancelled by BeamCore")
	case domain.KindRoomMedia:
		status = recordStatus(m.media, cancel.WorkloadID)
		err = m.media.Cancel(ctx, cancel.WorkloadID, "cancelled by BeamCore")
	default:
		return fmt.Errorf("unsupported generic room workload kind %s", cancel.Kind)
	}
	if err != nil {
		return err
	}
	if m.currentSink() != nil {
		return m.publishStatus(ctx, status.identity, status.targets, contracts.RoomCancelled, status.details)
	}
	return nil
}

type statusContext struct {
	identity contracts.RoomWorkloadIdentity
	targets  []contracts.RoomInternalTarget
	details  any
}

func recordStatus[T, R any](target *service[T, R], id string) statusContext {
	record, ok := target.Record(id)
	if !ok {
		return statusContext{}
	}
	definition := record.Definition.Workload
	return statusContext{identity: definition.Identity, targets: definition.Targets,
		details: initialStatusDetails(definition.Details, len(definition.Targets))}
}

func initialStatusDetails(value any, targets int) any {
	switch details := value.(type) {
	case contracts.DatagramUnitDetails:
		return contracts.DatagramResultDetails{}
	case contracts.MessageUnitDetails:
		return contracts.MessageStatusDetails{Pending: int64(targets)}
	case contracts.CommandUnitDetails:
		return contracts.CommandStatusDetails{Pending: int64(targets)}
	case contracts.StreamUnitDetails:
		return contracts.StreamStatusDetails{Sequence: details.ResumeFromSequence, BackpressurePolicy: details.BackpressurePolicy}
	case contracts.MediaUnitDetails:
		tracks := make([]contracts.MediaTrackCounters, 0, len(details.Tracks))
		for _, track := range details.Tracks {
			tracks = append(tracks, contracts.MediaTrackCounters{TrackID: track.TrackID})
		}
		return contracts.MediaProgressDetails{Sequence: details.ResumeFromSequence, Tracks: tracks}
	default:
		return map[string]any{}
	}
}

func (m *Manager) publishStatus(ctx context.Context, identity contracts.RoomWorkloadIdentity,
	targets []contracts.RoomInternalTarget, state contracts.RoomLifecycleState, details any) error {
	sink := m.currentSink()
	if sink == nil {
		return nil
	}
	destinations := make([]contracts.RoomDestinationStatusWire, 0, len(targets))
	for _, target := range targets {
		destinationState := contracts.DestinationActive
		if state == contracts.RoomCancelled {
			destinationState = contracts.DestinationDropped
		}
		destinations = append(destinations, contracts.RoomDestinationStatusWire{TargetMemberID: target.MemberID, Status: destinationState, Reason: nil})
	}
	encoded, err := json.Marshal(contracts.RoomWorkloadStatusWire[any]{Type: contracts.RoomWorkloadStatusType,
		SchemaVersion: contracts.RoomWorkloadSchema, Kind: identity.Kind, WorkloadID: identity.WorkloadID,
		Status: state, Epoch: identity.Epoch, Destinations: destinations, Details: details, UpdatedAt: m.now().UTC()})
	if err != nil {
		return err
	}
	return sink.SubmitRoomWorkloadStatus(ctx, encoded)
}

func (m *Manager) Replay(ctx context.Context) {
	m.datagram.Replay(ctx)
	m.message.Replay(ctx)
	m.command.Replay(ctx)
	m.stream.Replay(ctx)
	m.media.Replay(ctx)
}

func decodeCanonical(encoded []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("room workload message contains trailing JSON")
	}
	return nil
}

func fromOffer[T any](value contracts.RoomWorkloadOfferWire[T]) contracts.RoomWorkloadDefinition[T] {
	identity := contracts.RoomWorkloadIdentity{WorkloadID: value.WorkloadID, Kind: value.Kind, RoomID: value.RoomID,
		ChannelID: value.ChannelID, SourceMemberID: value.SourceMemberID, TargetSnapshot: value.DestinationSnapshot.SnapshotVersion,
		AuthorizationEpoch: value.AuthorizationEpoch, PlanEpoch: value.PlanEpoch, UnitID: value.UnitID,
		Epoch: value.Epoch, Attempt: value.Attempt, ExpiresAt: value.OfferExpiresAt}
	return internalDefinition(identity, value.RequiredCapacity, value.DestinationSnapshot.Targets, "", value.Details, &value.PathAuthorizations)
}

func fromSubmit[D, T any](value contracts.RoomWorkloadSubmitWire[D], details T) contracts.RoomWorkloadDefinition[T] {
	identity := contracts.RoomWorkloadIdentity{WorkloadID: value.WorkloadID, Kind: value.Kind, RoomID: value.RoomID,
		ChannelID: value.ChannelID, SourceMemberID: value.SourceMemberID, TargetSnapshot: value.PlanEpoch,
		AuthorizationEpoch: value.AuthorizationEpoch, PlanEpoch: value.PlanEpoch, UnitID: value.WorkloadID + "/unit-1",
		Epoch: value.PlanEpoch, Attempt: 1, ExpiresAt: value.ExpiresAt}
	return internalDefinition(identity, value.RequiredCapacity, value.Targets, value.IdempotencyKey, details, nil)
}

func internalDefinition[T any](identity contracts.RoomWorkloadIdentity, capacity contracts.RoomCapacityRequirement,
	targets []contracts.RoomWireTarget, authorization string, details T,
	pathAuthorizations *contracts.RoomPathAuthorizations) contracts.RoomWorkloadDefinition[T] {
	protocol := contracts.RoomPathProtocol(identity.Kind)
	source := contracts.RoomPathIntent{PathID: identity.UnitID + "/source", Role: "source", Protocol: protocol,
		Authorization: authorization, ExpiresAt: identity.ExpiresAt}
	targetAuthorizationByMember := map[string]contracts.RoomPathAuthorization{}
	if pathAuthorizations != nil {
		source = roomPathIntent(pathAuthorizations.Source, authorization)
		for _, target := range pathAuthorizations.Targets {
			targetAuthorizationByMember[target.TargetMemberID] = target
		}
	}
	result := contracts.RoomWorkloadDefinition[T]{Schema: contracts.RoomWorkloadSchema, Identity: identity,
		Source:           source,
		RequiredCapacity: capacity,
		Resources:        domain.Resources{Connections: max(1, capacity.Units), Streams: max(1, capacity.Units)}, Details: details}
	for _, target := range targets {
		path := contracts.RoomPathIntent{PathID: identity.UnitID + "/target/" + target.MemberID, Role: "target",
			TargetMemberID: target.MemberID, Protocol: protocol, Authorization: authorization, ExpiresAt: identity.ExpiresAt}
		if pathAuthorization, ok := targetAuthorizationByMember[target.MemberID]; ok {
			path = roomPathIntent(pathAuthorization, authorization)
		}
		result.Targets = append(result.Targets, contracts.RoomInternalTarget{MemberID: target.MemberID,
			Path: path})
	}
	return result
}

func roomPathIntent(value contracts.RoomPathAuthorization, authorization string) contracts.RoomPathIntent {
	return contracts.RoomPathIntent{PathID: value.PathID, Role: value.Role, TargetMemberID: value.TargetMemberID,
		Protocol: value.Protocol, Authorization: authorization, CoordinatorSignature: value.CoordinatorSignature,
		ExpiresAt: value.ExpiresAt}
}
