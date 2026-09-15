package dispatch

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

type callbackSink struct {
	call func(context.Context, Record) error
}

func callbackService(t *testing.T) (*Service, Record) {
	t.Helper()
	store := NewMemoryStore()
	service := checkpointServiceForTest(time.Now().UTC(), store)
	service.control = &cancelControl{}
	spec := domain.Spec{WorkloadID: "workload", AttemptID: "attempt", Kind: domain.KindActionExecute}
	record := Record{Source: SourceStudio, ExternalID: "event", WorkloadKey: spec.Key(),
		WorkerID: "worker", State: StateCommitted, Spec: spec}
	if err := store.Put(record); err != nil {
		t.Fatal(err)
	}
	return service, record
}

func (s callbackSink) DeliverResult(ctx context.Context, record Record, _ domain.Result) error {
	return s.call(ctx, record)
}
func (s callbackSink) DeliverProgress(ctx context.Context, record Record, _ domain.Progress) error {
	return s.call(ctx, record)
}
func (s callbackSink) DeliverCheckpoint(ctx context.Context, record Record, _ domain.Checkpoint) error {
	return s.call(ctx, record)
}

func TestCallbacksPermitDispatchReplayAndPreserveCancellation(t *testing.T) {
	for _, event := range []string{"progress", "checkpoint", "result"} {
		t.Run(event, func(t *testing.T) {
			service, record := callbackService(t)
			request := DispatchRequest{Source: SourceStudio, ExternalID: record.ExternalID, Spec: record.Spec}
			service.RegisterSink(SourceStudio, callbackSink{call: func(ctx context.Context, current Record) error {
				// Room replay holds the room lock and enters Dispatch while a
				// callback waits for that same room lock. This reentry exercises
				// the dispatch side of the verified lock-order cycle.
				if _, err := service.Dispatch(ctx, request); err != nil {
					return err
				}
				return service.CancelWorkload(ctx, current.WorkloadKey, "cancel during callback")
			}})
			done := make(chan error, 1)
			go func() {
				switch event {
				case "progress":
					done <- service.HandleProgress(domain.Progress{WorkloadID: record.Spec.WorkloadID, AttemptID: record.Spec.AttemptID})
				case "checkpoint":
					done <- service.HandleCheckpoint(context.Background(), record.WorkerID, domain.Checkpoint{
						WorkloadID: record.Spec.WorkloadID, AttemptID: record.Spec.AttemptID,
						Kind: record.Spec.Kind, Sequence: 1, Schema: "coverage", Payload: []byte(`{}`)})
				case "result":
					done <- service.HandleResult(context.Background(), domain.Result{
						WorkloadID: record.Spec.WorkloadID, AttemptID: record.Spec.AttemptID, State: domain.StateCompleted})
				}
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("upstream callback deadlocked dispatch replay")
			}
			current, _ := service.Record(record.WorkloadKey)
			if event == "result" {
				if current.State != StateCompleted || !current.UpstreamDelivered {
					t.Fatal("terminal acknowledgement was lost")
				}
			} else if current.State != StateCancelled {
				t.Fatal("callback acknowledgement overwrote concurrent cancellation")
			}
			if event == "checkpoint" && current.UpstreamCheckpointSequence != 1 {
				t.Fatal("checkpoint acknowledgement was lost")
			}
		})
	}
}

func TestConcurrentResultReplayDeliversOnce(t *testing.T) {
	service, record := callbackService(t)
	var calls atomic.Int32
	service.RegisterSink(SourceStudio, callbackSink{call: func(context.Context, Record) error { calls.Add(1); return nil }})
	result := domain.Result{WorkloadID: record.Spec.WorkloadID, AttemptID: record.Spec.AttemptID, State: domain.StateCompleted}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := service.HandleResult(context.Background(), result); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("delivered %d duplicate callbacks", calls.Load())
	}
}
