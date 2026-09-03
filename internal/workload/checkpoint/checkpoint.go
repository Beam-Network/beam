package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

var ErrUnavailable = errors.New("workload checkpoint context is unavailable")

type manager struct {
	mu      sync.Mutex
	current *domain.Checkpoint
	save    func(domain.Checkpoint) error
}

type contextKey struct{}

// WithManager installs a workload-scoped checkpoint manager. The initial
// checkpoint is cloned so handlers cannot mutate the durable snapshot.
func WithManager(ctx context.Context, initial *domain.Checkpoint, save func(domain.Checkpoint) error) context.Context {
	return context.WithValue(ctx, contextKey{}, &manager{current: clone(initial), save: save})
}

// Current decodes the most recent checkpoint payload when its schema matches.
func Current(ctx context.Context, schema string, destination any) (domain.Checkpoint, bool, error) {
	value, ok := ctx.Value(contextKey{}).(*manager)
	if !ok {
		return domain.Checkpoint{}, false, nil
	}
	value.mu.Lock()
	defer value.mu.Unlock()
	if value.current == nil || value.current.Schema != schema {
		return domain.Checkpoint{}, false, nil
	}
	checkpoint := *clone(value.current)
	if destination != nil && len(checkpoint.Payload) > 0 {
		if err := json.Unmarshal(checkpoint.Payload, destination); err != nil {
			return domain.Checkpoint{}, false, err
		}
	}
	return checkpoint, true, nil
}

// Save persists the next checkpoint before returning to the handler.
func Save(ctx context.Context, schema string, cursor map[string]string, payload any) error {
	value, ok := ctx.Value(contextKey{}).(*manager)
	if !ok || value.save == nil {
		return ErrUnavailable
	}
	if strings.TrimSpace(schema) == "" {
		return errors.New("checkpoint schema is required")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	value.mu.Lock()
	defer value.mu.Unlock()
	checkpoint := domain.Checkpoint{Schema: schema, Cursor: maps.Clone(cursor), Payload: encoded, ObservedAt: time.Now().UTC()}
	if value.current != nil {
		checkpoint.WorkloadID = value.current.WorkloadID
		checkpoint.AttemptID = value.current.AttemptID
		checkpoint.Kind = value.current.Kind
		checkpoint.Sequence = value.current.Sequence + 1
	} else {
		checkpoint.Sequence = 1
	}
	if err := value.save(checkpoint); err != nil {
		return err
	}
	value.current = clone(&checkpoint)
	return nil
}

// Apply imports a checkpoint emitted by an isolated subsystem process.
func Apply(ctx context.Context, incoming domain.Checkpoint) error {
	value, ok := ctx.Value(contextKey{}).(*manager)
	if !ok || value.save == nil {
		return ErrUnavailable
	}
	value.mu.Lock()
	defer value.mu.Unlock()
	if value.current != nil && incoming.Sequence <= value.current.Sequence {
		return errors.New("checkpoint sequence is not monotonic")
	}
	if incoming.Sequence == 0 || strings.TrimSpace(incoming.Schema) == "" || !json.Valid(incoming.Payload) {
		return errors.New("invalid checkpoint")
	}
	if err := value.save(incoming); err != nil {
		return err
	}
	value.current = clone(&incoming)
	return nil
}

// Snapshot returns a clone suitable for transfer to an isolated process.
func Snapshot(ctx context.Context) *domain.Checkpoint {
	value, ok := ctx.Value(contextKey{}).(*manager)
	if !ok {
		return nil
	}
	value.mu.Lock()
	defer value.mu.Unlock()
	return clone(value.current)
}

func clone(value *domain.Checkpoint) *domain.Checkpoint {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Cursor = maps.Clone(value.Cursor)
	copy.Payload = append(json.RawMessage(nil), value.Payload...)
	return &copy
}
