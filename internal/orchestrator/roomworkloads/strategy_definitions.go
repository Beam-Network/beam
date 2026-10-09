package roomworkloads

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/workload/contracts"
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

func newMediaStrategy() MediaStrategy {
	return MediaStrategy{strategyBase: strategyBase[contracts.MediaUnitDetails, contracts.MediaResultDetails]{
		kind: domain.KindRoomMedia, class: domain.ClassService, dispatchSource: dispatch.SourceRoomMedia,
		validateDetails: func(value contracts.MediaUnitDetails) error {
			if value.Profile == "" {
				value.Profile = contracts.RoomMediaLegacyProfile
			}
			if value.SessionID == "" || value.Replay || (value.Service != "publish" && value.Service != "relay") ||
				(value.Profile != contracts.RoomMediaLegacyProfile && value.Profile != contracts.RoomMediaWebRTCWorkerProfile) ||
				len(value.Tracks) == 0 || len(value.Tracks) > 128 || value.Layers == nil || len(value.Layers) > 512 {
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
		progressDetails: mediaProgress, resultDetails: mediaResult, payload: mediaPayload,
		allowNoTargets: func(value contracts.MediaUnitDetails) bool { return lateJoinMedia(value.Profile) },
	}}
}

// ValidateDefinition adds worker-hosted WebRTC admission, which needs the
// identity, targets, and capacity alongside the media details.
func (s MediaStrategy) ValidateDefinition(value genericDefinition[contracts.MediaUnitDetails], now time.Time) error {
	if err := s.strategyBase.ValidateDefinition(value, now); err != nil {
		return err
	}
	definition := value.Workload
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
