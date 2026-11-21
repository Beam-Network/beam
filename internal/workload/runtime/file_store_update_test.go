package runtime

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestFileStoreConcurrentUpdatesAreAtomicAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workloads.json")
	store, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"work-a", "work-b"}
	for _, id := range keys {
		if err := store.Create(Record{Spec: domain.Spec{WorkloadID: id, AttemptID: "attempt-1"}, State: domain.StateRunning}); err != nil {
			t.Fatal(err)
		}
	}
	var group sync.WaitGroup
	for writer := 0; writer < 16; writer++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for step := 0; step < 20; step++ {
				for _, id := range keys {
					if _, err := store.Update(id+"/attempt-1", func(record *Record) error {
						record.PlanVersion++
						return nil
					}); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}()
	}
	group.Wait()
	reopened, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range keys {
		for name, current := range map[string]Store{"open": store, "reopened": reopened} {
			record, err := current.Get(id + "/attempt-1")
			if err != nil || record.PlanVersion != 16*20 {
				t.Fatalf("%s %s plan version=%d err=%v, want %d", name, id, record.PlanVersion, err, 16*20)
			}
		}
	}
	if _, err := store.Update("missing/attempt-1", func(*Record) error { return nil }); err != ErrRecordNotFound {
		t.Fatalf("update of a missing record: %v", err)
	}
}
