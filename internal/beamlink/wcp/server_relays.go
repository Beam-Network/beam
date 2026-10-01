package wcp

import (
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

// StorageProbeRelayEvent is one relay message for BeamCore: a Worker's opened,
// data or close frame, or a close the Orchestrator originates.
type StorageProbeRelayEvent struct {
	Type    string
	RelayID string
	Seq     int64
	Data    string
	Reason  string
}

const (
	// maxStorageProbeRelays bounds the relays one Orchestrator routes at once.
	maxStorageProbeRelays = 64
	// A relay is forgotten shortly after expires_at so the Worker's own
	// timeout close still reaches BeamCore.
	storageProbeRelayForgetGrace = 2 * time.Second
)

type serverRelay struct {
	session *Session
	timer   *time.Timer
}

func advertisesStorageProbeRelay(manifest *contracts.CapabilityManifest) bool {
	return manifest != nil && contracts.AdvertisesCapabilityProtocol(*manifest, contracts.StorageProbeRelayCapability)
}

// StorageProbeRelayAvailable reports whether a connected Worker advertises
// storage.probe.relay.v1.
func (s *Server) StorageProbeRelayAvailable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, session := range s.sessions {
		if session.storageProbeRelay.Load() {
			return true
		}
	}
	return false
}

// SetStorageProbeRelaySink installs the publisher of relay messages to
// BeamCore. Clearing it ends every active relay.
func (s *Server) SetStorageProbeRelaySink(sink func(StorageProbeRelayEvent)) {
	s.relayMu.Lock()
	s.relaySink = sink
	var ended map[string]*serverRelay
	if sink == nil {
		ended = s.relays
		s.relays = make(map[string]*serverRelay)
		for _, relay := range ended {
			relay.timer.Stop()
		}
	}
	s.relayMu.Unlock()
	for relayID, relay := range ended {
		_, _ = relay.session.send(TypeStorageProbeRelayClose, "", contracts.StorageProbeRelayClose{
			RelayID: relayID, Reason: contracts.StorageProbeRelayCancelled,
		})
	}
}

// OpenStorageProbeRelay forwards a BeamCore open, unchanged, to the Worker it
// names. A non-empty result is the close reason to report instead. Opens of a
// relay that is already active are ignored.
func (s *Server) OpenStorageProbeRelay(workerID string, open contracts.StorageProbeRelayOpen, expiresAt time.Time) string {
	s.mu.RLock()
	session := s.sessions[workerID]
	s.mu.RUnlock()
	if session == nil {
		return contracts.StorageProbeRelayWorkerUnavailable
	}
	if !session.storageProbeRelay.Load() {
		return contracts.StorageProbeRelayUnsupported
	}
	s.relayMu.Lock()
	if _, exists := s.relays[open.RelayID]; exists {
		s.relayMu.Unlock()
		return ""
	}
	if len(s.relays) >= maxStorageProbeRelays {
		s.relayMu.Unlock()
		return contracts.StorageProbeRelayCapacityExhausted
	}
	relay := &serverRelay{session: session}
	relay.timer = time.AfterFunc(time.Until(expiresAt)+storageProbeRelayForgetGrace, func() {
		s.expireStorageProbeRelay(open.RelayID, relay)
	})
	s.relays[open.RelayID] = relay
	s.relayMu.Unlock()
	if _, err := session.send(TypeStorageProbeRelayOpen, "", open); err != nil && s.forgetStorageProbeRelay(open.RelayID, relay) {
		return contracts.StorageProbeRelayWorkerUnavailable
	}
	return ""
}

// ForwardStorageProbeRelayData passes one BeamCore frame to the relay's Worker.
func (s *Server) ForwardStorageProbeRelayData(frame contracts.StorageProbeRelayData) {
	s.relayMu.Lock()
	relay := s.relays[frame.RelayID]
	s.relayMu.Unlock()
	if relay != nil {
		// A failed send means the session is closing; its teardown reports worker_unavailable.
		_, _ = relay.session.send(TypeStorageProbeRelayData, "", frame)
	}
}

