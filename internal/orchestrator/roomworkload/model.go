// Package roomworkload contains the workload-agnostic orchestration kernel for
// competitive room workloads. Workload data and proofs remain owned by typed
// strategies at the package boundary.
package roomworkload

import (
	"context"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type AttemptState string

const (
	AttemptPending     AttemptState = "pending"
	AttemptProvisioned AttemptState = "provisioned"
	AttemptDispatched  AttemptState = "dispatched"
	AttemptCompleted   AttemptState = "completed"
	AttemptFailed      AttemptState = "failed"
	AttemptCancelled   AttemptState = "cancelled"
)

type RoomWorkloadDefinition[T any] struct {
	ID             string    `json:"id"`
	RoomID         string    `json:"room_id"`
	OfferExpiresAt time.Time `json:"offer_expires_at"`
	Workload       T         `json:"workload"`
}

type RoomExecutionUnit[T any] struct {
	ID   string `json:"id"`
	Unit T      `json:"unit"`
}

// RoomPath keeps the common routing identity separate from the workload-owned
// intent and credential types. A source path has an empty TargetID.
type RoomPath[I, C any] struct {
	ID         string `json:"id"`
	TargetID   string `json:"target_id,omitempty"`
	Intent     I      `json:"intent"`
	Credential *C     `json:"credential,omitempty"`
	State      string `json:"state,omitempty"`
}

type RoomProgress[T any] struct {
	ObservedAt time.Time `json:"observed_at"`
	Progress   T         `json:"progress"`
}

type RoomResult[T any] struct {
	CompletedAt time.Time `json:"completed_at"`
	Result      T         `json:"result"`
}

type RoomAttempt[U, I, C, P, R any] struct {
	Unit        RoomExecutionUnit[U]      `json:"unit"`
	WorkerID    string                    `json:"worker_id,omitempty"`
	NodeID      string                    `json:"node_id,omitempty"`
	State       AttemptState              `json:"state"`
	Source      RoomPath[I, C]            `json:"source"`
	Targets     map[string]RoomPath[I, C] `json:"targets"`
	WorkloadKey string                    `json:"workload_key,omitempty"`
	Progress    *RoomProgress[P]          `json:"progress,omitempty"`
	Result      *RoomResult[R]            `json:"result,omitempty"`
	Error       string                    `json:"error,omitempty"`
}

type Record[D, U, I, C, P, R any] struct {
	Definition        RoomWorkloadDefinition[D]             `json:"definition"`
	Attempts          map[string]RoomAttempt[U, I, C, P, R] `json:"attempts"`
	UpstreamDelivered bool                                  `json:"upstream_delivered"`
	UpstreamError     string                                `json:"upstream_error,omitempty"`
	CreatedAt         time.Time                             `json:"created_at"`
	UpdatedAt         time.Time                             `json:"updated_at"`
	Lifecycle         string                                `json:"lifecycle,omitempty"`
}

type Store[D, U, I, C, P, R any] interface {
	Get(string) (Record[D, U, I, C, P, R], bool)
	Put(Record[D, U, I, C, P, R]) error
	List() []Record[D, U, I, C, P, R]
}

type RedemptionRequest[D, U, I, C any] struct {
	Definition RoomWorkloadDefinition[D]
	Unit       RoomExecutionUnit[U]
	WorkerID   string
	NodeID     string
	Path       RoomPath[I, C]
}

type Provisioner[D, U, I, C any] interface {
	Redeem(context.Context, RedemptionRequest[D, U, I, C]) (C, error)
}

type ResultSink[O any] interface {
	DeliverRoomWorkloadResult(context.Context, O) error
}

type ProgressSink[P any] interface {
	DeliverRoomWorkloadProgress(context.Context, P) error
}

// Strategy is deliberately typed end-to-end: generic orchestration never
// inspects workload payloads, lease details, progress fields, or proofs.
type Strategy[D, U, I, C, P, R, O any] interface {
	ValidateDefinition(RoomWorkloadDefinition[D], time.Time) error
	SameDefinition(D, D) bool
	ExecutionUnits(RoomWorkloadDefinition[D]) ([]RoomExecutionUnit[U], error)
	Paths(RoomWorkloadDefinition[D], RoomExecutionUnit[U]) (RoomPath[I, C], []RoomPath[I, C], error)
	RequiredCapabilities(RoomWorkloadDefinition[D], RoomExecutionUnit[U]) []string
	Resources(RoomWorkloadDefinition[D], RoomExecutionUnit[U]) domain.Resources
	ValidateCredential(RoomPath[I, C], C, time.Time) error
	BuildWorkerSpec(RoomWorkloadDefinition[D], RoomAttempt[U, I, C, P, R], time.Time) (domain.Spec, error)
	ValidateProgress(RoomWorkloadDefinition[D], RoomAttempt[U, I, C, P, R], domain.Progress) (P, error)
	ValidateResult(RoomWorkloadDefinition[D], RoomAttempt[U, I, C, P, R], domain.Result) (R, error)
	Aggregate(RoomWorkloadDefinition[D], map[string]RoomAttempt[U, I, C, P, R]) (O, bool)
	DispatchSource() dispatch.Source
}
