package roomworkloads

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/orchestrator/scheduling"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type DatagramStrategy struct {
	strategyBase[contracts.DatagramUnitDetails, contracts.DatagramResultDetails]
}
type MessageStrategy struct {
	strategyBase[contracts.MessageUnitDetails, contracts.MessageProgressDetails]
}
type CommandStrategy struct {
	strategyBase[contracts.CommandUnitDetails, contracts.CommandProgressDetails]
}
type StreamStrategy struct {
	strategyBase[contracts.StreamUnitDetails, contracts.StreamResultDetails]
}
type MediaStrategy struct {
	strategyBase[contracts.MediaUnitDetails, contracts.MediaResultDetails]
	media *mediaNodes
}

func newDatagramStrategy() DatagramStrategy {
	return DatagramStrategy{strategyBase: strategyBase[contracts.DatagramUnitDetails, contracts.DatagramResultDetails]{
		kind: domain.KindRoomDatagram, class: domain.ClassSession, dispatchSource: dispatch.SourceRoomDatagram,
		validateDetails: func(value contracts.DatagramUnitDetails) error {
			if value.TTLMS <= 0 || value.TTLMS > 3_600_000 || value.MaxPacketBytes <= 0 || value.MaxPacketBytes > 65_507 {
				return errors.New("invalid room.datagram details")
			}
			return nil
		},
		progressDetails: decodeProgress[contracts.DatagramProgressDetails], resultDetails: decodeResult[contracts.DatagramResultDetails],
		payload: workerPayload[contracts.DatagramUnitDetails, contracts.DatagramResultDetails],
	}}
}

func newMessageStrategy() MessageStrategy {
	return MessageStrategy{strategyBase: strategyBase[contracts.MessageUnitDetails, contracts.MessageProgressDetails]{
		kind: domain.KindRoomMessage, class: domain.ClassJob, dispatchSource: dispatch.SourceRoomMessage,
		validateDetails: func(value contracts.MessageUnitDetails) error {
			if value.MessageID == "" || value.ContentType == "" || value.SizeBytes <= 0 {
				return errors.New("invalid room.message details")
			}
			return nil
		},
		progressDetails: decodeProgress[contracts.MessageProgressDetails], resultDetails: decodeResult[contracts.MessageProgressDetails],
		payload:           workerPayload[contracts.MessageUnitDetails, contracts.MessageProgressDetails],
		extraCapabilities: []string{contracts.RoomMessageDirectCapability},
	}}
}

func (s MessageStrategy) ValidateProgress(def genericDefinition[contracts.MessageUnitDetails],
	attempt genericAttempt[contracts.MessageUnitDetails, contracts.MessageProgressDetails], value domain.Progress) (progress, error) {
	validated, err := s.strategyBase.ValidateProgress(def, attempt, value)
	if err != nil {
		return progress{}, err
	}
	var details contracts.MessageProgressDetails
	if err := json.Unmarshal(validated.Details, &details); err != nil {
		return progress{}, err
	}
	if details.Runtime == nil {
		return validated, nil
	}
	targets := make([]string, 0, len(def.Workload.Targets))
	for _, target := range def.Workload.Targets {
		targets = append(targets, target.MemberID)
	}
	if err := details.Runtime.Validate(validated.Identity, targets, time.Now().UTC()); err != nil {
		return progress{}, err
	}
	return validated, nil
}

func newCommandStrategy() CommandStrategy {
	return CommandStrategy{strategyBase: strategyBase[contracts.CommandUnitDetails, contracts.CommandProgressDetails]{
		kind: domain.KindRoomCommand, class: domain.ClassJob, dispatchSource: dispatch.SourceRoomCommand,
		validateDetails: func(value contracts.CommandUnitDetails) error {
			if value.CommandID == "" || value.Command == "" || value.Request == nil || value.TimeoutMS <= 0 || value.TimeoutMS > 3_600_000 {
				return errors.New("invalid room.command details")
			}
			return nil
		},
		progressDetails: decodeProgress[contracts.CommandProgressDetails], resultDetails: decodeResult[contracts.CommandProgressDetails],
		payload: workerPayload[contracts.CommandUnitDetails, contracts.CommandProgressDetails],
	}}
}

func newStreamStrategy() StreamStrategy {
	return StreamStrategy{strategyBase: strategyBase[contracts.StreamUnitDetails, contracts.StreamResultDetails]{
		kind: domain.KindRoomStream, class: domain.ClassSession, dispatchSource: dispatch.SourceRoomStream,
		validateDetails: contracts.StreamUnitDetails.Validate,
		progressDetails: decodeProgress[contracts.StreamProgressDetails], resultDetails: decodeResult[contracts.StreamResultDetails],
		payload:           workerPayload[contracts.StreamUnitDetails, contracts.StreamResultDetails],
		extraCapabilities: []string{contracts.RoomStreamDirectCapability},
		resources:         streamResources,
	}}
}

