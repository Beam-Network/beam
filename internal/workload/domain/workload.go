package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type Kind string

const (
	KindTransferMultipart  Kind = "transfer.multipart"
	KindTransferRange      Kind = "transfer.range"
	KindTransferDistribute Kind = "transfer.distribute"
	KindActionExecute      Kind = "action.execute"
	KindTunnelHTTP         Kind = "tunnel.http"
	KindTunnelTCP          Kind = "tunnel.tcp"
	KindRoomDatagram       Kind = "room.datagram"
	KindRoomMessage        Kind = "room.message"
	KindRoomCommand        Kind = "room.command"
	KindRoomStream         Kind = "room.stream"
	KindRoomMedia          Kind = "room.media"
	KindRoomTransfer       Kind = "room.transfer"
	KindArtifactPublish    Kind = "artifact.publish"
)

type Class string

const (
	ClassJob     Class = "job"
	ClassSession Class = "session"
	ClassService Class = "service"
)

type State string

const (
	StateOffered          State = "offered"
	StateRejected         State = "rejected"
	StateReserved         State = "reserved"
	StateCommitted        State = "committed"
	StateStarting         State = "starting"
	StateRunning          State = "running"
	StateCompleted        State = "completed"
	StateFailed           State = "failed"
	StateCancelled        State = "cancelled"
	StateExpired          State = "expired"
	StateReceiptCommitted State = "receipt_committed"
)

type Identity struct {
	WorkerID       string `json:"worker_id"`
	OrchestratorID string `json:"orchestrator_id,omitempty"`
	NodeID         string `json:"node_id,omitempty"`
	InstanceID     string `json:"instance_id,omitempty"`
}

func (i Identity) Validate() error {
	if strings.TrimSpace(i.WorkerID) == "" {
		return errors.New("worker_id is required")
	}
	return nil
}

type Source struct {
	System    string `json:"system"`
	Reference string `json:"reference"`
}

type Resources struct {
	CPUMillis     int64 `json:"cpu_millis,omitempty"`
	MemoryBytes   int64 `json:"memory_bytes,omitempty"`
	ScratchBytes  int64 `json:"scratch_bytes,omitempty"`
	BandwidthMbps int64 `json:"bandwidth_mbps,omitempty"`
	Connections   int64 `json:"connections,omitempty"`
	Streams       int64 `json:"streams,omitempty"`
}

func (r Resources) Validate() error {
	if r.CPUMillis < 0 || r.MemoryBytes < 0 || r.ScratchBytes < 0 ||
		r.BandwidthMbps < 0 || r.Connections < 0 || r.Streams < 0 {
		return errors.New("resource requirements cannot be negative")
	}
	return nil
}

func (r Resources) Add(other Resources) Resources {
	return Resources{
		CPUMillis:     r.CPUMillis + other.CPUMillis,
		MemoryBytes:   r.MemoryBytes + other.MemoryBytes,
		ScratchBytes:  r.ScratchBytes + other.ScratchBytes,
		BandwidthMbps: r.BandwidthMbps + other.BandwidthMbps,
		Connections:   r.Connections + other.Connections,
		Streams:       r.Streams + other.Streams,
	}
}

func (r Resources) SubFloor(other Resources) Resources {
	return Resources{
		CPUMillis:     max(0, r.CPUMillis-other.CPUMillis),
		MemoryBytes:   max(0, r.MemoryBytes-other.MemoryBytes),
		ScratchBytes:  max(0, r.ScratchBytes-other.ScratchBytes),
		BandwidthMbps: max(0, r.BandwidthMbps-other.BandwidthMbps),
		Connections:   max(0, r.Connections-other.Connections),
		Streams:       max(0, r.Streams-other.Streams),
	}
}

func (r Resources) Fits(required Resources) bool {
	return r.CPUMillis >= required.CPUMillis &&
		r.MemoryBytes >= required.MemoryBytes &&
		r.ScratchBytes >= required.ScratchBytes &&
		r.BandwidthMbps >= required.BandwidthMbps &&
		r.Connections >= required.Connections &&
		r.Streams >= required.Streams
}

type Lease struct {
	OfferExpiresAt      time.Time `json:"offer_expires_at"`
	AssignmentExpiresAt time.Time `json:"assignment_expires_at,omitempty"`
}

type SecurityPolicy struct {
	NetworkTargets []string `json:"network_targets,omitempty"`
	Permissions    []string `json:"permissions,omitempty"`
	TrustProfile   string   `json:"trust_profile,omitempty"`
}

type EvidencePolicy struct {
	ReceiptRequired bool     `json:"receipt_required"`
	Commitments     []string `json:"commitments,omitempty"`
}

