package wcp

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/evidence"
	orchestratorcontrol "github.com/Beam-Network/beam/internal/orchestrator/control"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

func (s *Server) handle(parent context.Context, connection net.Conn) {
	defer connection.Close()
	tlsConnection, ok := connection.(*tls.Conn)
	if !ok {
		return
	}
	handshakeContext, cancelHandshake := context.WithTimeout(parent, 10*time.Second)
	defer cancelHandshake()
	if err := tlsConnection.HandshakeContext(handshakeContext); err != nil {
		return
	}
	state := tlsConnection.ConnectionState()
	if state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != ALPN {
		return
	}
	framed := newFramedConn(connection)
	challenge := NewChallenge(s.orchestratorID)
	if _, err := framed.write(TypeChallenge, "", challenge, 0); err != nil {
		return
	}
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	envelope, err := framed.read()
	if err != nil || envelope.Type != TypeHello {
		return
	}
	hello, err := decodePayload[Hello](envelope)
	if err != nil || VerifyHello(hello, challenge) != nil {
		return
	}
	membership, exists := s.registry.Membership(hello.Identity.WorkerID)
	if !exists || membership.Status != "active" || membership.NodeID != hello.Identity.NodeID {
		return
	}
	if len(membership.Delegation) > 0 &&
		(len(membership.Delegation) != len(hello.OrchestratorDelegation) || subtle.ConstantTimeCompare(membership.Delegation, hello.OrchestratorDelegation) != 1) {
		return
	}
	welcome, err := s.control.Accept(orchestratorcontrol.WorkerHello{
		Identity: hello.Identity, OrchestratorDelegation: hello.OrchestratorDelegation,
		SoftwareVersion: hello.SoftwareVersion, ProtocolVersion: orchestratorcontrol.ProtocolVersion,
		Capabilities: hello.Capabilities, CapabilityManifest: hello.CapabilityManifest,
		TotalResources:  hello.TotalResources,
		LastPlanVersion: hello.LastPlanVersion, LastEventCursor: hello.LastEventCursor,
	})
	if err != nil {
		return
	}
	session := &Session{
		workerID: hello.Identity.WorkerID, identity: hello.Identity, connection: connection,
		framed: framed, server: s, done: make(chan struct{}),
	}
	session.lastRead.Store(envelope.Sequence)
	s.mu.Lock()
	previous := s.sessions[session.workerID]
	s.sessions[session.workerID] = session
	s.mu.Unlock()
	if previous != nil {
		previous.close()
	}
	defer func() {
		s.mu.Lock()
		if s.sessions[session.workerID] == session {
			delete(s.sessions, session.workerID)
		}
		s.mu.Unlock()
		session.close()
	}()
	if _, err := session.send(TypeWelcome, envelope.MessageID, Welcome{
		OrchestratorID: welcome.OrchestratorID, SessionID: welcome.SessionID,
		HeartbeatInterval: welcome.HeartbeatInterval, ConfigEpoch: welcome.ConfigEpoch,
		PlanVersion: welcome.CurrentPlanVersion,
	}); err != nil {
		return
	}
	if err := s.replayCircuits(session); err != nil {
		return
	}
	_ = connection.SetReadDeadline(time.Now().Add(3 * welcome.HeartbeatInterval))
	for {
		envelope, err := framed.read()
		if err != nil {
			return
		}
		last := session.lastRead.Load()
		if envelope.Sequence <= last {
			return
		}
		session.lastRead.Store(envelope.Sequence)
		_ = connection.SetReadDeadline(time.Now().Add(3 * welcome.HeartbeatInterval))
		if err := s.handleEnvelope(session, envelope); err != nil {
			_, _ = session.send(TypeError, envelope.MessageID, ErrorMessage{Code: "invalid_message", Message: err.Error()})
		}
	}
}

func (s *Server) handleEnvelope(session *Session, envelope Envelope) error {
	switch envelope.Type {
	case TypeHeartbeat:
		return s.handleHeartbeat(session, envelope)
	case TypeDecision:
		return s.handleDecision(envelope)
	case TypeResult:
		return s.handleResult(session, envelope)
	case TypeProgress:
		return s.handleProgress(session, envelope)
	case TypeCheckpoint:
		return s.handleCheckpoint(session, envelope)
	case TypeReceipt:
		return s.handleReceipt(session, envelope)
	case TypeError:
		_, err := decodePayload[ErrorMessage](envelope)
		return err
	default:
		return fmt.Errorf("unsupported Worker message type %q", envelope.Type)
	}
}

func (s *Server) handleHeartbeat(session *Session, envelope Envelope) error {
	heartbeat, err := decodePayload[Heartbeat](envelope)
	if err != nil {
		return err
	}
	if heartbeat.Identity.WorkerID != session.workerID || heartbeat.Identity.NodeID != session.identity.NodeID {
		return errors.New("heartbeat identity changed within session")
	}
	return s.control.Heartbeat(heartbeat.Identity, heartbeat.Status, heartbeat.Region, heartbeat.CircuitEndpoint,
		heartbeat.Capabilities, heartbeat.CapabilityManifest, heartbeat.Total, heartbeat.Available, heartbeat.PlanVersion)
}

