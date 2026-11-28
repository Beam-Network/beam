package wcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const TypeMediaControl = "media.control"

type MediaControl struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

var (
	MediaControlToWorker       = []string{mediacontracts.TopologyUpdateType, mediacontracts.CertResultType, mediacontracts.CapacityReportAckType}
	MediaControlToOrchestrator = []string{mediacontracts.SessionEventType, mediacontracts.UsageReportType, mediacontracts.CertRequestType}
)

func (m MediaControl) Validate(allowed []string) error {
	if !slices.Contains(allowed, m.Type) {
		return fmt.Errorf("unsupported media control type %q", m.Type)
	}
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(m.Payload, &header); err != nil || header.Type != m.Type {
		return errors.New("media control payload type does not match")
	}
	return nil
}

type MediaBinder interface {
	Capacity() *mediacontracts.WorkerCapacity
	Outbound() <-chan MediaControl
	Deliver(MediaControl) error
}

func validMediaCapacity(workerID string, capacity *mediacontracts.WorkerCapacity) bool {
	return capacity != nil && capacity.WorkerID == workerID && contracts.ValidateWorkerMediaCapacity(*capacity) == nil
}

type MediaControlEvent struct {
	WorkerID string
	Message  MediaControl
}

type serverMedia struct {
	mu   sync.RWMutex
	sink func(MediaControlEvent)
}

func (s *Server) SetMediaControlSink(sink func(MediaControlEvent)) {
	s.media.mu.Lock()
	s.media.sink = sink
	s.media.mu.Unlock()
}

func (s *Server) SendMediaControl(workerID string, message MediaControl) error {
	if err := message.Validate(MediaControlToWorker); err != nil {
		return err
	}
	session, err := s.session(workerID)
	if err != nil {
		return err
	}
	if !session.media.Load() {
		return fmt.Errorf("Worker %s holds no media role", workerID)
	}
	_, err = session.send(TypeMediaControl, "", message)
	return err
}

func (s *Server) MediaCapable(workerID string) bool {
	session, err := s.session(workerID)
	return err == nil && session.media.Load()
}

func (s *Server) handleMediaControl(session *Session, envelope Envelope) error {
	message, err := decodePayload[MediaControl](envelope)
	if err != nil {
		return err
	}
	if err := message.Validate(MediaControlToOrchestrator); err != nil {
		return err
	}
	s.media.mu.RLock()
	sink := s.media.sink
	s.media.mu.RUnlock()
	if sink == nil {
		return errors.New("Orchestrator media control is not connected")
	}
	sink(MediaControlEvent{WorkerID: session.workerID, Message: message})
	return nil
}
