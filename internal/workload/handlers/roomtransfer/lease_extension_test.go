package roomtransfer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestExtendLeaseRaisesTheLanesLeasesAndTheLiveSession(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	transfer := contracts.RoomTransfer{TransferID: "transfer-1", LaneID: "lane-1", Attempt: 2, ChunkStart: 0, ChunkEnd: 3,
		SourceLease: contracts.TunnelLease{LeaseID: "source-lease", ExpiresAt: now.Add(time.Minute)},
		Targets: []contracts.RoomTransferDestination{{MemberID: "target-1",
			Lease: contracts.TunnelLease{LeaseID: "target-lease", ExpiresAt: now.Add(time.Minute)}}}}
	payload, _ := json.Marshal(transfer)
	spec := domain.Spec{WorkloadID: "lane", AttemptID: "attempt", Payload: payload,
		Lease: domain.Lease{AssignmentExpiresAt: now.Add(time.Minute)}}
	handler := NewHandler(Config{})
	shared := &sharedServer{sessions: make(map[string]*session)}
	handler.server = shared
	live := &session{transfer: transfer, now: time.Now}
	live.trackLeases()
	var reported []time.Time
	live.reportExpiry = func(expiresAt time.Time) { reported = append(reported, expiresAt) }
	shared.sessions["transfer-1-lane-1-2"] = live
	later := now.Add(5 * time.Minute)

	if _, err := handler.ExtendLease(spec, domain.Extension{WorkloadID: "lane", AttemptID: "attempt", AssignmentExpiresAt: later,
		LeaseExpiries: map[string]time.Time{"another-lease": later}}); err == nil {
		t.Fatal("an extension naming no lease of the lane must be refused")
	}
	extended, err := handler.ExtendLease(spec, domain.Extension{WorkloadID: "lane", AttemptID: "attempt", AssignmentExpiresAt: later,
		LeaseExpiries: map[string]time.Time{"source-lease": later, "target-lease": later}})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := decodeTransfer(extended)
	if err != nil || !extended.Lease.AssignmentExpiresAt.Equal(later) || !persisted.SourceLease.ExpiresAt.Equal(later) ||
		!persisted.Targets[0].Lease.ExpiresAt.Equal(later) {
		t.Fatalf("the persisted spec must carry the extended leases: %+v %v", extended.Lease, err)
	}
	current := live.current()
	if !current.SourceLease.ExpiresAt.Equal(later) || !current.Targets[0].Lease.ExpiresAt.Equal(later) ||
		!live.sourceExpiry().Equal(later) || !live.targetExpiry("target-1").Equal(later) {
		t.Fatal("the live session's path checks must follow the extended leases")
	}
	if len(reported) != 1 || !reported[0].Equal(later) {
		t.Fatalf("the worker must report its runtime at the extended expiry: %v", reported)
	}
}
