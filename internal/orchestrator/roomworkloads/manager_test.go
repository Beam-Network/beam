package roomworkloads

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestZeroTargetsAdmitOnlyLateJoinWebRTCMedia(t *testing.T) {
	now := time.Now().UTC()
	identity := contracts.RoomWorkloadIdentity{WorkloadID: "workload-1", Kind: domain.KindRoomMedia, RoomID: "room-1",
		ChannelID: "channel-1", SourceMemberID: "alice", TargetSnapshot: 1, AuthorizationEpoch: 1, PlanEpoch: 1,
		UnitID: "unit-1", Epoch: 1, Attempt: 1, ExpiresAt: now.Add(time.Hour)}
	source := contracts.RoomPathIntent{PathID: "unit-1/source", Role: "source"}
	capacity := contracts.RoomCapacityRequirement{Capability: contracts.RoomMediaWebRTCCapability, Units: 1}
	media := func(profile string) genericDefinition[contracts.MediaUnitDetails] {
		return genericDefinition[contracts.MediaUnitDetails]{Workload: contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails]{
			Schema: contracts.RoomWorkloadSchema, Identity: identity, Source: source, RequiredCapacity: capacity,
			Details: contracts.MediaUnitDetails{SessionID: "session-1", Service: "publish", Profile: profile,
				Tracks: []contracts.MediaTrack{{TrackID: "video-main", Kind: "video"}}, Layers: []contracts.MediaLayer{},
				Protection: contracts.MediaProtection{Scheme: contracts.RoomMediaProtectionSchemeV1,
					KeyScope: contracts.RoomMediaProtectionChannel, Required: true}}}}
	}
	webRTC := media(contracts.RoomMediaWebRTCWorkerProfile)
	if err := newMediaStrategy().ValidateDefinition(webRTC, now); err != nil {
		t.Fatalf("late-join WebRTC media was rejected: %v", err)
	}
	payload, err := mediaPayload(webRTC.Workload, genericAttempt[contracts.MediaUnitDetails, contracts.MediaResultDetails]{
		WorkerID: "worker-1", Source: genericPath{Credential: &contracts.RoomPathLease{PathID: source.PathID, Role: "source"}}})
	if err != nil {
		t.Fatal(err)
	}
	var worker contracts.RoomWorkerSpec[contracts.MediaUnitDetails]
	if err := json.Unmarshal(payload, &worker); err != nil || worker.Targets == nil || len(worker.Targets) != 0 {
		t.Fatalf("late-join worker payload targets = %v err=%v", worker.Targets, err)
	}
	if err := newMediaStrategy().ValidateDefinition(media(contracts.RoomMediaLegacyProfile), now); err == nil {
		t.Fatal("zero-target legacy media was accepted")
	}
	messageIdentity := identity
	messageIdentity.Kind = domain.KindRoomMessage
	message := genericDefinition[contracts.MessageUnitDetails]{Workload: contracts.RoomWorkloadDefinition[contracts.MessageUnitDetails]{
		Schema: contracts.RoomWorkloadSchema, Identity: messageIdentity, Source: source,
		RequiredCapacity: contracts.RoomCapacityRequirement{Capability: "room.message", Units: 1},
		Details:          contracts.MessageUnitDetails{MessageID: "message-1", ContentType: "application/json", SizeBytes: 1}}}
	if err := newMessageStrategy().ValidateDefinition(message, now); err == nil {
		t.Fatal("zero-target room.message was accepted")
	}

	manager := &Manager{now: time.Now}
	offer := func(kind domain.Kind, details any) []byte {
		encoded, _ := json.Marshal(contracts.RoomWorkloadOfferWire[any]{Type: contracts.RoomWorkloadOffer,
			SchemaVersion: contracts.RoomWorkloadSchema, Kind: kind, WorkloadID: "workload-1", RoomID: "room-1",
			ChannelID: "channel-1", SourceMemberID: "alice", AuthorizationEpoch: 1, PlanEpoch: 1,
			UnitID: "unit-1", Epoch: 1, Attempt: 1, RequiredCapacity: capacity,
			DestinationSnapshot: contracts.RoomDestinationSnapshot{SnapshotVersion: 1, Targets: []contracts.RoomWireTarget{}},
			PathAuthorizations: contracts.RoomPathAuthorizations{Targets: []contracts.RoomPathAuthorization{},
				Source: contracts.RoomPathAuthorization{PathID: "unit-1/epoch-1/attempt-1/source", Role: "source",
					Protocol: contracts.RoomPathProtocol(kind), ExpiresAt: now.Add(time.Hour), CoordinatorSignature: "signature"}},
			OfferExpiresAt: now.Add(time.Hour), Details: details})
		return encoded
	}
	for name, test := range map[string]struct {
		kind    domain.Kind
		details any
	}{
		"legacy media": {domain.KindRoomMedia, media(contracts.RoomMediaLegacyProfile).Workload.Details},
		"room.message": {domain.KindRoomMessage, message.Workload.Details},
	} {
		if err := manager.submitOffer(context.Background(), test.kind, offer(test.kind, test.details)); err == nil {
			t.Fatalf("zero-target %s offer was accepted", name)
		}
	}
}