// streamResources reserves the replay window, eight pipelined frames and
// 1 MiB of overhead, one connection per target plus nine for the source
// pipeline, and one stream per participant.
func streamResources(unit contracts.RoomWorkloadDefinition[contracts.StreamUnitDetails]) domain.Resources {
	const pipelineBytes, overheadBytes = 8 * contracts.RoomStreamMaxFrameBytes, 1 << 20
	resources := unit.Resources
	targets := int64(len(unit.Targets))
	resources.MemoryBytes = max(resources.MemoryBytes, unit.Details.MaxBufferBytes+pipelineBytes+overheadBytes)
	resources.Connections = max(resources.Connections, targets+9)
	resources.Streams = max(resources.Streams, targets+1)
	return resources
}

// ValidateDefinition requires one room.stream.direct.v1 capacity unit and at
// most 64 targets per lease.
func (s StreamStrategy) ValidateDefinition(value genericDefinition[contracts.StreamUnitDetails], now time.Time) error {
	if err := s.strategyBase.ValidateDefinition(value, now); err != nil {
		return err
	}
	definition := value.Workload
	if definition.RequiredCapacity != (contracts.RoomCapacityRequirement{Capability: contracts.RoomStreamDirectCapability, Units: 1}) {
		return errors.New("room.stream requires one room.stream.direct.v1 capacity unit")
	}
	if len(definition.Targets) > contracts.RoomStreamMaxTargets {
		return errors.New("room.stream allows at most 64 targets per lease")
	}
	return nil
}

// ValidateProgress checks a reported Worker runtime against the attempt
// identity and its destinations before it is announced.
func (s StreamStrategy) ValidateProgress(def genericDefinition[contracts.StreamUnitDetails],
	attempt genericAttempt[contracts.StreamUnitDetails, contracts.StreamResultDetails], value domain.Progress) (progress, error) {
	validated, err := s.strategyBase.ValidateProgress(def, attempt, value)
	if err != nil {
		return progress{}, err
	}
	var details contracts.StreamProgressDetails
	if err := json.Unmarshal(validated.Details, &details); err != nil {
		return progress{}, err
	}
	if details.Runtime == nil {
		return validated, nil
	}
	targets := make([]string, 0, len(def.Workload.Targets))
	for _, target := range def.Workload.Targets {
		targets = append(targets, target.MemberID)
	}
	if err := details.Runtime.Validate(validated.Identity, targets, time.Now().UTC()); err != nil {
		return progress{}, err
	}
	return validated, nil
}

func newMediaStrategy() MediaStrategy { return newMediaStrategyWith(nil) }

func newMediaStrategyWith(media *mediaNodes) MediaStrategy {
	return MediaStrategy{media: media, strategyBase: strategyBase[contracts.MediaUnitDetails, contracts.MediaResultDetails]{
		kind: domain.KindRoomMedia, class: domain.ClassService, dispatchSource: dispatch.SourceRoomMedia,
		validateDetails: func(value contracts.MediaUnitDetails) error {
			if contracts.IsRoomMediaNodeProfile(value.Profile) {
				return validateMediaNodeUnit(value)
			}
			if value.Profile == "" {
				value.Profile = contracts.RoomMediaLegacyProfile
			}
			if value.SessionID == "" || value.Replay || (value.Service != "publish" && value.Service != "relay") ||
				(value.Profile != contracts.RoomMediaLegacyProfile && value.Profile != contracts.RoomMediaWebRTCWorkerProfile) ||
				len(value.Tracks) == 0 || len(value.Tracks) > 128 || value.Layers == nil || len(value.Layers) > 512 || value.IsNode() {
				return errors.New("invalid room.media details")
			}
			if value.Profile == contracts.RoomMediaWebRTCWorkerProfile &&
				(value.Protection.Scheme != contracts.RoomMediaProtectionSchemeV1 ||
					value.Protection.KeyScope != contracts.RoomMediaProtectionChannel || !value.Protection.Required) {
				return errors.New("room.media WebRTC protection must be channel-scoped and required")
			}
			for _, track := range value.Tracks {
				if track.TrackID == "" || (track.Kind != "audio" && track.Kind != "video" && track.Kind != "data") {
					return errors.New("invalid room.media track")
				}
			}
			for _, layer := range value.Layers {
				if layer.LayerID == "" || layer.TrackID == "" || layer.BitrateBPS <= 0 {
					return errors.New("invalid room.media layer")
				}
			}
			return nil
		},
		progressDetails: mediaProgress, resultDetails: mediaResult,
		payload: func(definition contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails],
			attempt genericAttempt[contracts.MediaUnitDetails, contracts.MediaResultDetails]) (json.RawMessage, error) {
			return mediaPayload(media.withLatestView(definition), attempt)
		},
		allowNoTargets: func(value contracts.MediaUnitDetails) bool { return lateJoinMedia(value.Profile) },
		resources:      mediaResources,
	}}
}

