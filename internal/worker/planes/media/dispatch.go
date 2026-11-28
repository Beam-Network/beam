package media

import (
	"context"
	"encoding/json"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

type Dispatch struct {
	Worker runtime.Handler
	Legacy runtime.Handler
}

func (d Dispatch) Kind() domain.Kind { return domain.KindRoomMedia }

func (d Dispatch) Validate(spec domain.Spec) error { return d.route(spec).Validate(spec) }

func (d Dispatch) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	return d.route(spec).Execute(ctx, spec)
}

func (d Dispatch) route(spec domain.Spec) runtime.Handler {
	var payload struct {
		Schema string `json:"schema"`
	}
	if json.Unmarshal(spec.Payload, &payload) == nil && payload.Schema == contracts.RoomWorkloadSchema {
		return d.Worker
	}
	return d.Legacy
}
