package roomworkloads

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/orchestrator/roomworkload"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type progress = contracts.RoomGenericProgress

type strategyBase[T, R any] struct {
	kind            domain.Kind
	class           domain.Class
	dispatchSource  dispatch.Source
	validateDetails func(T) error
	progressDetails func(map[string]string) (json.RawMessage, error)
	resultDetails   func(map[string]string) (R, error)
	payload         func(contracts.RoomWorkloadDefinition[T], genericAttempt[T, R]) (json.RawMessage, error)
}

type genericDefinition[T any] = roomworkload.RoomWorkloadDefinition[contracts.RoomWorkloadDefinition[T]]
type genericUnit[T any] = roomworkload.RoomExecutionUnit[contracts.RoomWorkloadDefinition[T]]
type genericPath = roomworkload.RoomPath[contracts.RoomPathIntent, contracts.RoomPathLease]
type genericAttempt[T, R any] = roomworkload.RoomAttempt[contracts.RoomWorkloadDefinition[T], contracts.RoomPathIntent,
	contracts.RoomPathLease, progress, R]

func (s strategyBase[T, R]) ValidateDefinition(value genericDefinition[T], now time.Time) error {
	definition := value.Workload
	if definition.Schema != contracts.RoomWorkloadSchema {
		return errors.New("unsupported internal room workload schema")
	}
	if err := definition.Identity.Validate(s.kind, now); err != nil {
		return err
	}
	if len(definition.Targets) == 0 || len(definition.Targets) > 10_000 {
		return errors.New("room workload requires between 1 and 10000 targets")
	}
	if definition.Source.PathID == "" || definition.Source.Role != "source" {
		return errors.New("room workload source path is invalid")
	}
	seen := make(map[string]struct{}, len(definition.Targets))
	for _, target := range definition.Targets {
		if target.MemberID == "" || target.Path.PathID == "" || target.Path.Role != "target" ||
			target.Path.TargetMemberID != target.MemberID {
			return errors.New("room workload target path is invalid")
		}
		if _, duplicate := seen[target.MemberID]; duplicate {
			return fmt.Errorf("duplicate room workload target %s", target.MemberID)
		}
		seen[target.MemberID] = struct{}{}
	}
	if err := definition.Resources.Validate(); err != nil {
		return err
	}
	if definition.RequiredCapacity.Capability == "" || definition.RequiredCapacity.Units <= 0 {
		return errors.New("room workload required capacity is invalid")
	}
	return s.validateDetails(definition.Details)
}

func (s strategyBase[T, R]) SameDefinition(a, b contracts.RoomWorkloadDefinition[T]) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func (s strategyBase[T, R]) ExecutionUnits(value genericDefinition[T]) ([]genericUnit[T], error) {
	return []genericUnit[T]{{ID: value.Workload.Identity.UnitID, Unit: value.Workload}}, nil
}

func (s strategyBase[T, R]) Paths(_ genericDefinition[T], value genericUnit[T]) (genericPath, []genericPath, error) {
	source := genericPath{ID: value.Unit.Source.PathID, Intent: value.Unit.Source}
	targets := make([]genericPath, 0, len(value.Unit.Targets))
	for _, target := range value.Unit.Targets {
		targets = append(targets, genericPath{ID: target.MemberID, TargetID: target.MemberID, Intent: target.Path})
	}
	return source, targets, nil
}

func (s strategyBase[T, R]) RequiredCapabilities(def genericDefinition[T], _ genericUnit[T]) []string {
	capability := def.Workload.RequiredCapacity.Capability
	if capability == string(s.kind) {
		return []string{capability}
	}
	return []string{string(s.kind), capability}
}

func (s strategyBase[T, R]) Resources(_ genericDefinition[T], value genericUnit[T]) domain.Resources {
	resources := value.Unit.Resources
	resources.MemoryBytes = max(resources.MemoryBytes, 32<<20)
	resources.Connections = max(resources.Connections, int64(len(value.Unit.Targets)+1))
	if s.class == domain.ClassSession || s.class == domain.ClassService {
		resources.Streams = max(resources.Streams, int64(len(value.Unit.Targets)+1))
	}
	return resources
}

func (s strategyBase[T, R]) ValidateCredential(value genericPath, lease contracts.RoomPathLease, now time.Time) error {
	return lease.Validate(value.Intent, now)
}

