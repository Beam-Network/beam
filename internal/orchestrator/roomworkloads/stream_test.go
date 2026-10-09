package roomworkloads

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadruntime "github.com/Beam-Network/beam/internal/workload/runtime"
)

type idleControl struct{}

func (idleControl) Offer(context.Context, string, domain.Spec) (workloadruntime.Decision, error) {
	return workloadruntime.Decision{}, nil
}
func (idleControl) Commit(context.Context, string, domain.Commit) error     { return nil }
func (idleControl) Cancel(context.Context, string, string, string) error    { return nil }
func (idleControl) Connected(string) bool                                   { return true }
func (idleControl) UpsertCircuit(context.Context, circuit.Plan) error       { return nil }
func (idleControl) RevokeCircuit(context.Context, circuit.Revocation) error { return nil }

func streamDefinition(now time.Time, targets ...string) genericDefinition[contracts.StreamUnitDetails] {
	identity := contracts.RoomWorkloadIdentity{WorkloadID: "workload-1", Kind: domain.KindRoomStream, RoomID: "room-1",
		ChannelID: "channel-1", SourceMemberID: "alice", TargetSnapshot: 1, AuthorizationEpoch: 1, PlanEpoch: 1,
		UnitID: "unit-1", Epoch: 2, Attempt: 1, ExpiresAt: now.Add(time.Hour)}
	definition := contracts.RoomWorkloadDefinition[contracts.StreamUnitDetails]{Schema: contracts.RoomWorkloadSchema,
		Identity: identity, Source: contracts.RoomPathIntent{PathID: "unit-1/source", Role: "source"},
		RequiredCapacity: contracts.RoomCapacityRequirement{Capability: contracts.RoomStreamDirectCapability, Units: 1},
		Resources:        domain.Resources{Connections: 1, Streams: 1},
		Details: contracts.StreamUnitDetails{SessionID: "workload-1", Protocol: contracts.RoomStreamProtectionProtocol,
			BackpressurePolicy: "block", MaxBufferBytes: 1 << 20, HeartbeatTimeoutMS: 30_000}}
	for _, member := range targets {
		definition.Targets = append(definition.Targets, contracts.RoomInternalTarget{MemberID: member,
			Path: contracts.RoomPathIntent{PathID: "unit-1/target/" + member, Role: "target", TargetMemberID: member}})
	}
	return genericDefinition[contracts.StreamUnitDetails]{Workload: definition}
}

func streamRuntime(identity contracts.RoomWorkloadIdentity, targets ...string) contracts.RoomStreamRuntime {
	runtime := contracts.RoomStreamRuntime{SchemaVersion: contracts.RoomStreamRuntimeSchema, WorkloadID: identity.WorkloadID,
		UnitID: identity.UnitID, Epoch: identity.Epoch, Attempt: identity.Attempt, WorkerID: identity.WorkerID,
		Capability: contracts.RoomStreamDirectCapability, Transport: "worker_https",
		BaseURL:              "https://203.0.113.5:9480/v1/room-streams/workload-1-unit-1-1",
		TLSCertificateSHA256: strings.Repeat("ab", 32), ExpiresAt: identity.ExpiresAt,
		SourceAccessToken: testToken(0)}
	for index, member := range targets {
		runtime.TargetAccessTokens = append(runtime.TargetAccessTokens,
			contracts.RoomStreamTargetToken{TargetMemberID: member, AccessToken: testToken(byte(index + 1))})
	}
	return runtime
}

func testToken(seed byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32))
}

