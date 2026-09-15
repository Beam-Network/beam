package dispatch

import (
	"context"
	"errors"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

type Source string

const (
	SourceBeamCore     Source = "beamcore"
	SourceStudio       Source = "studio"
	SourceTunnel       Source = "tunnel"
	SourceRoomTransfer Source = "room_transfer"
	SourceRoomDatagram Source = "room_datagram"
	SourceRoomMessage  Source = "room_message"
	SourceRoomCommand  Source = "room_command"
	SourceRoomStream   Source = "room_stream"
	SourceRoomMedia    Source = "room_media"
)

type State string

const (
	StateReceived  State = "received"
	StateOffered   State = "offered"
	StateReserved  State = "reserved"
	StateCommitted State = "committed"
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
	StateRejected  State = "rejected"
)

type Record struct {
	Source                     Source             `json:"source"`
	ExternalID                 string             `json:"external_id"`
	WorkloadKey                string             `json:"workload_key"`
	WorkerID                   string             `json:"worker_id,omitempty"`
	State                      State              `json:"state"`
	Spec                       domain.Spec        `json:"spec"`
	Result                     *domain.Result     `json:"result,omitempty"`
	Progress                   *domain.Progress   `json:"progress,omitempty"`
	Checkpoint                 *domain.Checkpoint `json:"checkpoint,omitempty"`
	UpstreamCheckpoint         *domain.Checkpoint `json:"upstream_checkpoint,omitempty"`
	UpstreamCheckpointSequence uint64             `json:"upstream_checkpoint_sequence,omitempty"`
	UpstreamDelivered          bool               `json:"upstream_delivered"`
	UpstreamError              string             `json:"upstream_error,omitempty"`
	CreatedAt                  time.Time          `json:"created_at"`
	UpdatedAt                  time.Time          `json:"updated_at"`
	AssignmentExpiresAt        time.Time          `json:"assignment_expires_at,omitempty"`
}

func (r Record) Validate() error {
	if r.Source != SourceBeamCore && r.Source != SourceStudio && r.Source != SourceTunnel && r.Source != SourceRoomTransfer &&
		r.Source != SourceRoomDatagram && r.Source != SourceRoomMessage && r.Source != SourceRoomCommand &&
		r.Source != SourceRoomStream && r.Source != SourceRoomMedia {
		return errors.New("unknown external session source")
	}
	if r.ExternalID == "" || r.WorkloadKey == "" {
		return errors.New("external id and workload key are required")
	}
	if r.Spec.Key() != r.WorkloadKey {
		return errors.New("workload key does not match the workload specification")
	}
	return nil
}

type Event struct {
	Sequence    uint64    `json:"sequence"`
	ObservedAt  time.Time `json:"observed_at"`
	Source      Source    `json:"source"`
	ExternalID  string    `json:"external_id"`
	WorkloadKey string    `json:"workload_key"`
	WorkerID    string    `json:"worker_id,omitempty"`
	State       State     `json:"state"`
	Message     string    `json:"message"`
}

type DispatchRequest struct {
	Source       Source
	ExternalID   string
	Spec         domain.Spec
	WorkerID     string
	NodeID       string
	BeforeCommit func(context.Context, Record) error
}
