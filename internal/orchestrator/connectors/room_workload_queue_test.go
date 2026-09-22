package connectors

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/roomworkloads"
	"github.com/vmihailenco/msgpack/v5"
)

func TestRoomWorkloadQueueKeepsOfferAndCancelOrdered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	queue := newRoomWorkloadQueue(func(_ context.Context, job roomWorkloadJob) error {
		if !job.cancel {
			close(firstStarted)
			<-releaseFirst
		} else {
			close(secondStarted)
		}
		return nil
	})
	stopped := make(chan struct{})
	go func() { queue.run(ctx); close(stopped) }()
	defer func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
		cancel()
		<-stopped
	}()
	control := &roomControl{config: NATSConfig{Environment: "dev", Hotkey: "participant"}, workloads: &roomworkloads.Manager{}}
	for _, isCancel := range []bool{false, true} {
		messageType := "room_workload_offer"
		if isCancel {
			messageType = "room_workload_cancel"
		}
		encoded := roomWorkloadTestEnvelope(t, messageType, "workload-1")
		if err := control.queueRoomWorkload(ctx, queue, encoded, isCancel); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("offer did not start")
	}
	select {
	case <-secondStarted:
		t.Fatal("same-workload cancellation overtook offer")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("same-workload cancellation did not run after offer")
	}
}

func TestRoomWorkloadQueueRunsIndependentKeysTogether(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondCompleted := make(chan struct{})
	queue := newRoomWorkloadQueue(func(_ context.Context, job roomWorkloadJob) error {
		if job.workloadID == "a" {
			close(firstStarted)
			<-releaseFirst
		} else {
			close(secondCompleted)
		}
		return nil
	})
	stopped := make(chan struct{})
	go func() { queue.run(ctx); close(stopped) }()
	defer func() { close(releaseFirst); cancel(); <-stopped }()
	if err := queue.enqueue(ctx, roomWorkloadJob{workloadID: "a"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first workload did not start")
	}
	if err := queue.enqueue(ctx, roomWorkloadJob{workloadID: "b"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondCompleted:
	case <-time.After(time.Second):
		t.Fatal("independent workload waited for a slow peer")
	}
}

func TestRoomWorkloadQueueHasBoundedBackpressure(t *testing.T) {
	queue := newRoomWorkloadQueue(nil)
	queue.slots = make(chan struct{}, 1)
	queue.input = make(chan roomWorkloadJob, 1)
	if err := queue.enqueue(context.Background(), roomWorkloadJob{workloadID: "a"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := queue.enqueue(ctx, roomWorkloadJob{workloadID: "b"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("full queue did not apply bounded backpressure: %v", err)
	}
}

func TestRoomWorkloadQueuePreservesPerKeyBurstOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	expected := make(map[string]byte)
	var active atomic.Int32
	var peak atomic.Int32
	done := make(chan error, 200)
	queue := newRoomWorkloadQueue(func(_ context.Context, job roomWorkloadJob) error {
		count := active.Add(1)
		for {
			current := peak.Load()
			if count <= current || peak.CompareAndSwap(current, count) {
				break
			}
		}
		mu.Lock()
		want := expected[job.workloadID]
		got := job.payload[0]
		if got == want {
			expected[job.workloadID]++
		}
		mu.Unlock()
		time.Sleep(time.Millisecond)
		active.Add(-1)
		if got != want {
			done <- fmt.Errorf("%s processed sequence %d before %d", job.workloadID, got, want)
		} else {
			done <- nil
		}
		return nil
	})
	stopped := make(chan struct{})
	go func() { queue.run(ctx); close(stopped) }()
	defer func() { cancel(); <-stopped }()
	for sequence := 0; sequence < 20; sequence++ {
		for key := 0; key < 10; key++ {
			if err := queue.enqueue(ctx, roomWorkloadJob{workloadID: fmt.Sprintf("workload-%d", key), payload: []byte{byte(sequence)}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for range 200 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("bounded burst did not drain")
		}
	}
	if got := peak.Load(); got < 2 || got > roomWorkloadParallelism {
		t.Fatalf("unexpected worker concurrency: %d", got)
	}
}

func roomWorkloadTestEnvelope(t *testing.T, messageType, workloadID string) []byte {
	t.Helper()
	encoded, err := msgpack.Marshal(orchestratorControlEnvelope{SchemaVersion: orchestratorControlSchema,
		Environment: "dev", Hotkey: "participant", MessageType: messageType, Producer: "transfer-runtime",
		Payload: map[string]any{"workload_id": workloadID}})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
