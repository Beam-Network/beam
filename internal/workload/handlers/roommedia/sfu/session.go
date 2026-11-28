package sfu

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/ratelimit"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

type Options struct {
	SessionID        string
	NodeID           string
	Role             string
	Factory          *APIFactory
	ICE              ICEConfig
	Usage            *usage.Reporter
	SessionEvent     func(body any)
	Admission        func(kind string) error
	ExternalSpeakers bool
	PublisherGrace   time.Duration
	Logf             func(format string, args ...any)
}

type ICEConfig struct {
	Servers        []string
	TURNSecret     string
	TURNTTL        time.Duration
	UsernamePrefix string
}

type Session struct {
	opts Options

	mu                sync.RWMutex
	view              *mediacontracts.Topology
	verifier          *mediacontracts.TokenVerifier
	e2ee              bool
	membersOnly       bool
	children          map[string]bool
	accepting         bool
	draining          bool
	participants      map[string]*Participant
	identities        map[string]*Participant
	tracks            map[string]*Track
	publishers        map[string]*publisherConn
	lostWHIPs         map[string]*publisherConn
	receiptGone       map[string]struct{}
	subscribers       map[string]*subscriberConn
	speakers          speakerDetector
	upstreamListeners int64
	upstreamKnown     bool
	childListeners    map[string]int64
	lastListeners     int64
	lastListenersSent time.Time
	listenersDirty    bool
	closed            bool

	ingress  rateMeter
	egress   rateMeter
	limiter  ratelimit.Limiter
	done     chan struct{}
	once     sync.Once
	failed   chan struct{}
	failOnce sync.Once
	failure  error
	packets  atomic.Int64
}

type Participant struct {
	info         mediacontracts.Participant
	remote       bool
	whep         *subscriberConn
	whips        map[string]*publisherConn
	grants       map[string]grant
	announced    bool
	reported     bool
	role         string
	graceEnd     *time.Timer
	nonce        string
	activitiesAt time.Time
}

type grant struct {
	role string
	exp  int64
}

const MaxPublisherGrace = 30 * time.Second

func ClampPublisherGrace(grace time.Duration) (time.Duration, bool) {
	if grace <= 0 {
		return MaxPublisherGrace, false
	}
	if grace > MaxPublisherGrace {
		return MaxPublisherGrace, true
	}
	return grace, false
}

func NewSession(opts Options) (*Session, error) {
	if opts.SessionID == "" || opts.NodeID == "" || opts.Factory == nil {
		return nil, errors.New("sfu: session options are incomplete")
	}
	if opts.Role != mediacontracts.RoleSFU && opts.Role != mediacontracts.RoleRelay {
		return nil, errors.New("sfu: role must be sfu or relay")
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if grace, clamped := ClampPublisherGrace(opts.PublisherGrace); clamped {
		opts.Logf("sfu: publisher reconnect grace %s above the spec's %s: clamped", opts.PublisherGrace, MaxPublisherGrace)
		opts.PublisherGrace = grace
	} else {
		opts.PublisherGrace = grace
	}
	if opts.SessionEvent == nil {
		opts.SessionEvent = func(any) {}
	}
	session := &Session{opts: opts, participants: map[string]*Participant{}, identities: map[string]*Participant{},
		tracks: map[string]*Track{}, publishers: map[string]*publisherConn{}, subscribers: map[string]*subscriberConn{},
		children: map[string]bool{}, done: make(chan struct{}), failed: make(chan struct{})}
	go session.loop()
	return session, nil
}

func (s *Session) isE2EE() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.e2ee
}

func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) ApplyView(view *mediacontracts.Topology) {
	if view == nil || view.View == nil || view.View.NodeID != s.opts.NodeID {
		return
	}
	project := ""
	if view.ProjectID != nil {
		project = *view.ProjectID
	}
	verifier := mediacontracts.NewTokenVerifier(mediacontracts.TokenVerifyContext{Audience: view.Tokens.Audience,
		Project: project, Session: view.SessionID, Keys: view.Tokens.Keys, Deny: view.Tokens.Deny})
	children := map[string]bool{}
	for _, link := range view.Links {
		if link.Parent == s.opts.NodeID {
			children[link.Child] = true
		}
	}
	self := viewNode(view, s.opts.NodeID)
	draining := self == nil || self.State == "draining"
	now := time.Now().Unix()

	revoked, applied := s.applyViewLocked(view, verifier, children, draining, now)
	if !applied {
		return
	}
	for _, ref := range revoked {
		reason := ref.reason
		if reason == "" {
			reason = "token_revoked"
		}
		if ref.publisher != nil {
			ref.publisher.close(reason, "token_revoked")
		} else {
			ref.subscriber.close(reason, "token_revoked")
		}
	}
}

