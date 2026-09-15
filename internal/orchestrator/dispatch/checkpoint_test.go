package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

type checkpointSink struct {
	checkpointCalls int
	checkpointError error
}

func (s *checkpointSink) DeliverResult(context.Context, Record, domain.Result) error { return nil }

func checkpointServiceForTest(now time.Time, store Store) *Service {
	return &Service{config: Config{Now: func() time.Time { return now }}, store: store, sinks: make(map[Source]ResultSink)}
}

func (s *checkpointSink) DeliverCheckpoint(context.Context, Record, domain.Checkpoint) error {
	s.checkpointCalls++
	return s.checkpointError
}

func TestCheckpointRetainsCoverageAcrossUpstreamFailureAndRestart(t *testing.T) {
	now := time.Now().UTC()
	store := NewMemoryStore()
	service := checkpointServiceForTest(now, store)
	sink := &checkpointSink{checkpointError: errors.New("temporarily disconnected")}
	service.RegisterSink(SourceStudio, sink)
	spec := domain.Spec{WorkloadID: "workload", AttemptID: "attempt", Kind: domain.KindActionExecute}
	record := Record{Source: SourceStudio, ExternalID: "event", WorkloadKey: spec.Key(), WorkerID: "worker", State: StateRunning, Spec: spec}
	if err := store.Put(record); err != nil {
		t.Fatal(err)
	}
	checkpoint := domain.Checkpoint{WorkloadID: record.Spec.WorkloadID, AttemptID: record.Spec.AttemptID, Kind: record.Spec.Kind, Schema: "source-group", Sequence: 2, Payload: []byte(`{"completed":1}`)}
	if err := service.HandleCheckpoint(context.Background(), "wrong-worker", checkpoint); err == nil {
		t.Fatal("unassigned worker checkpoint accepted")
	}
	if err := service.HandleCheckpoint(context.Background(), record.WorkerID, checkpoint); err == nil {
		t.Fatal("upstream failure lost")
	}
	retained, _ := store.Get(record.WorkloadKey)
	if retained.Checkpoint == nil || retained.Checkpoint.Sequence != 2 || retained.UpstreamCheckpointSequence != 0 {
		t.Fatalf("checkpoint not retained: %+v", retained)
	}
	service = checkpointServiceForTest(now, store)
	sink.checkpointError = nil
	service.RegisterSink(SourceStudio, sink)
	service.ReplayResults(context.Background())
	retained, _ = store.Get(record.WorkloadKey)
	if retained.UpstreamCheckpointSequence != 2 || sink.checkpointCalls != 2 {
		t.Fatalf("checkpoint not replayed: %+v", retained)
	}
	checkpoint.Sequence = 1
	if err := service.HandleCheckpoint(context.Background(), record.WorkerID, checkpoint); err != nil {
		t.Fatal(err)
	}
	if sink.checkpointCalls != 2 {
		t.Fatal("stale checkpoint was replayed")
	}
}
