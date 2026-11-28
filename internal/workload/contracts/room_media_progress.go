package contracts

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type mediaProgressWire struct {
	Sequence uint64                   `json:"sequence"`
	Packets  int64                    `json:"packets"`
	Bytes    int64                    `json:"bytes"`
	Dropped  int64                    `json:"dropped"`
	Tracks   []MediaTrackCounters     `json:"tracks"`
	Runtime  json.RawMessage          `json:"runtime,omitempty"`
	Load     *mediacontracts.NodeLoad `json:"load,omitempty"`
}

type mediaResultWire struct {
	mediaProgressWire
	TerminalReason string `json:"terminal_reason"`
}

func (v MediaProgressDetails) MarshalJSON() ([]byte, error) {
	runtime, err := encodeMediaRuntime(v.Runtime, v.NodeRuntime)
	if err != nil {
		return nil, err
	}
	return json.Marshal(mediaProgressWire{Sequence: v.Sequence, Packets: v.Packets, Bytes: v.Bytes, Dropped: v.Dropped,
		Tracks: v.Tracks, Runtime: runtime, Load: v.Load})
}

func (v *MediaProgressDetails) UnmarshalJSON(data []byte) error {
	var wire mediaProgressWire
	if err := decodeStrictMedia(data, &wire); err != nil {
		return err
	}
	legacy, node, err := decodeMediaRuntime(wire.Runtime)
	if err != nil {
		return err
	}
	*v = MediaProgressDetails{Sequence: wire.Sequence, Packets: wire.Packets, Bytes: wire.Bytes, Dropped: wire.Dropped,
		Tracks: wire.Tracks, Runtime: legacy, NodeRuntime: node, Load: wire.Load}
	return nil
}

func (v MediaResultDetails) MarshalJSON() ([]byte, error) {
	runtime, err := encodeMediaRuntime(v.Runtime, v.NodeRuntime)
	if err != nil {
		return nil, err
	}
	return json.Marshal(mediaResultWire{mediaProgressWire: mediaProgressWire{Sequence: v.Sequence, Packets: v.Packets,
		Bytes: v.Bytes, Dropped: v.Dropped, Tracks: v.Tracks, Runtime: runtime, Load: v.Load}, TerminalReason: v.TerminalReason})
}

func (v *MediaResultDetails) UnmarshalJSON(data []byte) error {
	var wire mediaResultWire
	if err := decodeStrictMedia(data, &wire); err != nil {
		return err
	}
	legacy, node, err := decodeMediaRuntime(wire.Runtime)
	if err != nil {
		return err
	}
	*v = MediaResultDetails{Sequence: wire.Sequence, Packets: wire.Packets, Bytes: wire.Bytes, Dropped: wire.Dropped,
		Tracks: wire.Tracks, Runtime: legacy, NodeRuntime: node, Load: wire.Load, TerminalReason: wire.TerminalReason}
	return nil
}

func encodeMediaRuntime(legacy *MediaRuntime, node *mediacontracts.NodeRuntime) (json.RawMessage, error) {
	switch {
	case legacy != nil && node != nil:
		return nil, errors.New("room.media progress carries one runtime")
	case legacy != nil:
		return json.Marshal(legacy)
	case node != nil:
		return mediacontracts.MarshalWire(*node)
	default:
		return nil, nil
	}
}

func decodeMediaRuntime(raw json.RawMessage) (*MediaRuntime, *mediacontracts.NodeRuntime, error) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, nil, nil
	}
	var header struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, nil, err
	}
	if header.SchemaVersion == RoomMediaNodeRuntimeSchema {
		var node mediacontracts.NodeRuntime
		if err := mediacontracts.UnmarshalStrict(raw, &node); err != nil {
			return nil, nil, nodeIssue("details.runtime", "invalid media-node-runtime/v1")
		}
		if err := ValidateMediaNodeRuntime(node); err != nil {
			return nil, nil, nodeIssue("details.runtime", err.Error())
		}
		return nil, &node, nil
	}
	var legacy MediaRuntime
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, nil, nodeIssue("details.runtime", err.Error())
	}
	return &legacy, nil, nil
}

func decodeStrictMedia(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("room.media details contain trailing JSON")
	}
	return nil
}
