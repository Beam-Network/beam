package circuit

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

const muxProtocol = "beam-mux/1"

type streamAck struct {
	Accepted bool   `json:"accepted"`
	Error    string `json:"error,omitempty"`
}

type muxSession struct {
	key       string
	circuitID string
	peerNode  string
	initiator string
	plan      Plan
	peer      Peer
	session   *yamux.Session
}

func (s *Service) dialMux(ctx context.Context, circuitID, peerNodeID string, request OpenRequest) (*Conn, error) {
	plan, peer, err := s.table.Authorize(circuitID, s.identity.NodeID, peerNodeID, time.Now())
	if err != nil {
		return nil, err
	}
	if request.ChannelID == "" {
		request.ChannelID, err = randomToken(16)
		if err != nil {
			return nil, err
		}
	}
	if err := validateOpenRequest(request); err != nil {
		return nil, err
	}
	physical, err := s.sessionFor(ctx, plan, peer)
	if err != nil {
		return nil, err
	}
	stream, err := physical.session.OpenStream()
	if err != nil {
		s.dropMuxSession(physical)
		physical, err = s.sessionFor(ctx, plan, peer)
		if err != nil {
			return nil, err
		}
		stream, err = physical.session.OpenStream()
		if err != nil {
			return nil, fmt.Errorf("open multiplexed circuit stream: %w", err)
		}
	}
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline || time.Until(deadline) > 10*time.Second {
		deadline = time.Now().Add(10 * time.Second)
	}
	_ = stream.SetDeadline(deadline)
	if err := writeHandshake(stream, request); err != nil {
		stream.Close()
		return nil, err
	}
	var acknowledgement streamAck
	if err := readHandshake(stream, &acknowledgement); err != nil {
		stream.Close()
		return nil, err
	}
	if !acknowledgement.Accepted {
		stream.Close()
		return nil, fmt.Errorf("multiplexed circuit stream rejected: %s", acknowledgement.Error)
	}
	_ = stream.SetDeadline(time.Time{})
	tracked, err := s.track(stream, plan, peer, request)
	if err != nil {
		stream.Close()
		return nil, err
	}
	return tracked, nil
}

func (s *Service) sessionFor(ctx context.Context, plan Plan, peer Peer) (*muxSession, error) {
	key := muxSessionKey(plan.CircuitID, peer.NodeID)
	s.sessionMu.Lock()
	if current := s.sessions[key]; current != nil && !current.session.IsClosed() && current.plan.PlanVersion == plan.PlanVersion {
		s.sessionMu.Unlock()
		return current, nil
	}
	connection, err := s.dialMuxConnection(ctx, plan, peer)
	if err != nil {
		s.sessionMu.Unlock()
		return nil, err
	}
	configuration := yamux.DefaultConfig()
	configuration.LogOutput = io.Discard
	configuration.AcceptBacklog = 256
	configuration.StreamOpenTimeout = 10 * time.Second
	multiplexer, err := yamux.Client(connection, configuration)
	if err != nil {
		connection.Close()
		s.sessionMu.Unlock()
		return nil, err
	}
	candidate := &muxSession{key: key, circuitID: plan.CircuitID, peerNode: peer.NodeID,
		initiator: s.identity.NodeID, plan: plan, peer: peer, session: multiplexer}
	selected, installed := s.installMuxSessionLocked(candidate)
	s.sessionMu.Unlock()
	if installed {
		go s.acceptMuxStreams(selected)
	} else {
		_ = candidate.session.Close()
	}
	return selected, nil
}

func (s *Service) dialMuxConnection(ctx context.Context, plan Plan, peer Peer) (*tls.Conn, error) {
	clientConfig := s.config.Clone()
	clientConfig.ClientAuth = tls.NoClientCert
	clientConfig.NextProtos = []string{MuxALPN}
	clientConfig.InsecureSkipVerify = true // verified against the circuit-authorized node_id below.
	clientConfig.VerifyConnection = func(state tls.ConnectionState) error {
		return verifyPeerState(state, peer.NodeID, MuxALPN)
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: clientConfig}
	endpoints := append([]string{peer.Endpoint}, peer.StandbyEndpoints...)
	var failures []error
	for _, endpoint := range endpoints {
		raw, err := dialer.DialContext(ctx, "tcp", endpoint)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", endpoint, err))
			continue
		}
		connection := raw.(*tls.Conn)
		if err := s.authenticateMuxClient(connection, plan, peer); err != nil {
			connection.Close()
			failures = append(failures, fmt.Errorf("%s: %w", endpoint, err))
			continue
		}
		return connection, nil
	}
	return nil, fmt.Errorf("all endpoints for %s failed: %w", peer.NodeID, errors.Join(failures...))
}

func (s *Service) authenticateMuxClient(connection *tls.Conn, plan Plan, peer Peer) error {
	nonce, err := randomToken(24)
	if err != nil {
		return err
	}
	channelID, err := randomToken(16)
	if err != nil {
		return err
	}
	message := hello{CircuitID: plan.CircuitID, PlanVersion: plan.PlanVersion,
		FromNodeID: s.identity.NodeID, ToNodeID: peer.NodeID, ChannelID: channelID,
		Protocol: muxProtocol, Nonce: nonce}
	message.Proof = helloProof(plan.Token, message)
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	if err := writeHandshake(connection, message); err != nil {
		return err
	}
	var acknowledgement helloAck
	if err := readHandshake(connection, &acknowledgement); err != nil {
		return err
	}
	if acknowledgement.CircuitID != plan.CircuitID || acknowledgement.PlanVersion != plan.PlanVersion ||
		acknowledgement.FromNodeID != peer.NodeID || acknowledgement.ToNodeID != s.identity.NodeID ||
		acknowledgement.ChannelID != channelID || acknowledgement.ClientNonce != nonce ||
		!verifyAckProof(plan.Token, acknowledgement) {
		return errors.New("invalid multiplexed circuit acknowledgement")
	}
	return connection.SetDeadline(time.Time{})
}

