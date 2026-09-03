package circuit

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

const maxHandshakeBytes = 64 << 10

type OpenRequest struct {
	ChannelID string            `json:"channel_id"`
	Protocol  string            `json:"protocol"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type hello struct {
	CircuitID   string            `json:"circuit_id"`
	PlanVersion uint64            `json:"plan_version"`
	FromNodeID  string            `json:"from_node_id"`
	ToNodeID    string            `json:"to_node_id"`
	ChannelID   string            `json:"channel_id"`
	Protocol    string            `json:"protocol"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Nonce       string            `json:"nonce"`
	Proof       string            `json:"proof"`
}

type helloAck struct {
	CircuitID   string `json:"circuit_id"`
	PlanVersion uint64 `json:"plan_version"`
	FromNodeID  string `json:"from_node_id"`
	ToNodeID    string `json:"to_node_id"`
	ChannelID   string `json:"channel_id"`
	ClientNonce string `json:"client_nonce"`
	ServerNonce string `json:"server_nonce"`
	Proof       string `json:"proof"`
}

type Conn struct {
	net.Conn
	CircuitID  string
	WorkloadID string
	Peer       Peer
	ChannelID  string
	Protocol   string
	Metadata   map[string]string
	once       sync.Once
	onClose    func()
}

func (c *Conn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
	})
	return err
}

type Service struct {
	identity   domain.Identity
	privateKey ed25519.PrivateKey
	table      *Table
	config     *tls.Config

	mu         sync.Mutex
	listener   net.Listener
	active     map[string]map[*Conn]struct{}
	acceptMu   sync.Mutex
	backlog    []*Conn
	waiters    map[uint64]*acceptWaiter
	nextWaiter uint64
	errors     chan error
	closed     chan struct{}
	closeOnce  sync.Once
	sessionMu  sync.Mutex
	sessions   map[string]*muxSession
}

type acceptWaiter struct {
	match func(*Conn) bool
	ready chan *Conn
}

func NewService(identity domain.Identity, privateKey ed25519.PrivateKey, table *Table) (*Service, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if len(privateKey) != ed25519.PrivateKeySize || table == nil {
		return nil, errors.New("circuit node key and circuit table are required")
	}
	if identity.NodeID != NodeID(privateKey.Public().(ed25519.PublicKey)) {
		return nil, errors.New("circuit node_id does not match its Ed25519 key")
	}
	certificate := func() (*tls.Certificate, error) {
		generated, err := nodeCertificate(identity.NodeID, privateKey)
		return &generated, err
	}
	return &Service{
		identity: identity, privateKey: privateKey, table: table,
		config: &tls.Config{
			MinVersion: tls.VersionTLS13,
			NextProtos: []string{MuxALPN, ALPN}, ClientAuth: tls.RequireAnyClientCert,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return certificate()
			},
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				return certificate()
			},
		},
		active: make(map[string]map[*Conn]struct{}), sessions: make(map[string]*muxSession), waiters: make(map[uint64]*acceptWaiter),
		errors: make(chan error, 16), closed: make(chan struct{}),
	}, nil
}

func (s *Service) Listen(ctx context.Context, address string) (string, error) {
	baseListener, err := net.Listen("tcp", address)
	if err != nil {
		return "", err
	}
	tlsListener := tls.NewListener(baseListener, s.config.Clone())
	s.mu.Lock()
	if s.listener != nil {
		s.mu.Unlock()
		tlsListener.Close()
		return "", errors.New("circuit service is already listening")
	}
	s.listener = tlsListener
	s.mu.Unlock()
	go s.acceptLoop(ctx, tlsListener)
	go s.reconcile(ctx)
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	return tlsListener.Addr().String(), nil
}

func (s *Service) Accept(ctx context.Context) (*Conn, error) {
	return s.AcceptFor(ctx, "", "")
}

