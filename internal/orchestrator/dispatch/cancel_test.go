package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

func storeServiceForTest(now time.Time, store Store) *Service {
	return &Service{config: Config{Now: func() time.Time { return now }}, store: store, sinks: make(map[Source]ResultSink)}
}

type cancelControl struct {
	Control
	cancelled []string
}

func (c *cancelControl) Cancel(_ context.Context, _ string, workloadID, _ string) error {
	c.cancelled = append(c.cancelled, workloadID)
	return nil
}

func TestBatchCancellationIsScopedAndRejectsLateDispatch(t *testing.T) {
	now := time.Now().UTC()
	store := NewMemoryStore()
	service := storeServiceForTest(now, store)
	control := &cancelControl{}
	service.control = control
	for _, id := range []string{"cancelled", "scheduled"} {
		spec := domain.Spec{WorkloadID: id, AttemptID: "attempt", Payload: []byte(`{"parts":[]}`)}
		if err := store.Put(Record{Source: SourceBeamCore, ExternalID: id, WorkloadKey: spec.Key(), BatchID: id + "-batch",
			WorkerID: "worker", State: StateRunning, Spec: spec}); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.CancelBatches(context.Background(), []string{"cancelled-batch", "pending-batch"}); err != nil {
		t.Fatal(err)
	}
	if len(control.cancelled) != 1 || control.cancelled[0] != "cancelled" {
		t.Fatalf("cancelled wrong workloads: %v", control.cancelled)
	}
	scheduled, _ := store.Get("scheduled/attempt")
	if scheduled.State != StateRunning {
		t.Fatal("unrelated scheduled batch was stopped")
	}
	for _, batchID := range []string{"cancelled-batch", "pending-batch"} {
		late := domain.Spec{WorkloadID: "late-" + batchID, AttemptID: "attempt", Payload: []byte(`{"parts":[]}`)}
		if _, err := service.Dispatch(context.Background(), DispatchRequest{Source: SourceBeamCore, ExternalID: "late-" + batchID,
			BatchID: batchID, Spec: late}); err == nil || err.Error() != "batch_cancelled" {
			t.Fatalf("late offer of %s was accepted: %v", batchID, err)
		}
	}
	if !service.BatchCancelled("pending-batch") || service.BatchCancelled("scheduled-batch") || service.BatchCancelled("") {
		t.Fatal("batch tombstones are not scoped to the cancelled batches")
	}
	// A retained cancelled record also fences replay after process restart.
	restarted := storeServiceForTest(now, store)
	cancelled, _ := store.Get("cancelled/attempt")
	if _, err := restarted.Dispatch(context.Background(), DispatchRequest{Source: SourceBeamCore, ExternalID: "cancelled", Spec: cancelled.Spec}); err == nil {
		t.Fatal("cancelled workload replayed after restart")
	}
}
