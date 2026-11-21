package dispatch

import (
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

type countingStore struct {
	Store
	mu     sync.Mutex
	puts   int
	stages int
}

func (s *countingStore) Put(record Record) error {
	s.mu.Lock()
	s.puts++
	s.mu.Unlock()
	return s.Store.Put(record)
}

func (s *countingStore) Stage(record Record) error {
	s.mu.Lock()
	s.stages++
	s.mu.Unlock()
	return s.Store.Stage(record)
}

func TestWorkerProgressStaysInMemory(t *testing.T) {
	now := time.Now().UTC()
	store := &countingStore{Store: NewMemoryStore()}
	spec := domain.Spec{WorkloadID: "workload", AttemptID: "attempt", Kind: domain.KindActionExecute}
	if err := store.Store.Put(Record{Source: SourceStudio, ExternalID: "event", WorkloadKey: spec.Key(),
		WorkerID: "worker", State: StateCommitted, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	service := storeServiceForTest(now, store)
	if err := service.HandleProgress(domain.Progress{WorkloadID: "workload", AttemptID: "attempt", State: domain.StateRunning,
		ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	record, ok := store.Get(spec.Key())
	if store.puts != 0 || store.stages != 1 || !ok || record.State != StateRunning || record.Progress == nil {
		t.Fatalf("progress puts=%d stages=%d record=%+v", store.puts, store.stages, record)
	}
}
