package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/resources"
	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type checkpointingExtender struct {
	executing chan context.Context
	release   chan struct{}
	during    func()
}

func (h *checkpointingExtender) Kind() domain.Kind          { return domain.KindRoomTransfer }
func (h *checkpointingExtender) Validate(domain.Spec) error { return nil }
func (h *checkpointingExtender) Execute(ctx context.Context, _ domain.Spec) (domain.Result, error) {
	h.executing <- ctx
	<-h.release
	return domain.Result{}, workloadcheckpoint.Save(ctx, "beam.test/1", nil, map[string]int{"chunk": 2})
}
func (h *checkpointingExtender) ExtendLease(spec domain.Spec, extension domain.Extension) (domain.Spec, error) {
	h.during()
	spec.Lease.AssignmentExpiresAt = extension.AssignmentExpiresAt
	return spec, nil
}

func TestEngineExtendKeepsCheckpointSavedDuringTheExtension(t *testing.T) {
	handler := &checkpointingExtender{executing: make(chan context.Context, 1), release: make(chan struct{})}
	registry := NewRegistry()
	if err := registry.Register(handler); err != nil {
		t.Fatal(err)
	}
	governor, _ := resources.NewGovernor(domain.Resources{MemoryBytes: 1024})
	store := NewMemoryStore()
	engine, err := NewEngine("worker-1", []string{"room.transfer"}, registry, governor, store)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	spec := domain.Spec{WorkloadID: "lane", AttemptID: "attempt-1", Identity: domain.Identity{WorkerID: "worker-1"},
		Kind: domain.KindRoomTransfer, Class: domain.ClassJob, RequiredCapabilities: []string{"room.transfer"},
		Resources: domain.Resources{MemoryBytes: 128}, Lease: domain.Lease{OfferExpiresAt: now.Add(time.Minute)},
		Payload: json.RawMessage(`{}`)}
	if decision, err := engine.Offer(context.Background(), spec); err != nil || !decision.Accepted {
		t.Fatalf("offer decision=%+v err=%v", decision, err)
	}
	if err := engine.Commit(context.Background(), domain.Commit{WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID,
		PlanVersion: 1, AssignmentToken: "token", AssignmentExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	ctx := <-handler.executing
	handler.during = func() {
		if err := workloadcheckpoint.Save(ctx, "beam.test/1", nil, map[string]int{"chunk": 1}); err != nil {
			t.Errorf("checkpoint during extension: %v", err)
		}
	}
	extended := now.Add(2 * time.Minute)
	if err := engine.Extend(domain.Extension{WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID, AssignmentExpiresAt: extended}); err != nil {
		t.Fatal(err)
	}
	close(handler.release)
	select {
	case result := <-engine.Results():
		if result.State != domain.StateCompleted {
			t.Fatalf("checkpoint after the extension failed the workload: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the workload result")
	}
	record, err := store.Get(spec.Key())
	if err != nil || record.Checkpoint == nil || record.Checkpoint.Sequence != 2 || !record.Spec.Lease.AssignmentExpiresAt.Equal(extended) {
		t.Fatalf("record lost the checkpoint or the extension: %+v %v", record, err)
	}
}