func (s strategyBase[T, R]) BuildWorkerSpec(def genericDefinition[T], attempt genericAttempt[T, R], _ time.Time) (domain.Spec, error) {
	definition := def.Workload
	payload, err := s.payload(definition, attempt)
	if err != nil {
		return domain.Spec{}, err
	}
	identity := definition.Identity
	identity.WorkerID = attempt.WorkerID
	workID, attemptID := workloadIdentity(identity)
	return domain.Spec{WorkloadID: workID, AttemptID: attemptID,
		Identity: domain.Identity{WorkerID: attempt.WorkerID, NodeID: attempt.NodeID}, Kind: s.kind, Class: s.class,
		Source:               domain.Source{System: "beamcore.room-workload", Reference: identity.WorkloadID},
		RequiredCapabilities: s.RequiredCapabilities(def, attempt.Unit), Resources: s.Resources(def, attempt.Unit),
		Lease:    domain.Lease{OfferExpiresAt: identity.ExpiresAt, AssignmentExpiresAt: identity.ExpiresAt},
		Evidence: domain.EvidencePolicy{ReceiptRequired: true}, Payload: payload}, nil
}

func (s strategyBase[T, R]) ValidateProgress(def genericDefinition[T], attempt genericAttempt[T, R], value domain.Progress) (progress, error) {
	if value.WorkloadID+"/"+value.AttemptID != attempt.WorkloadKey {
		return progress{}, errors.New("room workload progress identity mismatch")
	}
	details, err := s.progressDetails(value.Outputs)
	if err != nil {
		return progress{}, err
	}
	identity := def.Workload.Identity
	identity.WorkerID = attempt.WorkerID
	return progress{Identity: identity, Details: details, At: value.ObservedAt}, nil
}

func (s strategyBase[T, R]) ValidateResult(_ genericDefinition[T], _ genericAttempt[T, R], result domain.Result) (R, error) {
	return s.resultDetails(result.Outputs)
}

func (s strategyBase[T, R]) Aggregate(def genericDefinition[T], attempts map[string]genericAttempt[T, R]) (contracts.RoomGenericResult, bool) {
	identity := def.Workload.Identity
	for _, attempt := range attempts {
		identity.WorkerID = attempt.WorkerID
		switch attempt.State {
		case roomworkload.AttemptCompleted:
			if attempt.Result == nil {
				return contracts.RoomGenericResult{}, false
			}
			details, err := json.Marshal(attempt.Result.Result)
			if err != nil {
				return contracts.RoomGenericResult{}, false
			}
			return contracts.RoomGenericResult{Identity: identity, State: contracts.RoomCompleted, Details: details}, true
		case roomworkload.AttemptFailed:
			detail := attempt.Error
			details, _ := failedResultDetails[R](s.kind, def.Workload, detail, "worker_failed")
			return contracts.RoomGenericResult{Identity: identity, State: contracts.RoomFailed,
				Details: details, Failures: []contracts.RoomWorkloadFailure{{Origin: "worker", Code: "execution_failed", Retryable: true, Detail: &detail}}}, true
		case roomworkload.AttemptCancelled:
			detail := attempt.Error
			details, _ := failedResultDetails[R](s.kind, def.Workload, detail, "expired")
			return contracts.RoomGenericResult{Identity: identity, State: contracts.RoomCancelled, Details: details,
				Failures: []contracts.RoomWorkloadFailure{{Origin: "orchestrator", Code: "cancelled", Retryable: false, Detail: &detail}}}, true
		default:
			return contracts.RoomGenericResult{}, false
		}
	}
	return contracts.RoomGenericResult{}, false
}