func (s *Service) acceptMuxConnection(ctx context.Context, connection *tls.Conn) {
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	var message hello
	if err := readHandshake(connection, &message); err != nil || message.ToNodeID != s.identity.NodeID || message.Protocol != muxProtocol {
		connection.Close()
		return
	}
	if err := verifyPeerState(connection.ConnectionState(), message.FromNodeID, MuxALPN); err != nil {
		connection.Close()
		return
	}
	plan, peer, err := s.table.Authorize(message.CircuitID, s.identity.NodeID, message.FromNodeID, time.Now())
	if err != nil || plan.PlanVersion != message.PlanVersion || !verifyHelloProof(plan.Token, message) {
		connection.Close()
		return
	}
	serverNonce, err := randomToken(24)
	if err != nil {
		connection.Close()
		return
	}
	acknowledgement := helloAck{CircuitID: plan.CircuitID, PlanVersion: plan.PlanVersion,
		FromNodeID: s.identity.NodeID, ToNodeID: peer.NodeID, ChannelID: message.ChannelID,
		ClientNonce: message.Nonce, ServerNonce: serverNonce}
	acknowledgement.Proof = ackProof(plan.Token, acknowledgement)
	if err := writeHandshake(connection, acknowledgement); err != nil {
		connection.Close()
		return
	}
	_ = connection.SetDeadline(time.Time{})
	configuration := yamux.DefaultConfig()
	configuration.LogOutput = io.Discard
	configuration.AcceptBacklog = 256
	configuration.StreamOpenTimeout = 10 * time.Second
	multiplexer, err := yamux.Server(connection, configuration)
	if err != nil {
		connection.Close()
		return
	}
	candidate := &muxSession{key: muxSessionKey(plan.CircuitID, peer.NodeID), circuitID: plan.CircuitID,
		peerNode: peer.NodeID, initiator: peer.NodeID, plan: plan, peer: peer, session: multiplexer}
	s.sessionMu.Lock()
	selected, installed := s.installMuxSessionLocked(candidate)
	s.sessionMu.Unlock()
	if !installed {
		_ = candidate.session.Close()
		return
	}
	go s.acceptMuxStreams(selected)
	select {
	case <-selected.session.CloseChan():
	case <-ctx.Done():
		_ = selected.session.Close()
	}
}

// installMuxSessionLocked converges simultaneous connections on the session
// initiated by the lexicographically lowest node_id.
func (s *Service) installMuxSessionLocked(candidate *muxSession) (*muxSession, bool) {
	current := s.sessions[candidate.key]
	if current != nil && !current.session.IsClosed() && current.plan.PlanVersion == candidate.plan.PlanVersion {
		if current.initiator < candidate.initiator || current.initiator == candidate.initiator {
			return current, false
		}
		_ = current.session.Close()
	} else if current != nil {
		_ = current.session.Close()
	}
	s.sessions[candidate.key] = candidate
	return candidate, true
}

func (s *Service) acceptMuxStreams(physical *muxSession) {
	defer s.dropMuxSession(physical)
	for {
		stream, err := physical.session.AcceptStream()
		if err != nil {
			return
		}
		go s.acceptMuxStream(physical, stream)
	}
}

func (s *Service) acceptMuxStream(physical *muxSession, stream net.Conn) {
	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))
	var request OpenRequest
	if err := readHandshake(stream, &request); err != nil || validateOpenRequest(request) != nil {
		_ = writeHandshake(stream, streamAck{Error: "invalid open request"})
		stream.Close()
		return
	}
	plan, peer, err := s.table.Authorize(physical.circuitID, s.identity.NodeID, physical.peerNode, time.Now())
	if err != nil || plan.PlanVersion != physical.plan.PlanVersion {
		_ = writeHandshake(stream, streamAck{Error: "circuit authorization expired"})
		stream.Close()
		return
	}
	accepted, err := s.track(stream, plan, peer, request)
	if err != nil {
		_ = writeHandshake(stream, streamAck{Error: err.Error()})
		stream.Close()
		return
	}
	if err := writeHandshake(stream, streamAck{Accepted: true}); err != nil {
		accepted.Close()
		return
	}
	_ = stream.SetDeadline(time.Time{})
	if !s.deliverAccepted(accepted) {
		accepted.Close()
	}
}

func muxSessionKey(circuitID, peerNodeID string) string { return circuitID + "\x00" + peerNodeID }

func (s *Service) dropMuxSession(session *muxSession) {
	s.sessionMu.Lock()
	if s.sessions[session.key] == session {
		delete(s.sessions, session.key)
	}
	s.sessionMu.Unlock()
	_ = session.session.Close()
}

func (s *Service) closeMuxCircuit(circuitID string) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	prefix := circuitID + "\x00"
	for key, session := range s.sessions {
		if strings.HasPrefix(key, prefix) {
			delete(s.sessions, key)
			_ = session.session.Close()
		}
	}
}

func (s *Service) closeAllMuxSessions() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	for key, session := range s.sessions {
		delete(s.sessions, key)
		_ = session.session.Close()
	}
}
