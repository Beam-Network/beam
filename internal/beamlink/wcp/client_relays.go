package wcp

import (
	"sync/atomic"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

// StorageProbeRelayEmitter sends a Worker's relay messages to its Orchestrator.
type StorageProbeRelayEmitter interface {
	Opened(contracts.StorageProbeRelayOpened) error
	Data(contracts.StorageProbeRelayData) error
	Close(contracts.StorageProbeRelayClose) error
}

// StorageProbeRelaySession is the Worker relay service bound to one WCP
// session. Its methods must not block on network I/O.
type StorageProbeRelaySession interface {
	Open(contracts.StorageProbeRelayOpen)
	Data(contracts.StorageProbeRelayData)
	Close(contracts.StorageProbeRelayClose)
	CloseAll()
}

// StorageProbeRelayBinder binds the relay service to a new WCP session for the
// Orchestrator hotkey announced in its welcome.
type StorageProbeRelayBinder func(orchestratorHotkey string, emitter StorageProbeRelayEmitter) StorageProbeRelaySession

type relayEmitter struct {
	framed *framedConn
	cursor *atomic.Uint64
}

func (e relayEmitter) Opened(value contracts.StorageProbeRelayOpened) error {
	_, err := e.framed.write(TypeStorageProbeRelayOpened, "", value, e.cursor.Load())
	return err
}

func (e relayEmitter) Data(value contracts.StorageProbeRelayData) error {
	_, err := e.framed.write(TypeStorageProbeRelayData, "", value, e.cursor.Load())
	return err
}

func (e relayEmitter) Close(value contracts.StorageProbeRelayClose) error {
	_, err := e.framed.write(TypeStorageProbeRelayClose, "", value, e.cursor.Load())
	return err
}

// handleStorageProbeRelay dispatches one Orchestrator relay message. Relay
// problems never end the WCP session.
func handleStorageProbeRelay(relays StorageProbeRelaySession, emitter StorageProbeRelayEmitter, envelope Envelope) {
	relayID := func() string {
		loose, _ := decodePayload[struct {
			RelayID string `json:"relay_id"`
		}](envelope)
		return loose.RelayID
	}
	switch envelope.Type {
	case TypeStorageProbeRelayOpen:
		open, err := decodeStrictPayload[contracts.StorageProbeRelayOpen](envelope)
		reason := ""
		switch {
		case relays == nil:
			reason = contracts.StorageProbeRelayUnsupported
		case err != nil:
			reason = contracts.StorageProbeRelayIntentInvalid
		}
		if reason == "" {
			relays.Open(open)
		} else if id := relayID(); contracts.ValidStorageProbeRelayID(id) {
			_ = emitter.Close(contracts.StorageProbeRelayClose{RelayID: id, Reason: reason})
		}
	case TypeStorageProbeRelayData:
		if relays == nil {
			return
		}
		frame, err := decodeStrictPayload[contracts.StorageProbeRelayData](envelope)
		if err != nil {
			// An undecodable frame is out of sequence for its relay.
			frame = contracts.StorageProbeRelayData{RelayID: relayID(), Seq: -1}
		}
		relays.Data(frame)
	case TypeStorageProbeRelayClose:
		if relays != nil {
			relays.Close(contracts.StorageProbeRelayClose{RelayID: relayID()})
		}
	}
}