func TestStreamStrategyAdmitsOnlyDirectProtectedStreams(t *testing.T) {
	now := time.Now().UTC()
	strategy := newStreamStrategy()
	valid := streamDefinition(now, "bob", "carol")
	if err := strategy.ValidateDefinition(valid, now); err != nil {
		t.Fatal(err)
	}
	type workload = contracts.RoomWorkloadDefinition[contracts.StreamUnitDetails]
	cases := map[string]func(*workload){
		"relay capacity":     func(value *workload) { value.RequiredCapacity.Capability = "room.stream" },
		"two capacity units": func(value *workload) { value.RequiredCapacity.Units = 2 },
		"drop_newest":        func(value *workload) { value.Details.BackpressurePolicy = "drop_newest" },
		"plaintext protocol": func(value *workload) { value.Details.Protocol = "raw" },
		"buffer below range": func(value *workload) { value.Details.MaxBufferBytes = 65_536 },
		"buffer above range": func(value *workload) { value.Details.MaxBufferBytes = contracts.RoomStreamMaxBufferBytes + 1 },
		"replay":             func(value *workload) { value.Details.Replay = true },
		"no heartbeat":       func(value *workload) { value.Details.HeartbeatTimeoutMS = 0 },
		"heartbeat too long": func(value *workload) { value.Details.HeartbeatTimeoutMS = 300_001 },
		"missing session":    func(value *workload) { value.Details.SessionID = "" },
		"65 targets": func(value *workload) {
			value.Targets = nil
			for index := range 65 {
				member := fmt.Sprintf("member-%d", index)
				value.Targets = append(value.Targets, contracts.RoomInternalTarget{MemberID: member,
					Path: contracts.RoomPathIntent{PathID: "unit-1/target/" + member, Role: "target", TargetMemberID: member}})
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			value := streamDefinition(now, "bob", "carol")
			mutate(&value.Workload)
			if err := strategy.ValidateDefinition(value, now); err == nil {
				t.Fatal("Orchestrator admitted a stream outside room.stream.direct.v1")
			}
		})
	}
	unit := genericUnit[contracts.StreamUnitDetails]{ID: "unit-1", Unit: valid.Workload}
	if got := strategy.RequiredCapabilities(valid, unit); !slices.Equal(got, []string{"room.stream", contracts.RoomStreamDirectCapability}) {
		t.Fatalf("required capabilities %v", got)
	}
	resources := strategy.Resources(valid, unit)
	if resources.MemoryBytes != 1<<20+8*65_536+1<<20 || resources.Connections != 11 || resources.Streams != 3 {
		t.Fatalf("stream resources %+v", resources)
	}
}

