package roomworkload

import (
	"testing"
	"time"
)

func TestKeyedLocksKeepIndependentWorkloadsMoving(t *testing.T) {
	var locks KeyedLocks
	unlockA := locks.Lock("a")
	defer unlockA()
	done := make(chan struct{})
	go func() {
		unlockB := locks.Lock("b")
		unlockB()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unrelated workload waited for another key")
	}
	locks.mu.Lock()
	remaining := len(locks.entries)
	locks.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("completed workload lock was retained: %d entries", remaining)
	}
}

func TestKeyedLocksSerializeSameWorkloadAndReleaseEntry(t *testing.T) {
	var locks KeyedLocks
	unlock := locks.Lock("same")
	acquired := make(chan struct{})
	completed := make(chan struct{})
	go func() {
		unlockSecond := locks.Lock("same")
		close(acquired)
		unlockSecond()
		close(completed)
	}()
	select {
	case <-acquired:
		t.Fatal("same workload acquired the lock twice")
	case <-time.After(20 * time.Millisecond):
	}
	unlock()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("waiting workload did not acquire the released lock")
	}
	locks.mu.Lock()
	remaining := len(locks.entries)
	locks.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("completed workload lock was retained: %d entries", remaining)
	}
}
