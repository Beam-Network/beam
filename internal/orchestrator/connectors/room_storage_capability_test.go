package connectors

import (
	"context"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/orchestrator/roomtransfer"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadruntime "github.com/Beam-Network/beam/internal/workload/runtime"
)

func TestRoomStorageManifestRequiresLiveCapableWorker(t *testing.T) {
	for _, test := range []struct {
		name         string
		capabilities []string
		capacity     bool
		want         bool
	}{
		{"hybrid", []string{contracts.RoomTransferCapability, contracts.RoomStorageCapability}, true, true},
		{"missing base", []string{contracts.RoomStorageCapability}, true, false},
		{"MLS only", []string{contracts.RoomTransferCapability, contracts.RoomTransferDirectCapability, contracts.RoomTransferE2EECapability}, true, false},
		{"no capacity", []string{contracts.RoomTransferCapability, contracts.RoomStorageCapability}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			workers, err := registry.New(orchestratordomain.Orchestrator{OrchestratorID: "orchestrator", CreatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			if err := workers.Join(orchestratordomain.Membership{OrchestratorID: "orchestrator", WorkerID: "worker", NodeID: "node", Status: "active", JoinedAt: now}, now); err != nil {
				t.Fatal(err)
			}
			capacity := domain.Resources{MemoryBytes: 1 << 30, BandwidthMbps: 100, Connections: 32, Streams: 32}
			available := capacity
			if !test.capacity {
				available = domain.Resources{}
			}
			if err := workers.UpdateObservation(orchestratordomain.WorkerObservation{WorkerID: "worker", NodeID: "node", Status: "active", Capabilities: test.capabilities, Total: capacity, Available: available, ObservedAt: now}, now); err != nil {
				t.Fatal(err)
			}
			transport := hybridCapabilityTransport{}
			tasks, err := dispatch.NewService(dispatch.Config{OrchestratorID: "orchestrator", Now: func() time.Time { return now }}, workers, transport, dispatch.NewMemoryStore())
			if err != nil {
				t.Fatal(err)
			}
			rooms, err := roomtransfer.NewService(roomtransfer.Config{Now: func() time.Time { return now }}, tasks, roomtransfer.NewMemoryStore())
			if err != nil {
				t.Fatal(err)
			}
			rooms.RegisterProvisioner(transport)
			rooms.RegisterSink(transport)
			control := newRoomControl(NATSConfig{Hotkey: "hotkey", SoftwareVersion: "test"}, nil, rooms, nil, tasks)
			manifest := control.capabilityManifest(now)
			if got := contracts.SupportsCapability(manifest, contracts.RoomStorageCapability); got != test.want {
				t.Fatalf("hybrid capability=%t, want=%t", got, test.want)
			}
			if test.want && !contracts.SupportsCapability(manifest, contracts.RoomTransferCapability) {
				t.Fatal("hybrid manifest omitted base room protocol")
			}
		})
	}
}

type hybridCapabilityTransport struct{}

func (hybridCapabilityTransport) Offer(context.Context, string, domain.Spec) (workloadruntime.Decision, error) {
	panic("unexpected offer")
}
func (hybridCapabilityTransport) Commit(context.Context, string, domain.Commit) error {
	panic("unexpected commit")
}
func (hybridCapabilityTransport) Cancel(context.Context, string, string, string) error {
	panic("unexpected cancel")
}
func (hybridCapabilityTransport) Connected(string) bool { return true }
func (hybridCapabilityTransport) UpsertCircuit(context.Context, circuit.Plan) error {
	panic("unexpected circuit")
}
func (hybridCapabilityTransport) RevokeCircuit(context.Context, circuit.Revocation) error {
	panic("unexpected revocation")
}
func (hybridCapabilityTransport) Redeem(context.Context, roomtransfer.RedeemRequest) (contracts.TunnelLease, error) {
	panic("unexpected lease")
}
func (hybridCapabilityTransport) SubmitRoomTaskResult(context.Context, contracts.RoomTaskResult) error {
	panic("unexpected result")
}