func (s *Service) AcceptFor(ctx context.Context, protocol, workloadID string) (*Conn, error) {
	match := func(connection *Conn) bool {
		return (protocol == "" || connection.Protocol == protocol) &&
			(workloadID == "" || connection.WorkloadID == workloadID)
	}
	s.acceptMu.Lock()
	for index, connection := range s.backlog {
		if match(connection) {
			s.backlog = append(s.backlog[:index], s.backlog[index+1:]...)
			s.acceptMu.Unlock()
			return connection, nil
		}
	}
	s.nextWaiter++
	waiterID := s.nextWaiter
	waiter := &acceptWaiter{match: match, ready: make(chan *Conn, 1)}
	s.waiters[waiterID] = waiter
	s.acceptMu.Unlock()
	select {
	case connection := <-waiter.ready:
		return connection, nil
	case err := <-s.errors:
		s.removeWaiter(waiterID, waiter)
		return nil, err
	case <-s.closed:
		s.removeWaiter(waiterID, waiter)
		return nil, net.ErrClosed
	case <-ctx.Done():
		s.removeWaiter(waiterID, waiter)
		return nil, ctx.Err()
	}
}

func (s *Service) removeWaiter(id uint64, waiter *acceptWaiter) {
	s.acceptMu.Lock()
	if s.waiters[id] == waiter {
		delete(s.waiters, id)
		s.acceptMu.Unlock()
		return
	}
	s.acceptMu.Unlock()
	select {
	case connection := <-waiter.ready:
		_ = connection.Close()
	default:
	}
}

func (s *Service) Dial(ctx context.Context, circuitID, peerNodeID string, request OpenRequest) (*Conn, error) {
	return s.dialMux(ctx, circuitID, peerNodeID, request)
}

func (s *Service) DialWithFailover(ctx context.Context, circuitID, primaryNodeID string, standbyNodeIDs []string, request OpenRequest) (*Conn, error) {
	nodes := append([]string{primaryNodeID}, standbyNodeIDs...)
	var failures []error
	seen := make(map[string]struct{}, len(nodes))
	for _, nodeID := range nodes {
		if nodeID == "" {
			continue
		}
		if _, exists := seen[nodeID]; exists {
			continue
		}
		seen[nodeID] = struct{}{}
		connection, err := s.Dial(ctx, circuitID, nodeID, request)
		if err == nil {
			return connection, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", nodeID, err))
		if ctx.Err() != nil {
			break
		}
	}
	if len(failures) == 0 {
		return nil, errors.New("circuit failover requires at least one peer")
	}
	return nil, fmt.Errorf("all circuit routes failed: %w", errors.Join(failures...))
}

func (s *Service) Upsert(plan Plan) error {
	previous, existed := s.table.Plan(plan.CircuitID)
	if err := s.table.Upsert(plan); err != nil {
		return err
	}
	if existed && plan.PlanVersion > previous.PlanVersion {
		s.closeCircuit(plan.CircuitID)
	}
	return nil
}

func (s *Service) Revoke(revocation Revocation) error {
	if err := s.table.Revoke(revocation); err != nil {
		return err
	}
	s.closeCircuit(revocation.CircuitID)
	return nil
}

func (s *Service) Plans() []Plan { return s.table.Plans() }

func (s *Service) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		close(s.closed)
		s.mu.Lock()
		if s.listener != nil {
			closeErr = s.listener.Close()
		}
		for circuitID := range s.active {
			s.closeCircuitLocked(circuitID)
		}
		s.closeAllMuxSessions()
		s.mu.Unlock()
		s.acceptMu.Lock()
		for _, connection := range s.backlog {
			_ = connection.Close()
		}
		s.backlog = nil
		s.acceptMu.Unlock()
	})
	return closeErr
}

func (s *Service) acceptLoop(ctx context.Context, listener net.Listener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				s.reportError(err)
			}
			return
		}
		go s.acceptConnection(ctx, connection.(*tls.Conn))
	}
}

