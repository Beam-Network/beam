package roomtransfer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

func TestServiceReusesWorkerAcrossCompactLanesAndRedeemsConcurrently(t *testing.T) {
	now := time.Now().UTC()
	control := &testControl{}
	dispatcher := testDispatcher(t, now, control)
	store := NewMemoryStore()
	service, err := NewService(Config{Now: func() time.Time { return now }}, dispatcher, store)
	if err != nil {
		t.Fatal(err)
	}
	provisioner := newTestProvisioner(t, now)
	service.RegisterProvisioner(provisioner)
	service.RegisterSink(&testRoomSink{})
	batch := testBatch(t, now)
	if err := service.Submit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	record, ok := store.Get(batch.BatchID)
	if !ok || len(record.Lanes) != 2 || control.count() != 2 || provisioner.count() != 4 {
		t.Fatalf("record=%+v offers=%d provisions=%d", record, control.count(), provisioner.count())
	}
	for _, lane := range record.Lanes {
		if lane.State != LaneDispatched || lane.WorkerID != "worker-1" || len(lane.TargetLeases) != 1 || lane.SourceLease == nil {
			t.Fatalf("unexpected lane: %+v", lane)
		}
	}
	if provisioner.maxConcurrent < 2 {
		t.Fatalf("lease redemption was sequential: max_concurrent=%d", provisioner.maxConcurrent)
	}
}

func TestReplayResumesAfterProvisionerReconnects(t *testing.T) {
	now := time.Now().UTC()
	control := &testControl{}
	dispatcher := testDispatcher(t, now, control)
	store := NewMemoryStore()
	service, err := NewService(Config{Now: func() time.Time { return now }}, dispatcher, store)
	if err != nil {
		t.Fatal(err)
	}
	batch := testBatch(t, now)
	batch.Lanes = batch.Lanes[:1]
	if err := service.Submit(context.Background(), batch); err == nil {
		t.Fatal("expected unavailable tunnel provisioner")
	}
	record, ok := store.Get(batch.BatchID)
	if !ok || record.Lanes[batch.Lanes[0].LaneID].WorkerID != "worker-1" {
		t.Fatalf("pending lane was not durably placed: %+v", record)
	}
	provisioner := newTestProvisioner(t, now)
	service.RegisterProvisioner(provisioner)
	service.Replay(context.Background())
	record, _ = store.Get(batch.BatchID)
	if record.Lanes[batch.Lanes[0].LaneID].State != LaneDispatched || control.count() != 1 || provisioner.count() != 2 {
		t.Fatalf("replay failed: record=%+v offers=%d provisions=%d", record, control.count(), provisioner.count())
	}
}

