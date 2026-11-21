package roomtransfer

import (
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestStreamRelayIsAvailableOnlyWithARelayingWorker(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name         string
		capabilities []string
		want         bool
	}{
		{"relaying worker", []string{contracts.RoomTransferStreamRelayCapability}, true},
		{"worker without the relay", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, err := NewService(Config{Now: func() time.Time { return now }},
				leaseDispatcher(t, now, &leaseControl{}, test.capabilities...), NewMemoryStore())
			if err != nil {
				t.Fatal(err)
			}
			service.RegisterProvisioner(newTestProvisioner(t, now))
			service.RegisterSink(&testRoomSink{})
			if service.StreamRelayAvailable() != test.want {
				t.Fatalf("stream relay available=%v, want %v", !test.want, test.want)
			}
		})
	}
}

func TestStorageSourceLanesReserveTheNextChunkReadAhead(t *testing.T) {
	service := &Service{config: Config{Resources: domain.Resources{MemoryBytes: 96 << 20}}}
	chunk := int64(41 << 20)
	batch := contracts.RoomTaskOfferBatch{SchemaVersion: contracts.RoomStorageSchemaVersion, FileSizeBytes: 1 << 30, ChunkSizeBytes: chunk,
		Targets: []contracts.RoomTransferTarget{{MemberID: "agent", Kind: "agent"}, {MemberID: "bucket", Kind: contracts.RoomObjectStorageTargetKind}}}
	if memory := service.resourcesFor(batch, contracts.RoomSourceLane{TargetMemberIDs: []string{"agent"}}).MemoryBytes; memory != 2*chunk+(32<<20) {
		t.Fatalf("storage-source lane reserved %d bytes", memory)
	}
	if memory := service.resourcesFor(batch, contracts.RoomSourceLane{TargetMemberIDs: []string{"bucket"}}).MemoryBytes; memory != 96<<20 {
		t.Fatalf("storage-target lane reserved %d bytes", memory)
	}
	batch.SchemaVersion = contracts.RoomTransferSchemaVersion
	if memory := service.resourcesFor(batch, contracts.RoomSourceLane{TargetMemberIDs: []string{"agent"}}).MemoryBytes; memory != 96<<20 {
		t.Fatalf("agent lane reserved %d bytes", memory)
	}
}