func (s *Service) acceptConnection(ctx context.Context, connection *tls.Conn) {
	handshakeContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := connection.HandshakeContext(handshakeContext); err != nil {
		connection.Close()
		return
	}
	if connection.ConnectionState().NegotiatedProtocol == MuxALPN {
		s.acceptMuxConnection(ctx, connection)
		return
	}
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	var helloMessage hello
	if err := readHandshake(connection, &helloMessage); err != nil {
		connection.Close()
		return
	}
	if helloMessage.ToNodeID != s.identity.NodeID || validateOpenRequest(OpenRequest{
		ChannelID: helloMessage.ChannelID, Protocol: helloMessage.Protocol, Metadata: helloMessage.Metadata,
	}) != nil {
		connection.Close()
		return
	}
	if err := verifyPeerState(connection.ConnectionState(), helloMessage.FromNodeID, ALPN); err != nil {
		connection.Close()
		return
	}
	plan, peer, err := s.table.Authorize(helloMessage.CircuitID, s.identity.NodeID, helloMessage.FromNodeID, time.Now())
	if err != nil || plan.PlanVersion != helloMessage.PlanVersion || !verifyHelloProof(plan.Token, helloMessage) {
		connection.Close()
		return
	}
	serverNonce, err := randomToken(24)
	if err != nil {
		connection.Close()
		return
	}
	acknowledgement := helloAck{
		CircuitID: plan.CircuitID, PlanVersion: plan.PlanVersion,
		FromNodeID: s.identity.NodeID, ToNodeID: peer.NodeID,
		ChannelID: helloMessage.ChannelID, ClientNonce: helloMessage.Nonce, ServerNonce: serverNonce,
	}
	acknowledgement.Proof = ackProof(plan.Token, acknowledgement)
	if err := writeHandshake(connection, acknowledgement); err != nil {
		connection.Close()
		return
	}
	_ = connection.SetDeadline(time.Time{})
	accepted, err := s.track(connection, plan, peer, OpenRequest{
		ChannelID: helloMessage.ChannelID, Protocol: helloMessage.Protocol, Metadata: helloMessage.Metadata,
	})
	if err != nil {
		connection.Close()
		return
	}
	if ctx.Err() != nil || !s.deliverAccepted(accepted) {
		accepted.Close()
	}
}

func (s *Service) deliverAccepted(connection *Conn) bool {
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	for id, waiter := range s.waiters {
		if waiter.match(connection) {
			delete(s.waiters, id)
			waiter.ready <- connection
			return true
		}
	}
	if len(s.backlog) >= 128 {
		s.reportError(errors.New("circuit accept queue is full"))
		return false
	}
	s.backlog = append(s.backlog, connection)
	return true
}

func (s *Service) track(connection net.Conn, plan Plan, peer Peer, request OpenRequest) (*Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
		return nil, net.ErrClosed
	default:
	}
	current, currentPeer, err := s.table.Authorize(plan.CircuitID, s.identity.NodeID, peer.NodeID, time.Now())
	if err != nil || current.PlanVersion != plan.PlanVersion || currentPeer.WorkerID != peer.WorkerID {
		return nil, errors.New("circuit circuit changed during connection establishment")
	}
	result := &Conn{
		Conn: connection, CircuitID: current.CircuitID, WorkloadID: current.WorkloadID,
		Peer: currentPeer, ChannelID: request.ChannelID, Protocol: request.Protocol,
		Metadata: request.Metadata,
	}
	result.onClose = func() {
		s.mu.Lock()
		if connections := s.active[current.CircuitID]; connections != nil {
			delete(connections, result)
			if len(connections) == 0 {
				delete(s.active, current.CircuitID)
			}
		}
		s.mu.Unlock()
	}
	if s.active[current.CircuitID] == nil {
		s.active[current.CircuitID] = make(map[*Conn]struct{})
	}
	s.active[current.CircuitID][result] = struct{}{}
	return result, nil
}

func (s *Service) reconcile(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for _, plan := range s.table.Plans() {
				if !now.Before(plan.ExpiresAt) {
					s.closeCircuit(plan.CircuitID)
				}
			}
		}
	}
}

func (s *Service) closeCircuit(circuitID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCircuitLocked(circuitID)
}

func (s *Service) closeCircuitLocked(circuitID string) {
	connections := s.active[circuitID]
	delete(s.active, circuitID)
	for connection := range connections {
		_ = connection.Conn.Close()
	}
	s.closeMuxCircuit(circuitID)
}

func (s *Service) reportError(err error) {
	select {
	case s.errors <- err:
	default:
	}
}

func validateOpenRequest(request OpenRequest) error {
	if request.ChannelID == "" || len(request.ChannelID) > 256 || request.Protocol == "" || len(request.Protocol) > 128 {
		return errors.New("circuit channel_id and bounded protocol are required")
	}
	if len(request.Metadata) > 64 {
		return errors.New("circuit metadata is too large")
	}
	for key, value := range request.Metadata {
		if len(key) > 256 || len(value) > 4096 {
			return errors.New("circuit metadata entry is too large")
		}
	}
	return nil
}

var _ Controller = (*Service)(nil)
