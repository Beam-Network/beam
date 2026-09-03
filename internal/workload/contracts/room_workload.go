package contracts

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

const RoomWorkloadSchema = "room-workload/v1"

const (
	RoomWorkloadSubmit             = "room_workload_submit"
	RoomWorkloadOffer              = "room_workload_offer"
	RoomWorkloadProgressType       = "room_workload_progress"
	RoomWorkloadResultType         = "room_workload_result"
	RoomWorkloadCancel             = "room_workload_cancel"
	RoomWorkloadStatusType         = "room_workload_status"
	RoomWorkloadProvisioningResult = "room_workload_provisioning_result"
)

type RoomLifecycleState string

const (
	RoomSubmitted    RoomLifecycleState = "submitted"
	RoomPlanning     RoomLifecycleState = "planning"
	RoomProvisioning RoomLifecycleState = "provisioning"
	RoomReady        RoomLifecycleState = "ready"
	RoomRunning      RoomLifecycleState = "running"
	RoomDraining     RoomLifecycleState = "draining"
	RoomCompleted    RoomLifecycleState = "completed"
	RoomPartial      RoomLifecycleState = "partial"
	RoomFailed       RoomLifecycleState = "failed"
	RoomCancelled    RoomLifecycleState = "cancelled"
	RoomExpired      RoomLifecycleState = "expired"
)

type RoomDestinationState string

const (
	DestinationSelected     RoomDestinationState = "selected"
	DestinationProvisioning RoomDestinationState = "provisioning"
	DestinationReady        RoomDestinationState = "ready"
	DestinationActive       RoomDestinationState = "active"
	DestinationCompleted    RoomDestinationState = "completed"
	DestinationDropped      RoomDestinationState = "dropped"
	DestinationUnavailable  RoomDestinationState = "unavailable"
	DestinationFailed       RoomDestinationState = "failed"
)

// The Wire types below mirror BeamCore's canonical flat room-workload/v1
// schemas. Private-internal definitions and path leases are intentionally
// separate and translated only at connector boundaries.
type RoomCapacityRequirement struct {
	Capability string `json:"capability"`
	Units      int64  `json:"units"`
}

type RoomWireTarget struct {
	MemberID string `json:"member_id"`
}

type RoomDestinationSnapshot struct {
	SnapshotVersion uint64           `json:"snapshot_version"`
	Targets         []RoomWireTarget `json:"targets"`
}

type RoomPathAuthorization struct {
	PathID               string    `json:"path_id"`
	Role                 string    `json:"role"`
	TargetMemberID       string    `json:"target_member_id,omitempty"`
	Protocol             string    `json:"protocol"`
	ExpiresAt            time.Time `json:"expires_at"`
	CoordinatorSignature string    `json:"coordinator_signature"`
}

type RoomPathAuthorizations struct {
	Source  RoomPathAuthorization   `json:"source"`
	Targets []RoomPathAuthorization `json:"targets"`
}

type RoomWorkloadSubmitWire[T any] struct {
	Type               string                  `json:"type"`
	SchemaVersion      string                  `json:"schema_version"`
	Kind               domain.Kind             `json:"kind"`
	WorkloadID         string                  `json:"workload_id"`
	IdempotencyKey     string                  `json:"idempotency_key"`
	RoomID             string                  `json:"room_id"`
	ChannelID          string                  `json:"channel_id"`
	SourceMemberID     string                  `json:"source_member_id"`
	Targets            []RoomWireTarget        `json:"targets"`
	AuthorizationEpoch uint64                  `json:"authorization_epoch"`
	PlanEpoch          uint64                  `json:"plan_epoch"`
	RequiredCapacity   RoomCapacityRequirement `json:"required_capacity"`
	ExpiresAt          time.Time               `json:"expires_at"`
	Details            T                       `json:"details"`
}

type RoomWorkloadOfferWire[T any] struct {
	Type                string                  `json:"type"`
	SchemaVersion       string                  `json:"schema_version"`
	Kind                domain.Kind             `json:"kind"`
	WorkloadID          string                  `json:"workload_id"`
	RoomID              string                  `json:"room_id"`
	ChannelID           string                  `json:"channel_id"`
	SourceMemberID      string                  `json:"source_member_id"`
	DestinationSnapshot RoomDestinationSnapshot `json:"destination_snapshot"`
	AuthorizationEpoch  uint64                  `json:"authorization_epoch"`
	PlanEpoch           uint64                  `json:"plan_epoch"`
	UnitID              string                  `json:"unit_id"`
	Epoch               uint64                  `json:"epoch"`
	Attempt             uint64                  `json:"attempt"`
	RequiredCapacity    RoomCapacityRequirement `json:"required_capacity"`
	PathAuthorizations  RoomPathAuthorizations  `json:"path_authorizations"`
	OfferExpiresAt      time.Time               `json:"offer_expires_at"`
	Details             T                       `json:"details"`
}