func validateMediaNodeUnit(value contracts.MediaUnitDetails) error {
	if value.SessionID == "" || value.Replay || value.Service != "" || value.Tracks != nil || value.Layers != nil ||
		value.HeartbeatTimeoutMS <= 0 || value.HeartbeatTimeoutMS > contracts.RoomMediaNodeMaxHeartbeatTimeoutMS ||
		mediacontracts.RoleProfiles[value.Role] != value.Profile {
		return errors.New("invalid room.media node details")
	}
	topology, err := value.NodeTopology()
	if err != nil {
		return fmt.Errorf("invalid room.media node topology: %w", err)
	}
	node, ok := contracts.NodeEntry(topology, value.NodeID)
	if !ok || node.Role != value.Role || topology.View == nil || topology.View.NodeID != value.NodeID || topology.SessionID != value.SessionID {
		return errors.New("room.media node is not the view's node")
	}
	if contracts.NodeHasParentRole(value.Role, topology.DeliveryProfile) != (value.ServiceToken != nil) ||
		contracts.NodeServesHLS(value.Role) != (value.PlaybackSecret != nil) {
		return errors.New("room.media node credentials do not match its role")
	}
	return nil
}

func mediaResources(unit contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails]) domain.Resources {
	resources := unit.Resources
	if !contracts.IsRoomMediaNodeProfile(unit.Details.Profile) {
		resources.MemoryBytes = max(resources.MemoryBytes, 32<<20)
		resources.Connections = max(resources.Connections, int64(len(unit.Targets)+1))
		resources.Streams = max(resources.Streams, int64(len(unit.Targets)+1))
		return resources
	}
	memory := map[string]int64{mediacontracts.RoleSFU: 128 << 20, mediacontracts.RoleRelay: 128 << 20, mediacontracts.RoleIngest: 128 << 20,
		mediacontracts.RoleTranscode: 512 << 20, mediacontracts.RolePackage: 256 << 20, mediacontracts.RoleShield: 256 << 20,
		mediacontracts.RoleEdge: 256 << 20}[unit.Details.Role]
	resources.MemoryBytes = max(resources.MemoryBytes, memory)
	resources.Connections = max(resources.Connections, 16)
	resources.Streams = max(resources.Streams, 16)
	return resources
}

func (s MediaStrategy) ValidateDefinition(value genericDefinition[contracts.MediaUnitDetails], now time.Time) error {
	if err := s.strategyBase.ValidateDefinition(value, now); err != nil {
		return err
	}
	definition := value.Workload
	if contracts.IsRoomMediaNodeProfile(definition.Details.Profile) {
		if err := contracts.ValidateMediaNodeEnvelope(definition.Details.Role, definition.RequiredCapacity, len(definition.Targets), nil); err != nil {
			return err
		}
		if definition.Source.Protocol != contracts.RoomMediaNodePathProtocol {
			return errors.New("room.media nodes use the beam.media.node/1 path protocol")
		}
		if definition.Identity.ExpiresAt.After(now.Add(24 * time.Hour)) {
			return errors.New("a room.media node lease lasts at most 24 hours")
		}
		return nil
	}
	if definition.Details.Profile != contracts.RoomMediaWebRTCWorkerProfile {
		return nil
	}
	if definition.RequiredCapacity != (contracts.RoomCapacityRequirement{Capability: contracts.RoomMediaWebRTCCapability, Units: 1}) {
		return errors.New("room.media WebRTC requires one room.media.webrtc.v1 capacity unit")
	}
	targets := make([]string, 0, len(definition.Targets))
	for _, target := range definition.Targets {
		targets = append(targets, target.MemberID)
	}
	return contracts.ValidateWebRTCMediaAdmission(definition.Identity, targets, definition.Details, now)
}

func (s MediaStrategy) ValidateProgress(def genericDefinition[contracts.MediaUnitDetails],
	attempt genericAttempt[contracts.MediaUnitDetails, contracts.MediaResultDetails], value domain.Progress) (progress, error) {
	validated, err := s.strategyBase.ValidateProgress(def, attempt, value)
	if err != nil {
		return progress{}, err
	}
	var details contracts.MediaProgressDetails
	if err := json.Unmarshal(validated.Details, &details); err != nil {
		return progress{}, err
	}
	if reason := contracts.MediaNodeRuntimeMismatch(def.Workload.Details, details.NodeRuntime, details.Runtime, attempt.WorkerID); reason != "" {
		return progress{}, errors.New(reason)
	}
	if details.NodeRuntime != nil && s.media != nil {
		s.media.rememberNodeKey(def.ID, details.NodeRuntime.NodeKey)
	}
	if details.Load != nil {
		if !contracts.IsRoomMediaNodeProfile(def.Workload.Details.Profile) {
			return progress{}, errors.New("only room.media nodes report a load")
		}
		if err := contracts.ValidateMediaNodeLoad(*details.Load); err != nil {
			return progress{}, err
		}
	}
	return validated, nil
}

func (s MediaStrategy) Place(def genericDefinition[contracts.MediaUnitDetails], unit genericUnit[contracts.MediaUnitDetails],
	required []string, resources domain.Resources, excluded []string) (scheduling.Placement, error) {
	if s.media == nil {
		return scheduling.Placement{}, errors.New("room.media placement is not configured")
	}
	return s.media.place(def.Workload, required, resources, excluded)
}
