package roomworkloads

import (
	"errors"

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
		payload: workerPayload[contracts.MessageUnitDetails, contracts.MessageProgressDetails],
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
		validateDetails: func(value contracts.StreamUnitDetails) error {
			if value.SessionID == "" || value.Replay || value.Protocol == "" || value.MaxBufferBytes <= 0 ||
				(value.BackpressurePolicy != "drop_oldest" && value.BackpressurePolicy != "drop_newest" && value.BackpressurePolicy != "block") {
				return errors.New("invalid room.stream details")
			}
			return nil
		},
		progressDetails: decodeProgress[contracts.StreamProgressDetails], resultDetails: decodeResult[contracts.StreamResultDetails],
		payload: workerPayload[contracts.StreamUnitDetails, contracts.StreamResultDetails],
	}}
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
	}}
}
