package subsystem

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

const maxIPCBytes = 16 << 20

type Request struct {
	Spec       domain.Spec        `json:"spec"`
	Checkpoint *domain.Checkpoint `json:"checkpoint,omitempty"`
}

type Event struct {
	Type       string             `json:"type"`
	Progress   map[string]string  `json:"progress,omitempty"`
	Checkpoint *domain.Checkpoint `json:"checkpoint,omitempty"`
	Result     *domain.Result     `json:"result,omitempty"`
	Error      string             `json:"error,omitempty"`
}

type emitter struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

func (e *emitter) send(event Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.encoder.Encode(event)
}

// ServeOne executes exactly one request. Keeping the subprocess single-use
// prevents state and credentials from leaking between unrelated workloads.
func ServeOne(ctx context.Context, handler runtime.Handler, input io.Reader, output io.Writer) error {
	if handler == nil {
		return errors.New("subsystem handler is required")
	}
	decoder := json.NewDecoder(io.LimitReader(input, maxIPCBytes))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode subsystem request: %w", err)
	}
	stream := &emitter{encoder: json.NewEncoder(output)}
	if request.Spec.Kind != handler.Kind() {
		err := fmt.Errorf("subsystem %s cannot execute %s", handler.Kind(), request.Spec.Kind)
		_ = stream.send(Event{Type: "error", Error: err.Error()})
		return err
	}
	if err := handler.Validate(request.Spec); err != nil {
		_ = stream.send(Event{Type: "error", Error: err.Error()})
		return err
	}
	initial := request.Checkpoint
	if initial == nil {
		initial = &domain.Checkpoint{WorkloadID: request.Spec.WorkloadID, AttemptID: request.Spec.AttemptID, Kind: request.Spec.Kind}
	}
	executionContext := workloadprogress.WithReporter(ctx, func(progress map[string]string) {
		_ = stream.send(Event{Type: "progress", Progress: progress})
	})
	executionContext = workloadcheckpoint.WithManager(executionContext, initial, func(value domain.Checkpoint) error {
		return stream.send(Event{Type: "checkpoint", Checkpoint: &value})
	})
	result, err := handler.Execute(executionContext, request.Spec)
	if err != nil {
		if sendErr := stream.send(Event{Type: "result", Result: &result, Error: err.Error()}); sendErr != nil {
			return sendErr
		}
		return nil
	}
	return stream.send(Event{Type: "result", Result: &result})
}

func decodeEvents(input io.Reader, consume func(Event) error) error {
	scanner := bufio.NewScanner(io.LimitReader(input, maxIPCBytes))
	scanner.Buffer(make([]byte, 64<<10), maxIPCBytes)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return fmt.Errorf("decode subsystem event: %w", err)
		}
		if err := consume(event); err != nil {
			return err
		}
	}
	return scanner.Err()
}
