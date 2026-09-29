package contracts

import (
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestRoomWorkloadZeroTargetsRequireLateJoin(t *testing.T) {
	now := time.Now()
	offer := RoomWorkloadOfferWire[MediaUnitDetails]{Type: RoomWorkloadOffer, SchemaVersion: RoomWorkloadSchema,
		Kind: domain.KindRoomMedia, WorkloadID: "work-1", RoomID: "room-1", ChannelID: "channel-1", SourceMemberID: "alice",
		AuthorizationEpoch: 3, PlanEpoch: 4, UnitID: "unit-1", Epoch: 5, Attempt: 1,
		DestinationSnapshot: RoomDestinationSnapshot{SnapshotVersion: 4, Targets: []RoomWireTarget{}},
		RequiredCapacity:    RoomCapacityRequirement{Capability: RoomMediaWebRTCCapability, Units: 1},
		PathAuthorizations: RoomPathAuthorizations{Targets: []RoomPathAuthorization{},
			Source: RoomPathAuthorization{PathID: "unit-1/epoch-5/attempt-1/source", Role: "source", Protocol: "webrtc",
				ExpiresAt: now.Add(time.Hour).UTC(), CoordinatorSignature: "source-signature"}},
		OfferExpiresAt: now.Add(time.Hour).UTC(), Details: MediaUnitDetails{SessionID: "session-1", Service: "publish",
			Profile: RoomMediaWebRTCWorkerProfile, Tracks: []MediaTrack{{TrackID: "video-main", Kind: "video"}}, Layers: []MediaLayer{}}}
	if err := offer.Validate(now, false); err == nil {
		t.Fatal("zero-target offer was accepted without late join")
	}
	if err := offer.Validate(now, true); err != nil {
		t.Fatalf("late-join offer was rejected: %v", err)
	}
	submit := RoomWorkloadSubmitWire[MediaDefinitionDetails]{Type: RoomWorkloadSubmit, SchemaVersion: RoomWorkloadSchema,
		Kind: domain.KindRoomMedia, WorkloadID: "work-1", IdempotencyKey: "key-1", RoomID: "room-1", ChannelID: "channel-1",
		SourceMemberID: "alice", Targets: []RoomWireTarget{}, AuthorizationEpoch: 3, PlanEpoch: 4,
		RequiredCapacity: RoomCapacityRequirement{Capability: RoomMediaWebRTCCapability, Units: 1}, ExpiresAt: now.Add(time.Hour).UTC()}
	if err := submit.Validate(now, false); err == nil {
		t.Fatal("zero-target submission was accepted without late join")
	}
	if err := submit.Validate(now, true); err != nil {
		t.Fatalf("late-join submission was rejected: %v", err)
	}
}