func (v RoomWorkloadOfferWire[T]) Validate(now time.Time) error {
	if v.Type != RoomWorkloadOffer || v.SchemaVersion != RoomWorkloadSchema || !IsLogicalRoomWorkloadKind(v.Kind) || v.WorkloadID == "" || v.RoomID == "" ||
		v.ChannelID == "" || v.SourceMemberID == "" || v.DestinationSnapshot.SnapshotVersion == 0 ||
		len(v.DestinationSnapshot.Targets) == 0 || len(v.DestinationSnapshot.Targets) > 10_000 ||
		v.AuthorizationEpoch == 0 || v.PlanEpoch == 0 || v.UnitID == "" || v.Epoch == 0 || v.Attempt == 0 ||
		v.RequiredCapacity.Capability == "" || v.RequiredCapacity.Units <= 0 || v.OfferExpiresAt.IsZero() || !now.Before(v.OfferExpiresAt) {
		return errors.New("canonical room workload offer is incomplete or expired")
	}
	seen := make(map[string]struct{}, len(v.DestinationSnapshot.Targets))
	for _, target := range v.DestinationSnapshot.Targets {
		if target.MemberID == "" {
			return errors.New("canonical room workload target is empty")
		}
		if _, duplicate := seen[target.MemberID]; duplicate {
			return fmt.Errorf("duplicate canonical room workload target %s", target.MemberID)
		}
		seen[target.MemberID] = struct{}{}
	}
	if err := v.PathAuthorizations.Validate(v.Kind, v.UnitID, v.DestinationSnapshot.Targets, now); err != nil {
		return err
	}
	return nil
}

func (v RoomPathAuthorizations) Validate(kind domain.Kind, unitID string, targets []RoomWireTarget, now time.Time) error {
	protocol := RoomPathProtocol(kind)
	if err := v.Source.validate(unitID+"/source", "source", "", protocol, now); err != nil {
		return err
	}
	byMember := make(map[string]RoomPathAuthorization, len(v.Targets))
	for _, target := range v.Targets {
		if target.TargetMemberID == "" {
			return errors.New("canonical room workload target path authorization is missing target member")
		}
		if _, duplicate := byMember[target.TargetMemberID]; duplicate {
			return fmt.Errorf("duplicate canonical room workload path authorization %s", target.TargetMemberID)
		}
		byMember[target.TargetMemberID] = target
	}
	if len(byMember) != len(targets) {
		return errors.New("canonical room workload path authorization count mismatch")
	}
	for _, target := range targets {
		authorization, ok := byMember[target.MemberID]
		if !ok {
			return fmt.Errorf("canonical room workload target path authorization missing %s", target.MemberID)
		}
		if err := authorization.validate(unitID+"/target/"+target.MemberID, "target", target.MemberID, protocol, now); err != nil {
			return err
		}
	}
	return nil
}

func RoomPathProtocol(kind domain.Kind) string {
	switch kind {
	case domain.KindRoomDatagram:
		return "btr-datagram"
	case domain.KindRoomMessage, domain.KindRoomCommand:
		return "btr-message"
	case domain.KindRoomStream:
		return "btr-stream"
	case domain.KindRoomMedia:
		return "webrtc"
	default:
		return string(kind)
	}
}

func (v RoomPathAuthorization) validate(pathID, role, targetMemberID, protocol string, now time.Time) error {
	if v.PathID != pathID || v.Role != role || v.TargetMemberID != targetMemberID || v.Protocol != protocol ||
		v.CoordinatorSignature == "" || v.ExpiresAt.IsZero() || !now.Before(v.ExpiresAt) {
		return errors.New("canonical room workload path authorization is incomplete or expired")
	}
	return nil
}