func (s *Server) handleDecision(envelope Envelope) error {
	decision, err := decodePayload[runtime.Decision](envelope)
	if err != nil {
		return err
	}
	s.mu.RLock()
	pending := s.pending[envelope.ReplyTo]
	s.mu.RUnlock()
	if pending != nil {
		select {
		case pending <- decision:
		default:
		}
	}
	return nil
}

func (s *Server) handleResult(session *Session, envelope Envelope) error {
	result, err := decodePayload[domain.Result](envelope)
	if err != nil {
		return err
	}
	eventID := fmt.Sprintf("result:%s:%s:%s:%s:%d", session.workerID, result.WorkloadID,
		result.AttemptID, result.State, result.CompletedAt.UnixNano())
	appended, err := s.journal.Append(JournalEvent{
		EventID: eventID, WorkerID: session.workerID, Type: TypeResult,
		WorkloadID: result.WorkloadID, AttemptID: result.AttemptID, Result: &result,
	})
	if err != nil || !appended {
		return err
	}
	select {
	case s.results <- ResultEvent{WorkerID: session.workerID, Result: result}:
		return nil
	default:
		return errors.New("Orchestrator result queue is full")
	}
}

func (s *Server) handleProgress(session *Session, envelope Envelope) error {
	progress, err := decodePayload[domain.Progress](envelope)
	if err != nil {
		return err
	}
	// Progress is advisory and continuously refreshed. Persisting every sample
	// forced an fsync on the WCP read loop and let historical telemetry delay a
	// new workload's runtime endpoint beyond the publisher handshake deadline.
	// Results, receipts and checkpoints remain journaled; current progress is
	// also retained in the durable workload stores downstream.
	select {
	case s.progress <- ProgressEvent{WorkerID: session.workerID, Progress: progress}:
		return nil
	default:
		return errors.New("Orchestrator progress queue is full")
	}
}

func (s *Server) handleCheckpoint(session *Session, envelope Envelope) error {
	checkpoint, err := decodePayload[domain.Checkpoint](envelope)
	if err != nil {
		return err
	}
	if checkpoint.WorkloadID == "" || checkpoint.AttemptID == "" || checkpoint.Kind == "" || checkpoint.Sequence == 0 || checkpoint.Schema == "" {
		return errors.New("invalid workload checkpoint")
	}
	eventID := fmt.Sprintf("checkpoint:%s:%s:%s:%d", session.workerID, checkpoint.WorkloadID, checkpoint.AttemptID, checkpoint.Sequence)
	_, err = s.journal.Append(JournalEvent{
		EventID: eventID, WorkerID: session.workerID, Type: TypeCheckpoint,
		WorkloadID: checkpoint.WorkloadID, AttemptID: checkpoint.AttemptID, Checkpoint: &checkpoint,
	})
	if err != nil {
		return err
	}
	select {
	case s.checkpoints <- CheckpointEvent{WorkerID: session.workerID, Checkpoint: checkpoint}:
		return nil
	default:
		return errors.New("Orchestrator checkpoint queue is full")
	}
}

func (s *Server) handleReceipt(session *Session, envelope Envelope) error {
	receipt, err := decodePayload[evidence.Receipt](envelope)
	if err != nil {
		return err
	}
	if err := receipt.Verify(); err != nil {
		return err
	}
	// instance_id identifies the process that produced the evidence and may
	// legitimately differ after a restart. The persistent node key is the
	// authority used to verify replayed receipts.
	if receipt.OrchestratorID != s.orchestratorID || receipt.WorkerID != session.workerID || receipt.NodeID != session.identity.NodeID {
		return errors.New("receipt identity does not match its authenticated WCP session")
	}
	appended, err := s.journal.Append(JournalEvent{
		EventID: "receipt:" + receipt.ReceiptID, WorkerID: session.workerID, Type: TypeReceipt,
		WorkloadID: receipt.WorkloadID, AttemptID: receipt.AttemptID, Receipt: &receipt,
	})
	if err != nil {
		return err
	}
	if _, err := session.send(TypeReceiptAck, envelope.MessageID, ReceiptAck{
		ReceiptID: receipt.ReceiptID, AcknowledgedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	if appended {
		select {
		case s.receipts <- ReceiptEvent{WorkerID: session.workerID, Receipt: receipt}:
		default:
		}
	}
	return nil
}

type Session struct {
	workerID   string
	identity   domain.Identity
	connection net.Conn
	framed     *framedConn
	server     *Server
	done       chan struct{}
	closeOnce  sync.Once
	lastRead   atomic.Uint64
}

func (s *Session) send(messageType, replyTo string, value any) (Envelope, error) {
	select {
	case <-s.done:
		return Envelope{}, io.ErrClosedPipe
	default:
	}
	return s.framed.write(messageType, replyTo, value, s.lastRead.Load())
}

func (s *Session) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.connection.Close()
	})
}