func failedResultDetails[R, T any](kind domain.Kind, definition contracts.RoomWorkloadDefinition[T], reason, terminal string) (json.RawMessage, error) {
	var value any
	switch kind {
	case domain.KindRoomDatagram:
		value = contracts.DatagramResultDetails{}
	case domain.KindRoomMessage:
		deliveries := make([]contracts.MessageDelivery, 0, len(definition.Targets))
		for _, target := range definition.Targets {
			reasonCopy := reason
			if len(reasonCopy) > 512 {
				reasonCopy = reasonCopy[:512]
			}
			deliveries = append(deliveries, contracts.MessageDelivery{TargetMemberID: target.MemberID, State: "failed", Reason: &reasonCopy})
		}
		value = contracts.MessageProgressDetails{Deliveries: deliveries}
	case domain.KindRoomCommand:
		outcomes := make([]contracts.CommandOutcome, 0, len(definition.Targets))
		for _, target := range definition.Targets {
			outcomes = append(outcomes, contracts.CommandOutcome{TargetMemberID: target.MemberID, State: "failed", Reason: reason})
		}
		value = contracts.CommandProgressDetails{Outcomes: outcomes}
	case domain.KindRoomStream:
		value = contracts.StreamResultDetails{TerminalReason: terminal}
	case domain.KindRoomMedia:
		var details contracts.MediaUnitDetails
		if typed, ok := any(definition.Details).(contracts.MediaUnitDetails); ok {
			details = typed
		}
		tracks := make([]contracts.MediaTrackCounters, 0, len(details.Tracks))
		for _, track := range details.Tracks {
			tracks = append(tracks, contracts.MediaTrackCounters{TrackID: track.TrackID})
		}
		value = contracts.MediaResultDetails{Tracks: tracks, TerminalReason: terminal}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var typed R
	if err := json.Unmarshal(encoded, &typed); err != nil {
		return nil, err
	}
	return json.Marshal(typed)
}

func (s strategyBase[T, R]) DispatchSource() dispatch.Source { return s.dispatchSource }

func workerPayload[T, R any](definition contracts.RoomWorkloadDefinition[T], attempt genericAttempt[T, R]) (json.RawMessage, error) {
	if attempt.Source.Credential == nil {
		return nil, errors.New("room workload source path is not provisioned")
	}
	targets := make([]contracts.RoomWorkerTarget, 0, len(definition.Targets))
	for _, target := range definition.Targets {
		path := attempt.Targets[target.MemberID]
		if path.Credential == nil {
			return nil, fmt.Errorf("room workload target %s is not provisioned", target.MemberID)
		}
		targets = append(targets, contracts.RoomWorkerTarget{MemberID: target.MemberID, Path: *path.Credential})
	}
	identity := definition.Identity
	identity.WorkerID = attempt.WorkerID
	return json.Marshal(contracts.RoomWorkerSpec[T]{Schema: contracts.RoomWorkloadSchema, Identity: identity,
		Source: *attempt.Source.Credential, Targets: targets, Details: definition.Details})
}

func mediaPayload(definition contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails], attempt genericAttempt[contracts.MediaUnitDetails, contracts.MediaResultDetails]) (json.RawMessage, error) {
	if definition.Details.Profile == contracts.RoomMediaWebRTCWorkerProfile {
		return workerPayload[contracts.MediaUnitDetails, contracts.MediaResultDetails](definition, attempt)
	}
	if attempt.Source.Credential == nil {
		return nil, errors.New("room.media source path is not provisioned")
	}
	members := map[string]string{definition.Identity.SourceMemberID: mediaMemberToken(*attempt.Source.Credential, definition.Source.Authorization)}
	for _, target := range definition.Targets {
		path := attempt.Targets[target.MemberID]
		if path.Credential == nil {
			return nil, fmt.Errorf("room.media target %s is not provisioned", target.MemberID)
		}
		members[target.MemberID] = mediaMemberToken(*path.Credential, target.Path.Authorization)
	}
	role := "relay"
	return json.Marshal(contracts.RoomAssignment{RoomID: definition.Identity.RoomID, Role: role, Protocol: "udp",
		ListenAddress: "127.0.0.1:0", Members: members, MaxDatagramBytes: 1200,
		IdleTimeoutSeconds: max(1, int64(time.Until(definition.Identity.ExpiresAt)/time.Second)),
		Metadata:           map[string]string{"room_workload_id": definition.Identity.WorkloadID, "session_id": definition.Details.SessionID}})
}

func mediaMemberToken(lease contracts.RoomPathLease, fallback string) string {
	for _, endpoint := range lease.Endpoints {
		for key, value := range endpoint.Headers {
			if strings.EqualFold(key, "X-Beam-Room-Token") {
				return value
			}
			if strings.EqualFold(key, "Authorization") {
				return strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
			}
		}
	}
	return fallback
}