func (v RoomWorkloadSubmitWire[T]) Validate(now time.Time) error {
	if v.Type != RoomWorkloadSubmit || v.SchemaVersion != RoomWorkloadSchema || !IsLogicalRoomWorkloadKind(v.Kind) || v.WorkloadID == "" || v.IdempotencyKey == "" ||
		v.RoomID == "" || v.ChannelID == "" || v.SourceMemberID == "" || len(v.Targets) == 0 || len(v.Targets) > 10_000 ||
		v.AuthorizationEpoch == 0 || v.PlanEpoch == 0 || v.RequiredCapacity.Capability == "" || v.RequiredCapacity.Units <= 0 ||
		v.ExpiresAt.IsZero() || !now.Before(v.ExpiresAt) {
		return errors.New("canonical room workload submission is incomplete or expired")
	}
	seen := make(map[string]struct{}, len(v.Targets))
	for _, target := range v.Targets {
		if target.MemberID == "" {
			return errors.New("canonical room workload target is empty")
		}
		if _, duplicate := seen[target.MemberID]; duplicate {
			return fmt.Errorf("duplicate canonical room workload target %s", target.MemberID)
		}
		seen[target.MemberID] = struct{}{}
	}
	return nil
}

type RoomWorkloadProgressWire[T any] struct {
	Type          string      `json:"type"`
	SchemaVersion string      `json:"schema_version"`
	Kind          domain.Kind `json:"kind"`
	WorkloadID    string      `json:"workload_id"`
	UnitID        string      `json:"unit_id"`
	Epoch         uint64      `json:"epoch"`
	Attempt       uint64      `json:"attempt"`
	WorkerID      string      `json:"worker_id"`
	ProgressID    string      `json:"progress_id"`
	ReportedAt    time.Time   `json:"reported_at"`
	Details       T           `json:"details"`
}

type RoomWorkloadFailure struct {
	Origin    string  `json:"origin"`
	Code      string  `json:"code"`
	Retryable bool    `json:"retryable"`
	Detail    *string `json:"detail"`
}

type RoomWorkloadResultWire[T any] struct {
	Type          string                `json:"type"`
	SchemaVersion string                `json:"schema_version"`
	Kind          domain.Kind           `json:"kind"`
	WorkloadID    string                `json:"workload_id"`
	UnitID        string                `json:"unit_id"`
	Epoch         uint64                `json:"epoch"`
	Attempt       uint64                `json:"attempt"`
	WorkerID      string                `json:"worker_id"`
	ResultID      string                `json:"result_id"`
	ReportedAt    time.Time             `json:"reported_at"`
	Details       T                     `json:"details"`
	Failures      []RoomWorkloadFailure `json:"failures"`
}

type RoomDestinationStatusWire struct {
	TargetMemberID string               `json:"target_member_id"`
	Status         RoomDestinationState `json:"status"`
	Reason         *string              `json:"reason"`
}

type RoomWorkloadStatusWire[T any] struct {
	Type          string                      `json:"type"`
	SchemaVersion string                      `json:"schema_version"`
	Kind          domain.Kind                 `json:"kind"`
	WorkloadID    string                      `json:"workload_id"`
	Status        RoomLifecycleState          `json:"status"`
	Epoch         uint64                      `json:"epoch"`
	Destinations  []RoomDestinationStatusWire `json:"destinations"`
	Details       T                           `json:"details"`
	UpdatedAt     time.Time                   `json:"updated_at"`
}

type RoomWorkloadCancelWire struct {
	Type           string      `json:"type"`
	SchemaVersion  string      `json:"schema_version"`
	Kind           domain.Kind `json:"kind"`
	WorkloadID     string      `json:"workload_id"`
	IdempotencyKey string      `json:"idempotency_key"`
	RequestedAt    time.Time   `json:"requested_at"`
}

func (v RoomWorkloadCancelWire) Validate() error {
	if v.Type != RoomWorkloadCancel || v.SchemaVersion != RoomWorkloadSchema || !IsLogicalRoomWorkloadKind(v.Kind) || v.WorkloadID == "" ||
		v.IdempotencyKey == "" || v.RequestedAt.IsZero() {
		return errors.New("canonical room workload cancellation is incomplete")
	}
	return nil
}

func IsLogicalRoomWorkloadKind(kind domain.Kind) bool {
	switch kind {
	case domain.KindRoomDatagram, domain.KindRoomMessage, domain.KindRoomCommand, domain.KindRoomStream, domain.KindRoomMedia:
		return true
	default:
		return false
	}
}