func TestStreamDetailsAreRuntimeOnlyOrCounters(t *testing.T) {
	identity := streamDefinition(time.Now()).Workload.Identity
	runtime := streamRuntime(identity, "bob")
	reason := "no_live_subscriber"
	long := strings.Repeat("x", 513)
	targets := []contracts.RoomStreamTargetState{{TargetMemberID: "bob", State: "active", DeliveredThrough: 7},
		{TargetMemberID: "carol", State: "dropped", Reason: &reason}}
	valid := []any{
		contracts.StreamProgressDetails{Runtime: &runtime},
		contracts.StreamProgressDetails{Sequence: 7, Bytes: 70, Dropped: 1, Targets: targets},
		contracts.StreamResultDetails{Sequence: 7, Bytes: 70, Dropped: 1, Targets: targets, TerminalReason: "ended"},
		contracts.StreamResultDetails{Targets: targets[:1], TerminalReason: "source_lost"},
	}
	for _, value := range valid {
		if err := validateDetails(value); err != nil {
			t.Fatalf("%+v rejected: %v", value, err)
		}
	}
	invalid := map[string]any{
		"runtime with counters":    contracts.StreamProgressDetails{Sequence: 1, Runtime: &runtime},
		"runtime with targets":     contracts.StreamProgressDetails{Targets: targets, Runtime: &runtime},
		"counters without targets": contracts.StreamProgressDetails{Sequence: 1},
		"unknown target state": contracts.StreamProgressDetails{Targets: []contracts.RoomStreamTargetState{
			{TargetMemberID: "bob", State: "ready"}}},
		"oversized reason": contracts.StreamProgressDetails{Targets: []contracts.RoomStreamTargetState{
			{TargetMemberID: "bob", State: "failed", Reason: &long}}},
		"duplicate target": contracts.StreamProgressDetails{Targets: []contracts.RoomStreamTargetState{
			{TargetMemberID: "bob", State: "active"}, {TargetMemberID: "bob", State: "active"}}},
		"dropped beyond targets": contracts.StreamProgressDetails{Dropped: 3, Targets: targets},
		"relay terminal reason":  contracts.StreamResultDetails{Targets: targets, TerminalReason: "source_closed"},
		"result without targets": contracts.StreamResultDetails{TerminalReason: "ended"},
	}
	for name, value := range invalid {
		if err := validateDetails(value); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	encoded, _ := json.Marshal(contracts.StreamProgressDetails{Runtime: &runtime})
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil || len(keys) != 1 || keys["runtime"] == nil {
		t.Fatalf("runtime progress must encode as runtime only: %s", encoded)
	}
	failed, err := failedResultDetails[contracts.StreamResultDetails](domain.KindRoomStream,
		streamDefinition(time.Now(), "bob").Workload, "worker exited", "worker_failed")
	if err != nil || !strings.Contains(string(failed), `"targets":[]`) || !strings.Contains(string(failed), `"terminal_reason":"worker_failed"`) {
		t.Fatalf("synthesized failure %s err=%v", failed, err)
	}
}

type recordingSink struct {
	runtimes []contracts.RoomWorkloadRuntimeWire
	progress []contracts.RoomGenericProgress
}

func (s *recordingSink) SubmitRoomWorkloadProgress(_ context.Context, value contracts.RoomGenericProgress) error {
	s.progress = append(s.progress, value)
	return nil
}
func (s *recordingSink) SubmitRoomWorkloadRuntime(_ context.Context, value contracts.RoomWorkloadRuntimeWire) error {
	s.runtimes = append(s.runtimes, value)
	return nil
}
func (s *recordingSink) SubmitRoomWorkloadResult(context.Context, contracts.RoomGenericResult) error {
	return nil
}
func (s *recordingSink) SubmitRoomWorkloadStatus(context.Context, json.RawMessage) error { return nil }
func (s *recordingSink) SubmitRoomWorkloadProvisioningResult(context.Context, contracts.RoomWorkloadProvisioningResultWire) error {
	return nil
}

func TestStreamRuntimeProgressBecomesAnAnnouncement(t *testing.T) {
	now := time.Now().UTC()
	definition := streamDefinition(now, "bob", "carol")
	identity := definition.Workload.Identity
	identity.WorkerID = "worker-1"
	attempt := genericAttempt[contracts.StreamUnitDetails, contracts.StreamResultDetails]{WorkerID: "worker-1",
		WorkloadKey: "room-workload-1/epoch-2-attempt-1"}
	progressFor := func(details any) domain.Progress {
		encoded, _ := json.Marshal(details)
		return domain.Progress{WorkloadID: "room-workload-1", AttemptID: "epoch-2-attempt-1", ObservedAt: now,
			Outputs: map[string]string{"room_progress_details": string(encoded)}}
	}
	strategy := newStreamStrategy()
	runtime := streamRuntime(identity, "bob", "carol")
	validated, err := strategy.ValidateProgress(definition, attempt, progressFor(contracts.StreamProgressDetails{Runtime: &runtime}))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*contracts.RoomStreamRuntime){
		"missing target": func(value *contracts.RoomStreamRuntime) { value.TargetAccessTokens = value.TargetAccessTokens[:1] },
		"foreign target": func(value *contracts.RoomStreamRuntime) { value.TargetAccessTokens[1].TargetMemberID = "mallory" },
		"shared token": func(value *contracts.RoomStreamRuntime) {
			value.TargetAccessTokens[1].AccessToken = value.SourceAccessToken
		},
		"other attempt":      func(value *contracts.RoomStreamRuntime) { value.Attempt = 2 },
		"beyond the lease":   func(value *contracts.RoomStreamRuntime) { value.ExpiresAt = identity.ExpiresAt.Add(time.Second) },
		"uppercase pin":      func(value *contracts.RoomStreamRuntime) { value.TLSCertificateSHA256 = strings.Repeat("AB", 32) },
		"query in base URL":  func(value *contracts.RoomStreamRuntime) { value.BaseURL += "?token=1" },
		"short source token": func(value *contracts.RoomStreamRuntime) { value.SourceAccessToken = "short" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := streamRuntime(identity, "bob", "carol")
			mutate(&candidate)
			if _, err := strategy.ValidateProgress(definition, attempt, progressFor(contracts.StreamProgressDetails{Runtime: &candidate})); err == nil {
				t.Fatal("invalid stream runtime was accepted")
			}
		})
	}
	sink := &recordingSink{}
	manager := &Manager{now: time.Now, sink: sink}
	deliver := progressSink[contracts.StreamUnitDetails, contracts.StreamResultDetails]{manager: manager}
	if err := deliver.DeliverRoomWorkloadProgress(context.Background(), validated); err != nil {
		t.Fatal(err)
	}
	if len(sink.progress) != 0 || len(sink.runtimes) != 1 {
		t.Fatalf("runtime progress forwarded=%d announced=%d", len(sink.progress), len(sink.runtimes))
	}
	announcement := sink.runtimes[0]
	var announced contracts.RoomStreamRuntime
	if err := json.Unmarshal(announcement.Runtime, &announced); err != nil {
		t.Fatal(err)
	}
	if announcement.Type != contracts.RoomWorkloadRuntimeType || announcement.Kind != domain.KindRoomStream ||
		announcement.Epoch != 2 || announcement.WorkerID != "worker-1" || announced.SourceAccessToken != runtime.SourceAccessToken ||
		len(announced.TargetAccessTokens) != 2 {
		t.Fatalf("announcement %+v runtime %+v", announcement, announced)
	}
	counters, err := strategy.ValidateProgress(definition, attempt, progressFor(contracts.StreamProgressDetails{Sequence: 3, Bytes: 30,
		Targets: []contracts.RoomStreamTargetState{{TargetMemberID: "bob", State: "active", DeliveredThrough: 3},
			{TargetMemberID: "carol", State: "joining"}}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := deliver.DeliverRoomWorkloadProgress(context.Background(), counters); err != nil {
		t.Fatal(err)
	}
	if len(sink.progress) != 1 || len(sink.runtimes) != 1 || strings.Contains(string(sink.progress[0].Details), "runtime") {
		t.Fatalf("counters progress forwarded=%d announced=%d", len(sink.progress), len(sink.runtimes))
	}
}

func TestStreamResultStatusMapsTargetsToDestinations(t *testing.T) {
	now := time.Now().UTC()
	workers, err := registry.New(orchestratordomain.Orchestrator{OrchestratorID: "orchestrator-1", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := dispatch.NewService(dispatch.Config{OrchestratorID: "orchestrator-1", Now: time.Now}, workers,
		idleControl{}, dispatch.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := OpenManager(t.TempDir(), dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	reason := "no_live_subscriber"
	status := func(terminal string, targets ...contracts.RoomStreamTargetState) resultStatus {
		dropped := int64(0)
		for _, target := range targets {
			if target.State == "dropped" || target.State == "failed" {
				dropped++
			}
		}
		encoded, _ := json.Marshal(contracts.StreamResultDetails{Sequence: 9, Bytes: 90, Dropped: dropped, Targets: targets,
			TerminalReason: terminal})
		return manager.resultStatus(contracts.RoomGenericResult{Identity: contracts.RoomWorkloadIdentity{Kind: domain.KindRoomStream},
			State: contracts.RoomCompleted, Details: encoded})
	}
	completed := contracts.RoomStreamTargetState{TargetMemberID: "bob", State: "completed", DeliveredThrough: 9}
	dropped := contracts.RoomStreamTargetState{TargetMemberID: "carol", State: "dropped", Reason: &reason}
	partial := status("ended", completed, dropped)
	if partial.state != contracts.RoomPartial || len(partial.destinations) != 2 ||
		partial.destinations[0].Status != contracts.DestinationCompleted || partial.destinations[1].Status != contracts.DestinationDropped ||
		*partial.destinations[1].Reason != reason {
		t.Fatalf("partial status %+v", partial)
	}
	if details := partial.details.(contracts.StreamStatusDetails); details.Sequence != 9 || details.Dropped != 1 {
		t.Fatalf("status details %+v", details)
	}
	if got := status("ended", completed).state; got != contracts.RoomCompleted {
		t.Fatalf("all targets completed = %s", got)
	}
	if got := status("ended", dropped).state; got != contracts.RoomFailed {
		t.Fatalf("no target completed = %s", got)
	}
	if got := status("source_lost", contracts.RoomStreamTargetState{TargetMemberID: "bob", State: "failed"}).state; got != contracts.RoomFailed {
		t.Fatalf("lost source = %s", got)
	}
	if got := status("expired", contracts.RoomStreamTargetState{TargetMemberID: "bob", State: "joining"}); got.state != contracts.RoomExpired ||
		got.destinations[0].Status != contracts.DestinationReady {
		t.Fatalf("expired status %+v", got)
	}
}