type Spec struct {
	WorkloadID           string          `json:"workload_id"`
	AttemptID            string          `json:"attempt_id"`
	Identity             Identity        `json:"identity"`
	Kind                 Kind            `json:"kind"`
	Class                Class           `json:"class"`
	Source               Source          `json:"source"`
	RequiredCapabilities []string        `json:"required_capabilities,omitempty"`
	Resources            Resources       `json:"resources"`
	Lease                Lease           `json:"lease"`
	Security             SecurityPolicy  `json:"security,omitempty"`
	Evidence             EvidencePolicy  `json:"evidence,omitempty"`
	Payload              json.RawMessage `json:"payload"`
}

func (s Spec) Key() string {
	return s.WorkloadID + "/" + s.AttemptID
}

func (s Spec) Validate(now time.Time) error {
	if strings.TrimSpace(s.WorkloadID) == "" {
		return errors.New("workload_id is required")
	}
	if strings.TrimSpace(s.AttemptID) == "" {
		return errors.New("attempt_id is required")
	}
	if err := s.Identity.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(string(s.Kind)) == "" {
		return errors.New("workload kind is required")
	}
	if s.Class != ClassJob && s.Class != ClassSession && s.Class != ClassService {
		return fmt.Errorf("unsupported workload class %q", s.Class)
	}
	if err := s.Resources.Validate(); err != nil {
		return err
	}
	if s.Lease.OfferExpiresAt.IsZero() {
		return errors.New("offer expiration is required")
	}
	if !now.Before(s.Lease.OfferExpiresAt) {
		return errors.New("workload offer has expired")
	}
	if len(bytes.TrimSpace(s.Payload)) == 0 || !json.Valid(s.Payload) {
		return errors.New("workload payload must be valid JSON")
	}
	if slices.Contains(s.RequiredCapabilities, "") {
		return errors.New("required capabilities cannot contain an empty value")
	}
	return nil
}

type Commit struct {
	WorkloadID          string    `json:"workload_id"`
	AttemptID           string    `json:"attempt_id"`
	PlanVersion         uint64    `json:"plan_version"`
	AssignmentToken     string    `json:"assignment_token"`
	AssignmentExpiresAt time.Time `json:"assignment_expires_at"`
}

func (c Commit) Key() string { return c.WorkloadID + "/" + c.AttemptID }

func (c Commit) Validate(now time.Time) error {
	if strings.TrimSpace(c.WorkloadID) == "" || strings.TrimSpace(c.AttemptID) == "" {
		return errors.New("workload_id and attempt_id are required")
	}
	if c.PlanVersion == 0 {
		return errors.New("plan_version must be positive")
	}
	if strings.TrimSpace(c.AssignmentToken) == "" {
		return errors.New("assignment token is required")
	}
	if c.AssignmentExpiresAt.IsZero() || !now.Before(c.AssignmentExpiresAt) {
		return errors.New("assignment has expired")
	}
	return nil
}

type Result struct {
	WorkloadID     string            `json:"workload_id"`
	AttemptID      string            `json:"attempt_id"`
	State          State             `json:"state"`
	BytesProcessed int64             `json:"bytes_processed,omitempty"`
	Outputs        map[string]string `json:"outputs,omitempty"`
	ErrorCode      string            `json:"error_code,omitempty"`
	ErrorMessage   string            `json:"error_message,omitempty"`
	StartedAt      time.Time         `json:"started_at"`
	CompletedAt    time.Time         `json:"completed_at"`
}

type Progress struct {
	WorkloadID string            `json:"workload_id"`
	AttemptID  string            `json:"attempt_id"`
	State      State             `json:"state"`
	Outputs    map[string]string `json:"outputs,omitempty"`
	ObservedAt time.Time         `json:"observed_at"`
}

// Checkpoint is a durable, handler-owned resume point. Sequence is monotonic
// for one workload attempt and Schema identifies the typed payload contract.
// Payload must never contain bearer tokens, signing keys, or other secrets.
type Checkpoint struct {
	WorkloadID string            `json:"workload_id"`
	AttemptID  string            `json:"attempt_id"`
	Kind       Kind              `json:"kind"`
	Sequence   uint64            `json:"sequence"`
	Schema     string            `json:"schema"`
	Cursor     map[string]string `json:"cursor,omitempty"`
	Payload    json.RawMessage   `json:"payload,omitempty"`
	ObservedAt time.Time         `json:"observed_at"`
}

func CanTransition(from, to State) bool {
	allowed := map[State][]State{
		StateOffered:   {StateRejected, StateReserved, StateExpired},
		StateReserved:  {StateCommitted, StateCancelled, StateExpired},
		StateCommitted: {StateStarting, StateCancelled, StateExpired},
		StateStarting:  {StateRunning, StateFailed, StateCancelled},
		StateRunning:   {StateCompleted, StateFailed, StateCancelled, StateExpired},
		StateCompleted: {StateReceiptCommitted},
		StateFailed:    {StateReceiptCommitted},
		StateCancelled: {StateReceiptCommitted},
		StateExpired:   {StateReceiptCommitted},
	}
	return slices.Contains(allowed[from], to)
}