func (s *Session) applyViewLocked(view *mediacontracts.Topology, verifier *mediacontracts.TokenVerifier, children map[string]bool,
	draining bool, now int64) ([]connectionRef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	s.view, s.verifier, s.children = view, verifier, children
	s.e2ee = view.Protection.Mode == "e2ee"
	s.membersOnly = s.e2ee && view.Protection.KeyProvider != nil && *view.Protection.KeyProvider == "mls"
	startDrain := draining && !s.draining
	s.draining = draining
	s.accepting = !draining
	var revoked []connectionRef
	for _, conn := range s.publishers {
		if verifier.Revoked(conn.claims.Nonce, conn.claims.Identity, now) {
			revoked = append(revoked, connectionRef{publisher: conn})
		}
	}
	for _, conn := range s.subscribers {
		if conn.child != "" && !children[conn.child] {
			revoked = append(revoked, connectionRef{subscriber: conn, reason: "draining"})
			continue
		}
		if verifier.Revoked(conn.claims.Nonce, conn.claims.Identity, now) {
			revoked = append(revoked, connectionRef{subscriber: conn})
		}
	}
	var migrate event
	if startDrain {
		migrate, _ = newEvent(mediacontracts.MigrateEvent{EventHeader: header("migrate"), Reason: "drain", AfterMS: 0})
		for _, conn := range s.subscribers {
			conn.ev().push(migrate)
		}
	}
	return revoked, true
}

type connectionRef struct {
	publisher  *publisherConn
	subscriber *subscriberConn
	reason     string
}

func viewNode(view *mediacontracts.Topology, nodeID string) *mediacontracts.TopologyNode {
	for index := range view.Nodes {
		if view.Nodes[index].NodeID == nodeID {
			return &view.Nodes[index]
		}
	}
	return nil
}

func (s *Session) View() *mediacontracts.Topology {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.view
}

func (s *Session) Close() {
	publishers, subscribers, closing := func() ([]*publisherConn, []*subscriberConn, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return nil, nil, false
		}
		s.closed = true
		s.accepting = false
		publishers := make([]*publisherConn, 0, len(s.publishers))
		for _, conn := range s.publishers {
			publishers = append(publishers, conn)
		}
		subscribers := make([]*subscriberConn, 0, len(s.subscribers))
		for _, conn := range s.subscribers {
			subscribers = append(subscribers, conn)
		}
		return publishers, subscribers, true
	}()
	if !closing {
		return
	}
	for _, conn := range subscribers {
		conn.ev().pushValue(mediacontracts.ConnectionClosedEvent{EventHeader: header("connection_closed"), Reason: "draining"})
	}
	if len(subscribers) > 0 {
		deadline := time.Now().Add(250 * time.Millisecond)
		for _, conn := range subscribers {
			conn.ev().waitSent(time.Until(deadline))
		}
	}
	for _, conn := range subscribers {
		conn.close("", "left")
	}
	for _, conn := range publishers {
		conn.close("", "left")
	}
	s.once.Do(func() { close(s.done) })
}

type Load struct {
	Publishers  int64
	Subscribers int64
	Children    int64
	IngressBPS  int64
	EgressBPS   int64
	Packets     int64
	Tracks      int64
}

func (s *Session) Load() Load {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	publishers, children := int64(0), int64(0)
	for _, participant := range s.participants {
		if !participant.remote && len(participant.whips) > 0 {
			publishers++
		}
	}
	for _, conn := range s.subscribers {
		if conn.child != "" {
			children++
		}
	}
	return Load{Publishers: publishers, Subscribers: int64(len(s.subscribers)), Children: children, IngressBPS: s.ingress.value(now),
		EgressBPS: s.egress.value(now), Packets: s.packets.Load(), Tracks: int64(len(s.tracks))}
}

func newID(prefix string) string {
	buffer := make([]byte, 16)
	_, _ = rand.Read(buffer)
	return prefix + base64.RawURLEncoding.EncodeToString(buffer)
}

func (s *Session) loop() {
	ticker := time.NewTicker(speakerTick)
	defer ticker.Stop()
	lastSweep, lastReceipts := time.Now(), time.Now()
	for {
		select {
		case <-s.done:
			return
		case now := <-ticker.C:
			s.safeTick(now)
			if now.Sub(lastSweep) >= sweepEvery {
				lastSweep = now
				s.safeSweep()
			}
			if now.Sub(lastReceipts) >= receiptSweep {
				lastReceipts = now
				s.safeReceipts()
			}
		}
	}
}

func (s *Session) safeSweep() {
	defer s.guard("session sweep", nil)
	s.sweepEnded()
}

func (s *Session) safeReceipts() {
	defer s.guard("session receipts", nil)
	s.checkReceipts()
}

func (s *Session) safeTick(now time.Time) {
	defer s.guard("session tick", nil)
	s.tick(now)
}

func (s *Session) tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.listenersDirty && now.Sub(s.lastListenersSent) >= 5*time.Second {
		s.listenersDirty = false
		if count := s.listenersLocked(); count != s.lastListeners {
			s.lastListeners, s.lastListenersSent = count, now
			s.broadcastLocked(mediacontracts.ListenerCountEvent{EventHeader: header("listener_count"), Listeners: count}, nil)
		}
	}
	if s.opts.ExternalSpeakers {
		return
	}
	speakers, dominant, changed := s.speakers.update(now, s.speechTracksLocked())
	if changed {
		s.broadcastLocked(mediacontracts.ActiveSpeakersEvent{EventHeader: header("active_speakers"), Speakers: speakers, DominantID: dominant}, nil)
	}
}
