package connectors

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/orchestrator/roomworkloads"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestRoomControlPublishesStreamRuntimeOnRoomWorkloadRuntime(t *testing.T) {
	connection := &fakeRoomControlConnection{}
	control := newRoomControl(NATSConfig{Environment: "dev", Hotkey: "hotkey-1"}, nil, nil, nil, nil)
	control.conn = connection
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	identity := contracts.RoomWorkloadIdentity{WorkloadID: "workload-1", Kind: domain.KindRoomStream, UnitID: "unit-1",
		Epoch: 2, Attempt: 1, WorkerID: "worker-1", ExpiresAt: now.Add(time.Hour)}
	runtime := contracts.RoomStreamRuntime{SchemaVersion: contracts.RoomStreamRuntimeSchema, WorkloadID: "workload-1",
		UnitID: "unit-1", Epoch: 2, Attempt: 1, WorkerID: "worker-1", Capability: contracts.RoomStreamDirectCapability,
		Transport: "worker_https", BaseURL: "https://203.0.113.5:9480/v1/room-streams/workload-1-unit-1-1",
		TLSCertificateSHA256: strings.Repeat("ab", 32), ExpiresAt: now.Add(time.Hour),
		SourceAccessToken:  "source-token",
		TargetAccessTokens: []contracts.RoomStreamTargetToken{{TargetMemberID: "bob", AccessToken: "bob-token"}}}
	announcement, err := contracts.NewRoomWorkloadRuntimeWire(identity, runtime, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.SubmitRoomWorkloadRuntime(context.Background(), announcement); err != nil {
		t.Fatal(err)
	}
	request := connection.requests[0]
	if request.subject != "beam.orch.control.v2.dev.orch.hotkey-1.room_workload_runtime" {
		t.Fatalf("runtime subject %s", request.subject)
	}
	payload := request.payload
	for key, want := range map[string]any{"type": "room_workload_runtime", "schema_version": "room-workload/v1",
		"kind": "room.stream", "workload_id": "workload-1", "unit_id": "unit-1", "worker_id": "worker-1"} {
		if payload[key] != want {
			t.Fatalf("payload %s=%v want %v", key, payload[key], want)
		}
	}
	if len(payload) != 10 {
		t.Fatalf("runtime announcement keys %v", payload)
	}
	announced, _ := payload["runtime"].(map[string]any)
	tokens, _ := announced["target_access_tokens"].([]any)
	if announced["schema_version"] != contracts.RoomStreamRuntimeSchema || announced["source_access_token"] != "source-token" ||
		len(tokens) != 1 || tokens[0].(map[string]any)["target_member_id"] != "bob" {
		t.Fatalf("announced runtime %v", announced)
	}
}

func TestStreamDirectCapabilityRequiresACapableWorker(t *testing.T) {
	for _, test := range []struct {
		name         string
		capabilities []string
		want         bool
	}{
		{"direct stream worker", []string{"room.stream", contracts.RoomStreamDirectCapability}, true},
		{"relay stream worker", []string{"room.stream"}, false},
		{"message only worker", []string{"room.message", contracts.RoomMessageDirectCapability}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			workers, err := registry.New(orchestratordomain.Orchestrator{OrchestratorID: "orchestrator", CreatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			if err := workers.Join(orchestratordomain.Membership{OrchestratorID: "orchestrator", WorkerID: "worker", NodeID: "node",
				Status: "active", JoinedAt: now}, now); err != nil {
				t.Fatal(err)
			}
			capacity := domain.Resources{MemoryBytes: 1 << 30, BandwidthMbps: 100, Connections: 32, Streams: 32}
			if err := workers.UpdateObservation(orchestratordomain.WorkerObservation{WorkerID: "worker", NodeID: "node",
				Status: "active", Capabilities: test.capabilities, Total: capacity, Available: capacity, ObservedAt: now}, now); err != nil {
				t.Fatal(err)
			}
			tasks, err := dispatch.NewService(dispatch.Config{OrchestratorID: "orchestrator", Now: func() time.Time { return now }},
				workers, fakeControlWCP{}, dispatch.NewMemoryStore())
			if err != nil {
				t.Fatal(err)
			}
			manager, err := roomworkloads.OpenManager(t.TempDir(), tasks)
			if err != nil {
				t.Fatal(err)
			}
			manager.RegisterProvisioner(unusedRoomPathProvisioner{})
			manifest := newRoomControl(NATSConfig{Hotkey: "hotkey", SoftwareVersion: "test"}, nil, nil, manager, tasks).capabilityManifest(now)
			if got := contracts.SupportsCapability(manifest, contracts.RoomStreamDirectCapability); got != test.want {
				t.Fatalf("room.stream.direct.v1 advertised=%t, want %t (manifest %v)", got, test.want, manifest.Capabilities)
			}
		})
	}
}

type unusedRoomPathProvisioner struct{}

func (unusedRoomPathProvisioner) RedeemRoomPath(context.Context, contracts.RoomPathRedemptionRequest) (contracts.RoomPathLease, error) {
	panic("unexpected room path redemption")
}