func decodeProgress[T any](outputs map[string]string) (json.RawMessage, error) {
	encoded := outputs["room_progress_details"]
	if encoded == "" {
		return nil, errors.New("Worker room progress details are missing")
	}
	var value T
	if err := decodeStrictDetails([]byte(encoded), &value); err != nil {
		return nil, err
	}
	if err := validateDetails(value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func decodeResult[T any](outputs map[string]string) (T, error) {
	var value T
	encoded := outputs["room_result_details"]
	if encoded == "" {
		return value, errors.New("Worker room result details are missing")
	}
	if err := decodeStrictDetails([]byte(encoded), &value); err != nil {
		return value, err
	}
	return value, validateDetails(value)
}

func decodeStrictDetails(encoded []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("room workload details contain trailing JSON")
	}
	return nil
}

func validateDetails(value any) error {
	switch current := value.(type) {
	case contracts.DatagramProgressDetails:
		if current.PacketsDelta < 0 || current.BytesDelta < 0 || current.DroppedDelta < 0 {
			return errors.New("negative room.datagram progress counter")
		}
	case contracts.DatagramResultDetails:
		if current.Packets < 0 || current.Bytes < 0 || current.Dropped < 0 {
			return errors.New("negative room.datagram result counter")
		}
	case contracts.MessageProgressDetails:
		if len(current.Deliveries) == 0 || len(current.Deliveries) > 10_000 {
			return errors.New("room.message deliveries are empty or too large")
		}
		for _, delivery := range current.Deliveries {
			if delivery.TargetMemberID == "" || (delivery.State != "delivered" && delivery.State != "failed") {
				return errors.New("invalid room.message delivery")
			}
			if delivery.Reason != nil && len(*delivery.Reason) > 512 {
				return errors.New("room.message delivery reason is too long")
			}
		}
	case contracts.CommandProgressDetails:
		if len(current.Outcomes) == 0 || len(current.Outcomes) > 10_000 {
			return errors.New("room.command outcomes are empty or too large")
		}
		for _, outcome := range current.Outcomes {
			if outcome.TargetMemberID == "" || (outcome.State != "acknowledged" && outcome.State != "responded" && outcome.State != "failed") {
				return errors.New("invalid room.command outcome")
			}
			switch outcome.State {
			case "acknowledged":
				if outcome.Response != nil || outcome.Reason != "" {
					return errors.New("acknowledged room.command outcome has extra fields")
				}
			case "responded":
				if outcome.Response == nil || outcome.Reason != "" {
					return errors.New("responded room.command outcome is incomplete")
				}
			case "failed":
				if outcome.Response != nil || outcome.Reason == "" {
					return errors.New("failed room.command outcome is incomplete")
				}
			}
		}
	case contracts.StreamProgressDetails:
		if current.Bytes < 0 || current.BufferedBytes < 0 || current.Dropped < 0 {
			return errors.New("negative room.stream progress counter")
		}
	case contracts.StreamResultDetails:
		if current.Bytes < 0 || current.BufferedBytes < 0 || current.Dropped < 0 ||
			(current.TerminalReason != "ended" && current.TerminalReason != "source_closed" && current.TerminalReason != "worker_failed" && current.TerminalReason != "expired") {
			return errors.New("invalid room.stream result")
		}
	case contracts.MediaProgressDetails:
		if current.Packets < 0 || current.Bytes < 0 || current.Dropped < 0 || current.Tracks == nil || len(current.Tracks) > 128 {
			return errors.New("invalid room.media progress")
		}
		if err := validateMediaCounters(current.Tracks); err != nil {
			return err
		}
		if err := validateMediaRuntime(current.Runtime); err != nil {
			return err
		}
	case contracts.MediaResultDetails:
		if current.Packets < 0 || current.Bytes < 0 || current.Dropped < 0 || current.Tracks == nil || len(current.Tracks) > 128 ||
			(current.TerminalReason != "ended" && current.TerminalReason != "source_closed" && current.TerminalReason != "worker_failed" && current.TerminalReason != "expired") {
			return errors.New("invalid room.media result")
		}
		if err := validateMediaCounters(current.Tracks); err != nil {
			return err
		}
		if err := validateMediaRuntime(current.Runtime); err != nil {
			return err
		}
	}
	return nil
}

func validateMediaCounters(tracks []contracts.MediaTrackCounters) error {
	for _, track := range tracks {
		if track.TrackID == "" || track.Packets < 0 || track.Bytes < 0 || track.Dropped < 0 {
			return errors.New("invalid room.media track counters")
		}
	}
	return nil
}

func validateMediaRuntime(runtime *contracts.MediaRuntime) error {
	if runtime == nil {
		return nil
	}
	endpoint, err := url.Parse(runtime.BaseURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		runtime.Capability != contracts.RoomMediaWebRTCCapability || runtime.Transport != "worker_sfu" ||
		runtime.AccessToken == "" || runtime.ExpiresAt.IsZero() {
		return errors.New("invalid room.media Worker runtime")
	}
	return nil
}

func mediaProgress(outputs map[string]string) (json.RawMessage, error) {
	if encoded := outputs["room_progress_details"]; encoded != "" {
		return decodeProgress[contracts.MediaProgressDetails](outputs)
	}
	value := contracts.MediaProgressDetails{Tracks: []contracts.MediaTrackCounters{}}
	value.Packets, _ = strconv.ParseInt(outputs["packets"], 10, 64)
	value.Dropped, _ = strconv.ParseInt(outputs["dropped"], 10, 64)
	return json.Marshal(value)
}

func mediaResult(outputs map[string]string) (contracts.MediaResultDetails, error) {
	if encoded := outputs["room_result_details"]; encoded != "" {
		return decodeResult[contracts.MediaResultDetails](outputs)
	}
	packets, _ := strconv.ParseInt(outputs["packets"], 10, 64)
	dropped, _ := strconv.ParseInt(outputs["dropped"], 10, 64)
	return contracts.MediaResultDetails{Packets: packets, Dropped: dropped, Tracks: []contracts.MediaTrackCounters{}, TerminalReason: "source_closed"}, nil
}

func workloadIdentity(identity contracts.RoomWorkloadIdentity) (string, string) {
	digest := sha256.Sum256([]byte(identity.WorkloadID + "\x00" + identity.UnitID))
	return "room-workload-" + hex.EncodeToString(digest[:12]), fmt.Sprintf("epoch-%d-attempt-%d", identity.Epoch, identity.Attempt)
}