type RoomWorkloadProvisioningResultWire struct {
	Type           string      `json:"type"`
	SchemaVersion  string      `json:"schema_version"`
	Kind           domain.Kind `json:"kind"`
	WorkloadID     string      `json:"workload_id"`
	UnitID         string      `json:"unit_id"`
	Epoch          uint64      `json:"epoch"`
	Attempt        uint64      `json:"attempt"`
	ProvisioningID string      `json:"provisioning_id"`
	WorkerID       string      `json:"worker_id"`
	Admitted       bool        `json:"admitted"`
	Reason         *string     `json:"reason"`
	ReportedAt     time.Time   `json:"reported_at"`
}

// Private-internal identity and definition models.
type RoomWorkloadIdentity struct {
	WorkloadID         string      `json:"workload_id"`
	Kind               domain.Kind `json:"kind"`
	RoomID             string      `json:"room_id"`
	ChannelID          string      `json:"channel_id"`
	SourceMemberID     string      `json:"source_member_id"`
	TargetSnapshot     uint64      `json:"target_snapshot"`
	AuthorizationEpoch uint64      `json:"authorization_epoch"`
	PlanEpoch          uint64      `json:"plan_epoch"`
	UnitID             string      `json:"unit_id"`
	Epoch              uint64      `json:"epoch"`
	Attempt            uint64      `json:"attempt"`
	WorkerID           string      `json:"worker_id,omitempty"`
	ExpiresAt          time.Time   `json:"expiry"`
}

func (i RoomWorkloadIdentity) Validate(kind domain.Kind, now time.Time) error {
	if i.WorkloadID == "" || i.RoomID == "" || i.ChannelID == "" || i.SourceMemberID == "" || i.TargetSnapshot == 0 ||
		i.UnitID == "" || i.AuthorizationEpoch == 0 || i.PlanEpoch == 0 || i.Epoch == 0 || i.Attempt == 0 {
		return errors.New("room workload identity is incomplete")
	}
	if i.Kind != kind {
		return fmt.Errorf("room workload kind %s does not match %s", i.Kind, kind)
	}
	if i.ExpiresAt.IsZero() || !now.Before(i.ExpiresAt) {
		return errors.New("room workload has expired")
	}
	return nil
}

type RoomPathIntent struct {
	PathID               string    `json:"path_id"`
	Role                 string    `json:"role"`
	TargetMemberID       string    `json:"target_member_id,omitempty"`
	Protocol             string    `json:"protocol"`
	Authorization        string    `json:"authorization"`
	CoordinatorSignature string    `json:"coordinator_signature"`
	ExpiresAt            time.Time `json:"expires_at"`
}

type RoomPathLease struct {
	PathID         string         `json:"path_id"`
	Role           string         `json:"role"`
	TargetMemberID string         `json:"target_member_id,omitempty"`
	Protocol       string         `json:"protocol"`
	Endpoints      []HTTPEndpoint `json:"endpoints"`
	ExpiresAt      time.Time      `json:"expires_at"`
}

func (l RoomPathLease) Validate(intent RoomPathIntent, now time.Time) error {
	if l.PathID != intent.PathID || l.Role != intent.Role || l.TargetMemberID != intent.TargetMemberID ||
		l.Protocol != intent.Protocol || l.ExpiresAt.IsZero() || !now.Before(l.ExpiresAt) {
		return errors.New("provisioned room path does not match its intent or has expired")
	}
	if len(l.Endpoints) == 0 || len(l.Endpoints) > 8 {
		return errors.New("room path lease requires between 1 and 8 endpoints")
	}
	for _, endpoint := range l.Endpoints {
		parsed, err := url.Parse(endpoint.URL)
		if err != nil || parsed.Host == "" || !slices.Contains([]string{"http", "https"}, parsed.Scheme) {
			return errors.New("room path endpoint must be absolute HTTP(S)")
		}
	}
	return nil
}

type RoomInternalTarget struct {
	MemberID string         `json:"member_id"`
	Path     RoomPathIntent `json:"path"`
}

type RoomWorkloadDefinition[T any] struct {
	Schema           string                  `json:"schema"`
	Identity         RoomWorkloadIdentity    `json:"identity"`
	Source           RoomPathIntent          `json:"source"`
	Targets          []RoomInternalTarget    `json:"targets"`
	RequiredCapacity RoomCapacityRequirement `json:"required_capacity"`
	Resources        domain.Resources        `json:"resources"`
	Details          T                       `json:"details"`
}

type RoomWorkerTarget struct {
	MemberID string        `json:"member_id"`
	Path     RoomPathLease `json:"path"`
}

