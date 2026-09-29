package roomworkloads

import (
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestMediaRuntimeRequiresCleanHTTPSAndCertificatePin(t *testing.T) {
	runtime := func() contracts.MediaRuntime {
		return contracts.MediaRuntime{Capability: contracts.RoomMediaWebRTCCapability, Transport: "worker_sfu",
			BaseURL: "https://worker.example.test:9460/v1/rooms/session", AccessToken: "token",
			TLSCertificateSHA256: strings.Repeat("ab", 32), ExpiresAt: time.Now().Add(time.Hour).UTC()}
	}
	valid := runtime()
	if err := validateMediaRuntime(&valid); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*contracts.MediaRuntime){
		"plain http": func(value *contracts.MediaRuntime) {
			value.BaseURL = "http://worker.example.test:9460/v1/rooms/session"
		},
		"user info": func(value *contracts.MediaRuntime) {
			value.BaseURL = "https://user@worker.example.test/v1/rooms/session"
		},
		"query":         func(value *contracts.MediaRuntime) { value.BaseURL += "?token=value" },
		"fragment":      func(value *contracts.MediaRuntime) { value.BaseURL += "#fragment" },
		"missing pin":   func(value *contracts.MediaRuntime) { value.TLSCertificateSHA256 = "" },
		"short pin":     func(value *contracts.MediaRuntime) { value.TLSCertificateSHA256 = strings.Repeat("ab", 31) },
		"uppercase pin": func(value *contracts.MediaRuntime) { value.TLSCertificateSHA256 = strings.Repeat("AB", 32) },
		"non-hex pin":   func(value *contracts.MediaRuntime) { value.TLSCertificateSHA256 = strings.Repeat("zz", 32) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			value := runtime()
			mutate(&value)
			if err := validateMediaRuntime(&value); err == nil {
				t.Fatal("Orchestrator accepted a media runtime BeamCore rejects")
			}
		})
	}
}

func TestMediaStrategyMatchesWorkerAdmission(t *testing.T) {
	now := time.Now().UTC()
	definition := func() genericDefinition[contracts.MediaUnitDetails] {
		return genericDefinition[contracts.MediaUnitDetails]{Workload: contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails]{
			Schema: contracts.RoomWorkloadSchema,
			Identity: contracts.RoomWorkloadIdentity{WorkloadID: "workload-1", Kind: domain.KindRoomMedia, RoomID: "room-1",
				ChannelID: "channel-1", SourceMemberID: "alice", TargetSnapshot: 1, AuthorizationEpoch: 1, PlanEpoch: 1,
				UnitID: "unit-1", Epoch: 1, Attempt: 1, ExpiresAt: now.Add(time.Hour)},
			Source: contracts.RoomPathIntent{PathID: "unit-1/source", Role: "source"},
			Targets: []contracts.RoomInternalTarget{{MemberID: "bob",
				Path: contracts.RoomPathIntent{PathID: "unit-1/target/bob", Role: "target", TargetMemberID: "bob"}}},
			RequiredCapacity: contracts.RoomCapacityRequirement{Capability: contracts.RoomMediaWebRTCCapability, Units: 1},
			Details: contracts.MediaUnitDetails{SessionID: "session-1", Service: "publish", Profile: contracts.RoomMediaWebRTCWorkerProfile,
				Tracks: []contracts.MediaTrack{{TrackID: "video-main", Kind: "video"}}, Layers: []contracts.MediaLayer{},
				Protection: contracts.MediaProtection{Scheme: contracts.RoomMediaProtectionSchemeV1,
					KeyScope: contracts.RoomMediaProtectionChannel, Required: true}},
		}}
	}
	strategy := newMediaStrategy()
	if err := strategy.ValidateDefinition(definition(), now); err != nil {
		t.Fatal(err)
	}
	type workload = contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails]
	cases := map[string]func(*workload){
		"two capacity units":     func(value *workload) { value.RequiredCapacity.Units = 2 },
		"legacy capacity":        func(value *workload) { value.RequiredCapacity.Capability = "room.media" },
		"lifetime over 23 hours": func(value *workload) { value.Identity.ExpiresAt = now.Add(23*time.Hour + time.Minute) },
		"data track": func(value *workload) {
			value.Details.Tracks = append(value.Details.Tracks, contracts.MediaTrack{TrackID: "data-main", Kind: "data"})
		},
		"duplicate track id": func(value *workload) {
			value.Details.Tracks = append(value.Details.Tracks, contracts.MediaTrack{TrackID: "video-main", Kind: "audio"})
		},
		"source as target": func(value *workload) {
			value.Targets = append(value.Targets, contracts.RoomInternalTarget{MemberID: "alice",
				Path: contracts.RoomPathIntent{PathID: "unit-1/target/alice", Role: "target", TargetMemberID: "alice"}})
		},
		"replay": func(value *workload) { value.Details.Replay = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			value := definition()
			mutate(&value.Workload)
			if err := strategy.ValidateDefinition(value, now); err == nil {
				t.Fatal("Orchestrator admitted WebRTC media outside the worker contract")
			}
		})
	}
	legacy := definition()
	legacy.Workload.RequiredCapacity = contracts.RoomCapacityRequirement{Capability: "room.media", Units: 2}
	legacy.Workload.Details.Profile = contracts.RoomMediaLegacyProfile
	legacy.Workload.Details.Protection = contracts.MediaProtection{}
	legacy.Workload.Details.Tracks = append(legacy.Workload.Details.Tracks, contracts.MediaTrack{TrackID: "data-main", Kind: "data"})
	if err := strategy.ValidateDefinition(legacy, now); err != nil {
		t.Fatalf("legacy media admission changed: %v", err)
	}
}
