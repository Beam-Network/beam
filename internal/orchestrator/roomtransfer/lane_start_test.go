package roomtransfer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

type blockingProvisioner struct {
	*testProvisioner
	lane    string
	entered chan struct{}
	release chan struct{}
}

func (p *blockingProvisioner) Redeem(ctx context.Context, request RedeemRequest) (contracts.TunnelLease, error) {
	if request.LaneID == p.lane {
		select {
		case p.entered <- struct{}{}:
		default:
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return contracts.TunnelLease{}, ctx.Err()
		}
	}
	return p.testProvisioner.Redeem(ctx, request)
}

func TestBatchLanesStartWithoutWaitingForEachOther(t *testing.T) {
	now := time.Now().UTC()
	control := &testControl{}
	store := NewMemoryStore()
	service, err := NewService(Config{Now: func() time.Time { return now }}, testDispatcher(t, now, control), store)
	if err != nil {
		t.Fatal(err)
	}
	provisioner := &blockingProvisioner{testProvisioner: newTestProvisioner(t, now), lane: "lane-b",
		entered: make(chan struct{}, 1), release: make(chan struct{})}
	service.RegisterProvisioner(provisioner)
	service.RegisterSink(&testRoomSink{})
	batch := testBatch(t, now)
	done := make(chan error, 1)
	go func() { done <- service.Submit(context.Background(), batch) }()
	<-provisioner.entered
	deadline := time.Now().Add(5 * time.Second)
	for {
		record, _ := store.Get(batch.BatchID)
		if record.Lanes["lane-a"].State == LaneDispatched {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lane-a waited for lane-b's provisioning")
		}
		time.Sleep(5 * time.Millisecond)
	}
	record, _ := store.Get(batch.BatchID)
	if record.Lanes["lane-b"].State == LaneDispatched {
		t.Fatal("lane-b was dispatched before its redemption")
	}
	close(provisioner.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	record, _ = store.Get(batch.BatchID)
	if record.Lanes["lane-a"].State != LaneDispatched || record.Lanes["lane-b"].State != LaneDispatched || control.count() != 2 {
		t.Fatalf("lanes=%+v offers=%d", record.Lanes, control.count())
	}
}

func TestCancellationDuringLaneStartStopsTheLaneBeforeItsWorkerRuns(t *testing.T) {
	now := time.Now().UTC()
	control := &testControl{}
	store := NewMemoryStore()
	service, err := NewService(Config{Now: func() time.Time { return now }}, testDispatcher(t, now, control), store)
	if err != nil {
		t.Fatal(err)
	}
	provisioner := &blockingProvisioner{testProvisioner: newTestProvisioner(t, now), lane: "lane-a",
		entered: make(chan struct{}, 1), release: make(chan struct{})}
	service.RegisterProvisioner(provisioner)
	sink := &testRoomSink{}
	service.RegisterSink(sink)
	batch := testBatch(t, now)
	batch.Lanes = batch.Lanes[:1]
	done := make(chan error, 1)
	go func() { done <- service.Submit(context.Background(), batch) }()
	<-provisioner.entered
	if err := service.Cancel(context.Background(), contracts.RoomTaskCancel{Type: "room_task_cancel", SchemaVersion: batch.SchemaVersion,
		TransferID: batch.TransferID, Reason: "transfer_cancelled", CancelledAt: now}); err != nil {
		t.Fatal(err)
	}
	close(provisioner.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	record, _ := store.Get(batch.BatchID)
	if lane := record.Lanes["lane-a"]; lane.State != LaneCancelled {
		t.Fatalf("cancelled lane ended %s", lane.State)
	}
	if control.count() != 0 || control.cancelCount() != 0 {
		t.Fatalf("a lane cancelled during its start reached its Worker: offers=%d cancels=%d", control.count(), control.cancelCount())
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.results) != 0 {
		t.Fatalf("a cancelled lane reported %d results", len(sink.results))
	}
}

func TestRoomFileStorePrunesFinishedBatchesOnly(t *testing.T) {
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "rooms.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expired := now.Add(-time.Hour)
	lease := contracts.TunnelLease{ExpiresAt: expired}
	put := func(id string, offerExpires time.Time, lanes map[string]LaneRecord) {
		t.Helper()
		if err := store.Put(Record{Batch: contracts.RoomTaskOfferBatch{BatchID: id, OfferExpiresAt: offerExpires}, Lanes: lanes,
			CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	put("finished", expired, map[string]LaneRecord{"lane": {LaneID: "lane", State: LaneCompleted, SourceLease: &lease}})
	put("running", expired, map[string]LaneRecord{"lane": {LaneID: "lane", State: LaneDispatched, SourceLease: &lease}})
	put("live-offer", now.Add(time.Hour), map[string]LaneRecord{"lane": {LaneID: "lane", State: LaneCompleted}})
	live := contracts.TunnelLease{ExpiresAt: now}
	put("live-lease", expired, map[string]LaneRecord{"lane": {LaneID: "lane", State: LaneCancelled, SourceLease: &live}})
	for id, want := range map[string]bool{"finished": false, "running": true, "live-offer": true, "live-lease": true} {
		if _, ok := store.Get(id); ok != want {
			t.Fatalf("batch %s retained=%v, want %v", id, ok, want)
		}
	}
	reopened, err := OpenFileStore(filepath.Join(filepath.Dir(store.path), "rooms.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get("finished"); ok || len(reopened.List()) != 3 {
		t.Fatalf("persisted batches=%d", len(reopened.List()))
	}
}