type RoomWorkerSpec[T any] struct {
	Schema   string               `json:"schema"`
	Identity RoomWorkloadIdentity `json:"identity"`
	Source   RoomPathLease        `json:"source"`
	Targets  []RoomWorkerTarget   `json:"targets"`
	Details  T                    `json:"details"`
}

type RoomPathRedemptionRequest struct {
	Identity RoomWorkloadIdentity `json:"identity"`
	WorkerID string               `json:"worker_id"`
	NodeID   string               `json:"node_id"`
	Path     RoomPathIntent       `json:"path"`
}

type RoomGenericProgress struct {
	Identity RoomWorkloadIdentity `json:"identity"`
	Details  json.RawMessage      `json:"details"`
	At       time.Time            `json:"at"`
}

type RoomGenericResult struct {
	Identity RoomWorkloadIdentity  `json:"identity"`
	State    RoomLifecycleState    `json:"state"`
	Details  json.RawMessage       `json:"details"`
	Failures []RoomWorkloadFailure `json:"failures"`
}

// Canonical typed strategy details.
type DatagramDefinitionDetails struct {
	TTLMS          int64 `json:"ttl_ms"`
	MaxPacketBytes int   `json:"max_packet_bytes"`
}
type DatagramUnitDetails struct {
	TTLMS          int64 `json:"ttl_ms"`
	MaxPacketBytes int   `json:"max_packet_bytes"`
}
type DatagramProgressDetails struct {
	PacketsDelta int64 `json:"packets_delta"`
	BytesDelta   int64 `json:"bytes_delta"`
	DroppedDelta int64 `json:"dropped_delta"`
}
type DatagramResultDetails struct {
	Packets int64 `json:"packets"`
	Bytes   int64 `json:"bytes"`
	Dropped int64 `json:"dropped"`
}

type MessageDefinitionDetails struct {
	MessageID   string `json:"message_id"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}
type MessageUnitDetails struct {
	MessageID   string `json:"message_id"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}
type MessageDelivery struct {
	TargetMemberID string  `json:"target_member_id"`
	State          string  `json:"state"`
	Reason         *string `json:"reason"`
}
type MessageProgressDetails struct {
	Deliveries []MessageDelivery `json:"deliveries"`
}
type MessageStatusDetails struct {
	Delivered int64 `json:"delivered"`
	Failed    int64 `json:"failed"`
	Pending   int64 `json:"pending"`
}

type CommandDefinitionDetails struct {
	CommandID string         `json:"command_id"`
	Command   string         `json:"command"`
	Request   map[string]any `json:"request"`
	TimeoutMS int64          `json:"timeout_ms"`
}
type CommandUnitDetails struct {
	CommandID string         `json:"command_id"`
	Command   string         `json:"command"`
	Request   map[string]any `json:"request"`
	TimeoutMS int64          `json:"timeout_ms"`
}
type CommandOutcome struct {
	TargetMemberID string         `json:"target_member_id"`
	State          string         `json:"state"`
	Response       map[string]any `json:"response,omitempty"`
	Reason         string         `json:"reason,omitempty"`
}

func (value CommandOutcome) MarshalJSON() ([]byte, error) {
	switch value.State {
	case "acknowledged":
		return json.Marshal(struct {
			TargetMemberID string `json:"target_member_id"`
			State          string `json:"state"`
		}{value.TargetMemberID, value.State})
	case "responded":
		return json.Marshal(struct {
			TargetMemberID string         `json:"target_member_id"`
			State          string         `json:"state"`
			Response       map[string]any `json:"response"`
		}{value.TargetMemberID, value.State, value.Response})
	case "failed":
		return json.Marshal(struct {
			TargetMemberID string `json:"target_member_id"`
			State          string `json:"state"`
			Reason         string `json:"reason"`
		}{value.TargetMemberID, value.State, value.Reason})
	default:
		type raw CommandOutcome
		return json.Marshal(raw(value))
	}
}

type CommandProgressDetails struct {
	Outcomes []CommandOutcome `json:"outcomes"`
}
type CommandStatusDetails struct {
	Acknowledged int64 `json:"acknowledged"`
	Responded    int64 `json:"responded"`
	Failed       int64 `json:"failed"`
	Pending      int64 `json:"pending"`
}