func TestServiceCancelsEveryActiveLaneForTransfer(t *testing.T) {
	now := time.Now().UTC()
	control := &testControl{}
	dispatcher := testDispatcher(t, now, control)
	store := NewMemoryStore()
	service, err := NewService(Config{Now: func() time.Time { return now }}, dispatcher, store)
	if err != nil {
		t.Fatal(err)
	}
	service.RegisterProvisioner(newTestProvisioner(t, now))
	service.RegisterSink(&testRoomSink{})
	batch := testBatch(t, now)
	if err := service.Submit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if err := service.Cancel(context.Background(), contracts.RoomTaskCancel{Type: "room_task_cancel",
		SchemaVersion: contracts.RoomTransferSchemaVersion, TransferID: batch.TransferID,
		Reason: "source_cancelled", CancelledAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	record, _ := store.Get(batch.BatchID)
	if control.cancelCount() != 2 {
		t.Fatalf("cancelled workloads = %d, want 2", control.cancelCount())
	}
	for _, lane := range record.Lanes {
		if lane.State != LaneCancelled || lane.Error != "source_cancelled" {
			t.Fatalf("cancelled lane = %+v", lane)
		}
	}
}

func TestScopedCancellationPreservesOtherAttempts(t *testing.T) {
	for _, schema := range []string{contracts.RoomTransferSchemaVersion, contracts.RoomStorageSchemaVersion} {
		t.Run(schema, func(t *testing.T) {
			now := time.Now().UTC()
			store := NewMemoryStore()
			service, err := NewService(Config{Now: func() time.Time { return now }}, testDispatcher(t, now, &testControl{}), store)
			if err != nil {
				t.Fatal(err)
			}
			for i, id := range []string{"old", "recovery"} {
				batch := testBatch(t, now)
				batch.BatchID, batch.SchemaVersion = id, schema
				batch.Lanes[0].Attempt = int64(i + 1)
				if err := store.Put(Record{Batch: batch, Lanes: map[string]LaneRecord{
					"lane-a": {LaneID: "lane-a", State: LanePending},
					"lane-b": {LaneID: "lane-b", State: LanePending},
				}}); err != nil {
					t.Fatal(err)
				}
			}
			request := contracts.RoomTaskCancel{Type: "room_task_cancel", SchemaVersion: schema,
				TransferID: "transfer-1", LaneID: "lane-a", Attempt: 1, Reason: "attempt_expired", CancelledAt: now}
			if err := service.Cancel(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			old, _ := store.Get("old")
			recovery, _ := store.Get("recovery")
			if old.Lanes["lane-a"].State != LaneCancelled || old.Lanes["lane-b"].State != LanePending ||
				recovery.Lanes["lane-a"].State != LanePending || recovery.Lanes["lane-b"].State != LanePending {
				t.Fatalf("scoped cancellation affected unrelated work: old=%+v recovery=%+v", old.Lanes, recovery.Lanes)
			}
			request.Attempt = 0
			if service.Cancel(context.Background(), request) == nil {
				t.Fatal("accepted incomplete attempt scope")
			}
			request.LaneID, request.Attempt = "", 1
			if service.Cancel(context.Background(), request) == nil {
				t.Fatal("accepted attempt without lane")
			}
			request.LaneID, request.SchemaVersion = "lane-a", "unsupported"
			if service.Cancel(context.Background(), request) == nil {
				t.Fatal("accepted unsupported cancellation schema")
			}
		})
	}
}

func TestServiceReportsWorkerRejectionAsTerminalRecovery(t *testing.T) {
	now := time.Now().UTC()
	control := &testControl{rejectOffers: true}
	dispatcher := testDispatcher(t, now, control)
	store := NewMemoryStore()
	service, err := NewService(Config{Now: func() time.Time { return now }}, dispatcher, store)
	if err != nil {
		t.Fatal(err)
	}
	service.RegisterProvisioner(newTestProvisioner(t, now))
	sink := &testRoomSink{}
	service.RegisterSink(sink)
	batch := testBatch(t, now)
	batch.Lanes = batch.Lanes[:1]
	if err := service.Submit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	record, _ := store.Get(batch.BatchID)
	lane := record.Lanes[batch.Lanes[0].LaneID]
	result := sink.only(t)
	if lane.State != LaneCompleted || result.ExecutionStage != "terminal" || result.WorkerAcknowledgedAt != nil ||
		result.ExecutableLeaseIssuedAt == nil || len(result.Missing) != 1 || len(result.Failures) != 1 ||
		result.Failures[0].Origin != "orchestrator" || result.Failures[0].Code != "worker_dispatch_failed" {
		t.Fatalf("lane=%+v result=%+v", lane, result)
	}
}

func TestServiceReportsFullyUnprovisionedLaneAsTerminalRecovery(t *testing.T) {
	now := time.Now().UTC()
	control := &testControl{}
	dispatcher := testDispatcher(t, now, control)
	store := NewMemoryStore()
	service, err := NewService(Config{Now: func() time.Time { return now }}, dispatcher, store)
	if err != nil {
		t.Fatal(err)
	}
	service.RegisterProvisioner(&targetRejectingProvisioner{testProvisioner: newTestProvisioner(t, now)})
	sink := &testRoomSink{}
	service.RegisterSink(sink)
	batch := testBatch(t, now)
	batch.Lanes = batch.Lanes[:1]
	if err := service.Submit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	record, _ := store.Get(batch.BatchID)
	lane := record.Lanes[batch.Lanes[0].LaneID]
	result := sink.only(t)
	if lane.State != LaneCompleted || control.count() != 0 || result.ExecutionStage != "terminal" ||
		result.ExecutableLeaseIssuedAt != nil || len(result.Missing) != 1 || len(result.Failures) != 1 ||
		result.Failures[0].Origin != "room_coordinator" || result.Failures[0].Code != "target_tunnel_unavailable" {
		t.Fatalf("lane=%+v result=%+v", lane, result)
	}
	if result.SourceReceipts == nil || result.TargetReceipts == nil || result.FinalTargetReceipts == nil {
		t.Fatalf("room result receipt arrays must not be nil: %+v", result)
	}
}

func testBatch(t *testing.T, now time.Time) contracts.RoomTaskOfferBatch {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	target := contracts.RoomTransferTarget{MemberID: "target-1", ReceiptPublicKey: base64.RawURLEncoding.EncodeToString(publicKey)}
	lanes := make([]contracts.RoomSourceLane, 0, 2)
	for chunk := int64(0); chunk < 2; chunk++ {
		laneID := "lane-a"
		if chunk == 1 {
			laneID = "lane-b"
		}
		base := contracts.TunnelLeaseIntent{TransferID: "transfer-1", LaneID: laneID, Attempt: 1,
			ChunkStart: chunk, ChunkEnd: chunk, OrchestratorID: "orchestrator-1",
			RequiredWorkerCapability: contracts.RoomTransferE2EECapability, ExpiresAt: now.Add(time.Hour), Signature: "signed"}
		source := base
		source.IntentID, source.Role = "source-"+laneID, contracts.TunnelLeaseRoleSourceRead
		destination := base
		destination.IntentID, destination.Role, destination.TargetMemberID = "target-"+laneID, contracts.TunnelLeaseRoleTargetWrite, target.MemberID
		lanes = append(lanes, contracts.RoomSourceLane{LaneID: laneID, Attempt: 1, ChunkStart: chunk, ChunkEnd: chunk,
			TargetMemberIDs: []string{target.MemberID}, SourceIntent: source, TargetIntents: []contracts.TunnelLeaseIntent{destination}})
	}
	return contracts.RoomTaskOfferBatch{Type: "room_task_offer_batch", SchemaVersion: contracts.RoomTransferSchemaVersion,
		BatchID: "batch-1", RoomID: "room-1", ChannelID: "channel-1", PublicationID: "publication-1",
		TransferID: "transfer-1", SnapshotVersion: 1,
		Protection:    contracts.RoomTransferProtection{Scheme: contracts.RoomTransferProtectionScheme, KeyEpoch: 1},
		FileSizeBytes: 16, ChunkSizeBytes: 8, ChunkCount: 2,
		Targets: []contracts.RoomTransferTarget{target}, Lanes: lanes, OfferExpiresAt: now.Add(time.Hour)}
}

func testDispatcher(t *testing.T, now time.Time, control *testControl) *dispatch.Service {
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
		Status: "active", Capabilities: []string{contracts.RoomTransferCapability, contracts.RoomTransferDirectCapability,
			contracts.RoomTransferE2EECapability}, ObservedAt: now,
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

type testControl struct {
	mu           sync.Mutex
	offers       []domain.Spec
	cancels      int
	rejectOffers bool
}

func (c *testControl) Connected(string) bool { return true }
func (c *testControl) Offer(_ context.Context, _ string, spec domain.Spec) (runtime.Decision, error) {
	c.mu.Lock()
	c.offers = append(c.offers, spec)
	reject := c.rejectOffers
	c.mu.Unlock()
	if reject {
		return runtime.Decision{WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID, Reason: "capacity changed"}, nil
	}
	return runtime.Decision{WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID, Accepted: true}, nil
}
func (c *testControl) count() int                                          { c.mu.Lock(); defer c.mu.Unlock(); return len(c.offers) }
func (c *testControl) cancelCount() int                                    { c.mu.Lock(); defer c.mu.Unlock(); return c.cancels }
func (c *testControl) Commit(context.Context, string, domain.Commit) error { return nil }
func (c *testControl) Cancel(context.Context, string, string, string) error {
	c.mu.Lock()
	c.cancels++
	c.mu.Unlock()
	return nil
}
func (c *testControl) UpsertCircuit(context.Context, circuit.Plan) error       { return nil }
func (c *testControl) RevokeCircuit(context.Context, circuit.Revocation) error { return nil }

type testProvisioner struct {
	mu            sync.Mutex
	calls         int
	active        int
	maxConcurrent int
	now           time.Time
	publicKey     string
}

func newTestProvisioner(t *testing.T, now time.Time) *testProvisioner {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testProvisioner{now: now, publicKey: base64.RawURLEncoding.EncodeToString(publicKey)}
}
func (p *testProvisioner) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }
func (p *testProvisioner) Redeem(_ context.Context, request RedeemRequest) (contracts.TunnelLease, error) {
	p.mu.Lock()
	p.calls++
	p.active++
	if p.active > p.maxConcurrent {
		p.maxConcurrent = p.active
	}
	p.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return contracts.TunnelLease{LeaseID: "lease-" + request.Intent.IntentID, IntentID: request.Intent.IntentID,
		Role: request.Intent.Role, TargetMemberID: request.Intent.TargetMemberID, Protocol: contracts.RoomTransferDirectCapability,
		Endpoints:      []contracts.TunnelLeaseEndpoint{{URL: "https://tunnel.invalid/" + request.Intent.IntentID}},
		AgentPublicKey: p.publicKey, ExpiresAt: p.now.Add(time.Hour)}, nil
}

type targetRejectingProvisioner struct{ *testProvisioner }

func (p *targetRejectingProvisioner) Redeem(ctx context.Context, request RedeemRequest) (contracts.TunnelLease, error) {
	if request.Intent.Role == contracts.TunnelLeaseRoleTargetWrite {
		return contracts.TunnelLease{}, errors.New("destination capacity changed")
	}
	return p.testProvisioner.Redeem(ctx, request)
}

type testRoomSink struct {
	mu      sync.Mutex
	results []contracts.RoomTaskResult
}

func (s *testRoomSink) SubmitRoomTaskResult(_ context.Context, result contracts.RoomTaskResult) error {
	s.mu.Lock()
	s.results = append(s.results, result)
	s.mu.Unlock()
	return nil
}

func (s *testRoomSink) only(t *testing.T) contracts.RoomTaskResult {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.results) != 1 {
		t.Fatalf("room results = %d, want 1", len(s.results))
	}
	return s.results[0]
}
