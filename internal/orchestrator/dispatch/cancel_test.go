package dispatch

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

type cancelControl struct {
	Control
	cancelled []string
}

func (c *cancelControl) Cancel(_ context.Context, _ string, workloadID, _ string) error {
	c.cancelled = append(c.cancelled, workloadID)
	return nil
}

func TestTransferCancellationIsScopedAndRejectsLateDispatch(t *testing.T) {
	now := time.Now().UTC()
	store := NewMemoryStore()
	service := checkpointServiceForTest(now, store)
	control := &cancelControl{}
	service.control = control
	for _, id := range []string{"cancelled", "scheduled"} {
		payload, _ := json.Marshal(map[string]string{"transfer_id": id})
		spec := domain.Spec{WorkloadID: id, AttemptID: "attempt", Payload: payload}
		if err := store.Put(Record{Source: SourceBeamCore, ExternalID: id, WorkloadKey: spec.Key(), WorkerID: "worker", State: StateRunning, Spec: spec}); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.CancelTransfer(context.Background(), "cancelled", "transfer_cancelled"); err != nil {
		t.Fatal(err)
	}
	if len(control.cancelled) != 1 || control.cancelled[0] != "cancelled" {
		t.Fatalf("cancelled wrong workloads: %v", control.cancelled)
	}
	scheduled, _ := store.Get("scheduled/attempt")
	if scheduled.State != StateRunning {
		t.Fatal("unrelated scheduled transfer was stopped")
	}
	late := domain.Spec{WorkloadID: "late", AttemptID: "attempt", Payload: json.RawMessage(`{"transfer_id":"cancelled"}`)}
	if _, err := service.Dispatch(context.Background(), DispatchRequest{Source: SourceBeamCore, ExternalID: "late", Spec: late}); err == nil || err.Error() != "transfer_cancelled" {
		t.Fatalf("late offer was accepted: %v", err)
	}
	// A retained cancelled record also fences replay after process restart.
	restarted := checkpointServiceForTest(now, store)
	cancelled, _ := store.Get("cancelled/attempt")
	if _, err := restarted.Dispatch(context.Background(), DispatchRequest{Source: SourceBeamCore, ExternalID: "cancelled", Spec: cancelled.Spec}); err == nil {
		t.Fatal("cancelled workload replayed after restart")
	}
}