type StreamDefinitionDetails struct {
	SessionID          string `json:"session_id"`
	Protocol           string `json:"protocol"`
	BackpressurePolicy string `json:"backpressure_policy"`
	MaxBufferBytes     int64  `json:"max_buffer_bytes"`
	HeartbeatTimeoutMS int64  `json:"heartbeat_timeout_ms"`
}
type StreamUnitDetails struct {
	SessionID          string `json:"session_id"`
	ResumeFromSequence uint64 `json:"resume_from_sequence"`
	Replay             bool   `json:"replay"`
	Protocol           string `json:"protocol"`
	BackpressurePolicy string `json:"backpressure_policy"`
	MaxBufferBytes     int64  `json:"max_buffer_bytes"`
}
type StreamProgressDetails struct {
	Sequence      uint64 `json:"sequence"`
	Bytes         int64  `json:"bytes"`
	BufferedBytes int64  `json:"buffered_bytes"`
	Dropped       int64  `json:"dropped"`
}
type StreamResultDetails struct {
	Sequence       uint64 `json:"sequence"`
	Bytes          int64  `json:"bytes"`
	BufferedBytes  int64  `json:"buffered_bytes"`
	Dropped        int64  `json:"dropped"`
	TerminalReason string `json:"terminal_reason"`
}
type StreamStatusDetails struct {
	Sequence           uint64 `json:"sequence"`
	Bytes              int64  `json:"bytes"`
	BufferedBytes      int64  `json:"buffered_bytes"`
	Dropped            int64  `json:"dropped"`
	BackpressurePolicy string `json:"backpressure_policy"`
}

type MediaTrack struct {
	TrackID string `json:"track_id"`
	Kind    string `json:"kind"`
}
type MediaLayer struct {
	LayerID    string `json:"layer_id"`
	TrackID    string `json:"track_id"`
	BitrateBPS int64  `json:"bitrate_bps"`
}

const (
	RoomMediaLegacyProfile       = "legacy"
	RoomMediaWebRTCWorkerProfile = "webrtc-worker-v1"
	RoomMediaWebRTCCapability    = "room.media.webrtc.v1"
	RoomMediaWHIPCapability      = "room.media.whip.v1"
	RoomMediaWHEPCapability      = "room.media.whep.v1"
	RoomMediaProtectionSchemeV1  = "beam-rtp-aead-v1"
	RoomMediaProtectionChannel   = "channel"
)

type MediaRuntime struct {
	Capability  string    `json:"capability"`
	Transport   string    `json:"transport"`
	BaseURL     string    `json:"base_url"`
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}
type MediaProtection struct {
	Scheme   string `json:"scheme"`
	KeyScope string `json:"key_scope"`
	Required bool   `json:"required"`
}
type MediaDefinitionDetails struct {
	SessionID          string          `json:"session_id"`
	Service            string          `json:"service"`
	Profile            string          `json:"profile,omitempty"`
	Tracks             []MediaTrack    `json:"tracks"`
	Layers             []MediaLayer    `json:"layers"`
	Protection         MediaProtection `json:"protection"`
	HeartbeatTimeoutMS int64           `json:"heartbeat_timeout_ms"`
}
type MediaUnitDetails struct {
	SessionID          string          `json:"session_id"`
	ResumeFromSequence uint64          `json:"resume_from_sequence"`
	Replay             bool            `json:"replay"`
	Service            string          `json:"service"`
	Profile            string          `json:"profile,omitempty"`
	Tracks             []MediaTrack    `json:"tracks"`
	Layers             []MediaLayer    `json:"layers"`
	Protection         MediaProtection `json:"protection"`
}
type MediaTrackCounters struct {
	TrackID string `json:"track_id"`
	Packets int64  `json:"packets"`
	Bytes   int64  `json:"bytes"`
	Dropped int64  `json:"dropped"`
}
type MediaProgressDetails struct {
	Sequence uint64               `json:"sequence"`
	Packets  int64                `json:"packets"`
	Bytes    int64                `json:"bytes"`
	Dropped  int64                `json:"dropped"`
	Tracks   []MediaTrackCounters `json:"tracks"`
	Runtime  *MediaRuntime        `json:"runtime,omitempty"`
}
type MediaResultDetails struct {
	Sequence       uint64               `json:"sequence"`
	Packets        int64                `json:"packets"`
	Bytes          int64                `json:"bytes"`
	Dropped        int64                `json:"dropped"`
	Tracks         []MediaTrackCounters `json:"tracks"`
	Runtime        *MediaRuntime        `json:"runtime,omitempty"`
	TerminalReason string               `json:"terminal_reason"`
}
