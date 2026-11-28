package roomworkloads

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"
)

const (
	mediaPanicMinBackoff = time.Second
	mediaPanicMaxBackoff = 30 * time.Second
	mediaPanicQuarantine = 3
	mediaPanicWindow     = 10 * time.Minute
)

type mediaRestartPolicy struct {
	minBackoff, maxBackoff time.Duration
	quarantine             int
	window                 time.Duration
}

func defaultMediaRestartPolicy() mediaRestartPolicy {
	return mediaRestartPolicy{minBackoff: mediaPanicMinBackoff, maxBackoff: mediaPanicMaxBackoff, quarantine: mediaPanicQuarantine,
		window: mediaPanicWindow}
}

var ErrMediaStopped = errors.New("room.media is stopped on this Orchestrator")

type MediaPanic struct {
	Edge  string
	Value any
	Stack []byte
}

func (p *MediaPanic) Error() string { return fmt.Sprintf("media %s: panic: %v", p.Edge, p.Value) }

type mediaLife struct {
	holdsMu     sync.Mutex
	resultHolds map[string]time.Time

	shapesMu sync.Mutex
	shapes   map[string]*nodeShape

	placeMu  sync.Mutex
	reserved map[string]mediaReservation

	topologyMu       sync.Mutex
	topologies       map[string]*pendingTopology
	topologyBytes    int
	topologyFlushing bool

	stop     chan struct{}
	stopOnce sync.Once
}

func newMediaLife() *mediaLife {
	return &mediaLife{resultHolds: map[string]time.Time{}, shapes: map[string]*nodeShape{}, reserved: map[string]mediaReservation{},
		topologies: map[string]*pendingTopology{}, stop: make(chan struct{})}
}

func (l *mediaLife) halt() bool {
	halted := false
	l.stopOnce.Do(func() {
		close(l.stop)
		halted = true
	})
	return halted
}

func (l *mediaLife) stopped() bool {
	select {
	case <-l.stop:
		return true
	default:
		return false
	}
}

func (n *mediaNodes) current() *mediaLife { return n.life.Load() }

func (m *Manager) StopMedia() {
	nodes := m.mediaNodes
	nodes.lifeMu.Lock()
	defer nodes.lifeMu.Unlock()
	nodes.held = true
	nodes.current().halt()
}

func (m *Manager) StartMedia() error {
	nodes := m.mediaNodes
	nodes.lifeMu.Lock()
	defer nodes.lifeMu.Unlock()
	nodes.held, nodes.panics = false, nil
	return nodes.startLocked()
}

func (m *Manager) MediaRunning() bool { return !m.mediaNodes.current().stopped() }

func (m *Manager) OnMediaPanic(handler func(*MediaPanic)) {
	m.mediaNodes.lifeMu.Lock()
	m.mediaNodes.onPanic = handler
	m.mediaNodes.lifeMu.Unlock()
}

func (m *Manager) RecoverMediaPanic(edge string) {
	if value := recover(); value != nil {
		m.mediaNodes.contain(m.mediaNodes.current(), edge, value)
	}
}

func (n *mediaNodes) startLocked() error {
	previous := n.current()
	if !previous.stopped() {
		return nil
	}
	if err := n.reloadDrain(); err != nil {
		return fmt.Errorf("start room.media: %w", err)
	}
	n.upstream.repair()
	next := newMediaLife()
	pending := n.handOver(previous, next)
	restarted := n.restarted
	n.restarted = make(chan struct{})
	close(restarted)
	log.Printf("room.media started (%d drained Workers, %d reservations and %d pending topology updates handed over)",
		len(n.drainedWorkers()), len(next.reserved), pending)
	if pending > 0 {
		n.ensureTopologyFlusher(next)
	}
	return nil
}

