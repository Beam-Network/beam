package roomtransfer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type leaseControl struct {
	testControl
	extensionsMu sync.Mutex
	extensions   []domain.Extension
}

func (c *leaseControl) Extend(_ context.Context, _ string, extension domain.Extension) error {
	c.extensionsMu.Lock()
	defer c.extensionsMu.Unlock()
	c.extensions = append(c.extensions, extension)
	return nil
}

func (c *leaseControl) extended() []domain.Extension {
	c.extensionsMu.Lock()
	defer c.extensionsMu.Unlock()
	return append([]domain.Extension(nil), c.extensions...)
}

func leaseDispatcher(t *testing.T, now time.Time, control dispatch.Control, capabilities ...string) *dispatch.Service {
	t.Helper()
	workerRegistry, err := registry.New(orchestratordomain.Orchestrator{OrchestratorID: "orchestrator-1", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := workerRegistry.Join(orchestratordomain.Membership{OrchestratorID: "orchestrator-1", WorkerID: "worker-1",
		NodeID: "node-1", Status: "active", JoinedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := workerRegistry.UpdateObservation(orchestratordomain.WorkerObservation{WorkerID: "worker-1", NodeID: "node-1",
		Status: "active", Capabilities: append([]string{contracts.RoomTransferCapability, contracts.RoomTransferDirectCapability,
			contracts.RoomTransferE2EECapability}, capabilities...), ObservedAt: now,
		Available: domain.Resources{CPUMillis: 4000, MemoryBytes: 4 << 30, BandwidthMbps: 1000, Connections: 1000, Streams: 100}}, now); err != nil {
		t.Fatal(err)
	}
	service, err := dispatch.NewService(dispatch.Config{OrchestratorID: "orchestrator-1", Now: func() time.Time { return now }},
		workerRegistry, control, dispatch.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func leaseUpdate(batch contracts.RoomTaskOfferBatch, lane LaneRecord, laneID string, expiresAt time.Time, leaseIDs ...string) contracts.RoomLaneLeaseUpdate {
	if len(leaseIDs) == 0 {
		leaseIDs = []string{lane.SourceLease.LeaseID}
		for _, lease := range lane.TargetLeases {
			leaseIDs = append(leaseIDs, lease.LeaseID)
		}
	}
	leases := make([]contracts.RoomLaneLeaseExpiry, 0, len(leaseIDs))
	for _, leaseID := range leaseIDs {
		leases = append(leases, contracts.RoomLaneLeaseExpiry{LeaseID: leaseID, ExpiresAt: expiresAt})
	}
	offer, _ := findOfferLane(batch, laneID)
	return contracts.RoomLaneLeaseUpdate{Type: "room_lane_lease_update", SchemaVersion: batch.SchemaVersion,
		TransferID: batch.TransferID, BatchID: batch.BatchID, LaneID: laneID, Attempt: offer.Attempt,
		ExpiresAt: expiresAt, Leases: leases, UpdatedAt: expiresAt}
}

func TestLeaseUpdateRaisesOnlyTheLanesOwnLeasesOnARenewingWorker(t *testing.T) {
	now := time.Now().UTC()
	control := &leaseControl{}
	store := NewMemoryStore()
	service, err := NewService(Config{Now: func() time.Time { return now }},
		leaseDispatcher(t, now, control, contracts.RoomTransferLeaseRenewalCapability), store)
	if err != nil {
		t.Fatal(err)
	}
	service.RegisterProvisioner(newTestProvisioner(t, now))
	service.RegisterSink(&testRoomSink{})
	batch := testBatch(t, now)
	if err := service.Submit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if !service.LeaseRenewalAvailable() {
		t.Fatal("a Worker advertising lease renewal must make the capability available")
	}
	record, _ := store.Get(batch.BatchID)
	laneA, laneB := record.Lanes["lane-a"], record.Lanes["lane-b"]
	later := laneA.SourceLease.ExpiresAt.Add(5 * time.Minute)
	if err := service.ExtendLease(context.Background(), leaseUpdate(batch, laneA, "lane-a", later, laneB.SourceLease.LeaseID)); err == nil {
		t.Fatal("an update naming another lane's lease must be refused")
	}
	if err := service.ExtendLease(context.Background(), leaseUpdate(batch, laneA, "lane-a", later)); err != nil {
		t.Fatal(err)
	}
	extensions := control.extended()
	record, _ = store.Get(batch.BatchID)
	if len(extensions) != 1 || !extensions[0].AssignmentExpiresAt.Equal(later) ||
		!record.Lanes["lane-a"].SourceLease.ExpiresAt.Equal(later) || !record.Lanes["lane-b"].SourceLease.ExpiresAt.Equal(laneB.SourceLease.ExpiresAt) {
		t.Fatalf("the update must reach the lane's Worker and raise only its leases: extensions=%+v", extensions)
	}
	for _, lease := range record.Lanes["lane-a"].TargetLeases {
		if !lease.ExpiresAt.Equal(later) {
			t.Fatalf("the lane's target lease must follow the update: %v", lease.ExpiresAt)
		}
	}
	if err := service.ExtendLease(context.Background(), leaseUpdate(batch, laneA, "lane-a", later.Add(-time.Minute))); err != nil ||
		len(control.extended()) != 1 {
		t.Fatalf("an earlier expiry must change nothing: %v", err)
	}
}

func TestLeaseUpdateIsRefusedForAWorkerWithoutLeaseRenewal(t *testing.T) {
	now := time.Now().UTC()
	control := &leaseControl{}
	store := NewMemoryStore()
	service, err := NewService(Config{Now: func() time.Time { return now }}, leaseDispatcher(t, now, control), store)
	if err != nil {
		t.Fatal(err)
	}
	service.RegisterProvisioner(newTestProvisioner(t, now))
	service.RegisterSink(&testRoomSink{})
	batch := testBatch(t, now)
	if err := service.Submit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if service.LeaseRenewalAvailable() {
		t.Fatal("lease renewal must not be advertised without a renewing Worker")
	}
	record, _ := store.Get(batch.BatchID)
	laneA := record.Lanes["lane-a"]
	if err := service.ExtendLease(context.Background(), leaseUpdate(batch, laneA, "lane-a", laneA.SourceLease.ExpiresAt.Add(time.Minute))); err == nil {
		t.Fatal("a Worker without lease renewal must not receive an extension")
	}
	record, _ = store.Get(batch.BatchID)
	if len(control.extended()) != 0 || !record.Lanes["lane-a"].SourceLease.ExpiresAt.Equal(laneA.SourceLease.ExpiresAt) {
		t.Fatal("a refused update must leave the lane's leases unchanged")
	}
}