// CloseStorageProbeRelay forwards a close to the relay's Worker and forgets
// the relay. notifyBeamCore also publishes the close, for closes the
// Orchestrator originates.
func (s *Server) CloseStorageProbeRelay(request contracts.StorageProbeRelayClose, notifyBeamCore bool) {
	s.relayMu.Lock()
	relay := s.relays[request.RelayID]
	if relay == nil {
		s.relayMu.Unlock()
		return
	}
	delete(s.relays, request.RelayID)
	relay.timer.Stop()
	if notifyBeamCore && s.relaySink != nil {
		s.relaySink(StorageProbeRelayEvent{Type: contracts.StorageProbeRelayCloseType, RelayID: request.RelayID, Reason: request.Reason})
	}
	s.relayMu.Unlock()
	_, _ = relay.session.send(TypeStorageProbeRelayClose, "", request)
}

// handleStorageProbeRelay publishes a Worker relay frame when the relay is
// active on that Worker's session; frames for other relays are dropped.
func (s *Server) handleStorageProbeRelay(session *Session, envelope Envelope) error {
	var event StorageProbeRelayEvent
	switch envelope.Type {
	case TypeStorageProbeRelayOpened:
		opened, err := decodeStrictPayload[contracts.StorageProbeRelayOpened](envelope)
		if err != nil {
			return err
		}
		event = StorageProbeRelayEvent{Type: contracts.StorageProbeRelayOpenedType, RelayID: opened.RelayID}
	case TypeStorageProbeRelayData:
		frame, err := decodeStrictPayload[contracts.StorageProbeRelayData](envelope)
		if err != nil {
			return err
		}
		event = StorageProbeRelayEvent{Type: contracts.StorageProbeRelayDataType, RelayID: frame.RelayID, Seq: frame.Seq, Data: frame.Data}
	default:
		request, err := decodeStrictPayload[contracts.StorageProbeRelayClose](envelope)
		if err != nil {
			return err
		}
		reason := request.Reason
		if !contracts.ValidStorageProbeRelayReason(reason) {
			reason = contracts.StorageProbeRelayProtocolError
		}
		event = StorageProbeRelayEvent{Type: contracts.StorageProbeRelayCloseType, RelayID: request.RelayID, Reason: reason}
	}
	// Publishing under the lock keeps every frame of a relay ahead of its close.
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	relay := s.relays[event.RelayID]
	if relay == nil || relay.session != session {
		return nil
	}
	if event.Type == contracts.StorageProbeRelayCloseType {
		delete(s.relays, event.RelayID)
		relay.timer.Stop()
	}
	if s.relaySink != nil {
		s.relaySink(event)
	}
	return nil
}

// dropStorageProbeRelays reports worker_unavailable for the relays of a
// Worker session that has ended.
func (s *Server) dropStorageProbeRelays(session *Session) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	for relayID, relay := range s.relays {
		if relay.session != session {
			continue
		}
		delete(s.relays, relayID)
		relay.timer.Stop()
		if s.relaySink != nil {
			s.relaySink(StorageProbeRelayEvent{Type: contracts.StorageProbeRelayCloseType, RelayID: relayID,
				Reason: contracts.StorageProbeRelayWorkerUnavailable})
		}
	}
}

// expireStorageProbeRelay forgets a relay past its lifetime, closing it on both sides.
func (s *Server) expireStorageProbeRelay(relayID string, relay *serverRelay) {
	s.relayMu.Lock()
	if s.relays[relayID] != relay {
		s.relayMu.Unlock()
		return
	}
	delete(s.relays, relayID)
	if s.relaySink != nil {
		s.relaySink(StorageProbeRelayEvent{Type: contracts.StorageProbeRelayCloseType, RelayID: relayID,
			Reason: contracts.StorageProbeRelayTimeout})
	}
	s.relayMu.Unlock()
	_, _ = relay.session.send(TypeStorageProbeRelayClose, "", contracts.StorageProbeRelayClose{
		RelayID: relayID, Reason: contracts.StorageProbeRelayTimeout,
	})
}

// forgetStorageProbeRelay removes a relay without notifying anyone and reports
// whether it was still active.
func (s *Server) forgetStorageProbeRelay(relayID string, relay *serverRelay) bool {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	if s.relays[relayID] != relay {
		return false
	}
	delete(s.relays, relayID)
	relay.timer.Stop()
	return true
}