func (n *mediaNodes) handOver(previous, next *mediaLife) int {
	now := n.now()
	previous.placeMu.Lock()
	defer previous.placeMu.Unlock()
	for key, reservation := range previous.reserved {
		if now.Sub(reservation.at) <= mediaReservationTTL {
			next.reserved[key] = reservation
		}
	}
	waiting := 0
	previous.withTopologies(func() {
		previous.pruneTopologyLocked(now)
		for key, entry := range previous.topologies {
			kept := *entry
			next.topologies[key] = &kept
			next.topologyBytes += len(kept.payload)
			if !kept.delivered {
				waiting++
			}
		}
		n.life.Store(next)
	})
	return waiting
}

func (n *mediaNodes) awaitStart(ctx context.Context) *mediaLife {
	for {
		n.lifeMu.Lock()
		life, restarted := n.current(), n.restarted
		n.lifeMu.Unlock()
		if ctx.Err() != nil {
			return nil
		}
		if !life.stopped() {
			return life
		}
		select {
		case <-ctx.Done():
			return nil
		case <-restarted:
		}
	}
}

func (n *mediaNodes) contain(life *mediaLife, edge string, value any) *MediaPanic {
	report := &MediaPanic{Edge: edge, Value: value, Stack: debug.Stack()}
	n.lifeMu.Lock()
	acted := life == n.current() && life.halt()
	handler := n.onPanic
	var backoff time.Duration
	quarantined := false
	if acted && handler == nil {
		backoff, quarantined = n.recordPanicLocked()
	}
	n.lifeMu.Unlock()
	if !acted {
		log.Printf("ERROR room.media %s: recovered panic of a start already stopped or replaced: %v\n%s", edge, value, report.Stack)
		return report
	}
	log.Printf("ERROR room.media %s: recovered panic, media stopped: %v\n%s", edge, value, report.Stack)
	switch {
	case handler != nil:
		handler(report)
	case quarantined:
		log.Printf("ERROR room.media panicked %d times within %s: it stays stopped until StartMedia", n.restartPolicy.quarantine,
			n.restartPolicy.window)
	default:
		log.Printf("room.media starts again in %s", backoff)
		time.AfterFunc(backoff, func() { n.restartAfterPanicRecovered(life) })
	}
	return report
}

func (n *mediaNodes) recordPanicLocked() (time.Duration, bool) {
	policy, now := n.restartPolicy, n.now()
	recent := n.panics[:0]
	for _, at := range n.panics {
		if now.Sub(at) < policy.window {
			recent = append(recent, at)
		}
	}
	n.panics = append(recent, now)
	if len(n.panics) >= policy.quarantine {
		return 0, true
	}
	backoff := policy.minBackoff
	for range len(n.panics) - 1 {
		backoff = min(2*backoff, policy.maxBackoff)
	}
	return backoff, false
}

func (n *mediaNodes) restartAfterPanic(life *mediaLife) {
	n.lifeMu.Lock()
	defer n.lifeMu.Unlock()
	if n.held || n.current() != life {
		return
	}
	if err := n.startLocked(); err != nil {
		log.Printf("ERROR room.media not started again after a panic: %v", err)
	}
}

func (n *mediaNodes) restartAfterPanicRecovered(life *mediaLife) {
	defer func() {
		value := recover()
		if value == nil {
			return
		}
		log.Printf("ERROR room.media start after a panic: recovered panic: %v\n%s", value, debug.Stack())
		n.lifeMu.Lock()
		retry := !n.held && n.current() == life && n.onPanic == nil
		var backoff time.Duration
		quarantined := false
		if retry {
			backoff, quarantined = n.recordPanicLocked()
		}
		n.lifeMu.Unlock()
		switch {
		case !retry:
		case quarantined:
			log.Printf("ERROR room.media panicked %d times within %s: it stays stopped until StartMedia", n.restartPolicy.quarantine,
				n.restartPolicy.window)
		default:
			log.Printf("room.media starts again in %s", backoff)
			time.AfterFunc(backoff, func() { n.restartAfterPanicRecovered(life) })
		}
	}()
	n.restartAfterPanic(life)
}

func (n *mediaNodes) recoverInto(life *mediaLife, edge string, err *error) {
	if value := recover(); value != nil {
		*err = n.contain(life, edge, value)
	}
}
