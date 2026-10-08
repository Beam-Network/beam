// Package storageprobe is the worker side of the storage probe relay
// (`storage.probe.relay.v2`). For each BeamCore-signed intent it opens one TCP
// connection to a vetted public address of the storage host on the intent's
// port and copies opaque bytes in sequenced, capped frames. BeamCore runs TLS
// end to end over the relay; the worker never terminates, inspects, logs or
// modifies relayed bytes.
package storageprobe

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

const (
	// DefaultMaxConcurrent bounds concurrent relays per worker.
	DefaultMaxConcurrent = 4
	connectTimeout       = 10 * time.Second
)

// BeamCorePublicKey is BeamCore's relay intent signing key (raw Ed25519,
// base64url). It ships with the participant runtime.
const BeamCorePublicKey = "zV-FjN_CkYuuNQrT6uarJ8F9OJqwHGeRsgomZijSuKc"

// beamCoreEnvironment is the environment BeamCore names in the intents it signs.
const beamCoreEnvironment = "prod"

// Emitter carries relay messages back to the orchestrator link.
type Emitter interface {
	Opened(contracts.StorageProbeRelayOpened) error
	Data(contracts.StorageProbeRelayData) error
	Close(contracts.StorageProbeRelayClose) error
}

type Config struct {
	// WorkerID is the worker's BeamCore worker_id.
	WorkerID string
	// PublicKey verifies intents; it defaults to BeamCorePublicKey (tests set their own).
	PublicKey     string
	MaxConcurrent int

	Now     func() time.Time
	Resolve func(context.Context, string) ([]netip.Addr, error)
	Dial    func(context.Context, netip.AddrPort) (net.Conn, error)
}

type Service struct {
	workerID      string
	key           ed25519.PublicKey
	keyID         string
	maxConcurrent int
	now           func() time.Time
	resolve       func(context.Context, string) ([]netip.Addr, error)
	dial          func(context.Context, netip.AddrPort) (net.Conn, error)

	mu     sync.Mutex
	seen   map[string]time.Time
	active int
}

func New(config Config) (*Service, error) {
	workerID := strings.TrimSpace(config.WorkerID)
	if workerID == "" {
		return nil, errors.New("storage probe relay requires the worker_id")
	}
	encodedKey := config.PublicKey
	if encodedKey == "" {
		encodedKey = BeamCorePublicKey
	}
	key, err := contracts.ParseStorageProbeRelayPublicKey(encodedKey)
	if err != nil {
		return nil, err
	}
	service := &Service{
		workerID: workerID, key: key, keyID: contracts.StorageProbeRelayKeyID(key), maxConcurrent: config.MaxConcurrent,
		now: config.Now, resolve: config.Resolve, dial: config.Dial, seen: make(map[string]time.Time),
	}
	if service.maxConcurrent <= 0 {
		service.maxConcurrent = DefaultMaxConcurrent
	}
	if service.now == nil {
		service.now = time.Now
	}
	if service.resolve == nil {
		service.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if service.dial == nil {
		service.dial = func(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", address.String())
		}
	}
	return service, nil
}

// Bind attaches the service to one orchestrator link session. Relays opened on
// a link end with it.
func (s *Service) Bind(orchestratorHotkey string, emitter Emitter) *Link {
	return &Link{service: s, hotkey: strings.TrimSpace(orchestratorHotkey), emitter: emitter, relays: make(map[string]*relay)}
}

// Link is the set of relays of one orchestrator link session.
type Link struct {
	service *Service
	hotkey  string
	emitter Emitter

	mu     sync.Mutex
	relays map[string]*relay
	closed bool
}

// Open validates an open request and starts the relay. It never blocks on
// network I/O; the dial and copy run in their own goroutines.
func (l *Link) Open(open contracts.StorageProbeRelayOpen) {
	if !contracts.ValidStorageProbeRelayID(open.RelayID) {
		return
	}
	intent, lifetime, reason := l.service.authorize(l.hotkey, open)
	if reason != "" {
		l.refuse(open.RelayID, reason)
		return
	}
	current := newRelay(l, intent, lifetime)
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		l.service.release()
		return
	}
	l.relays[intent.RelayID] = current
	l.mu.Unlock()
	current.start()
}

// Data queues one BeamCore frame for the storage connection.
func (l *Link) Data(frame contracts.StorageProbeRelayData) {
	if current := l.relay(frame.RelayID); current != nil {
		current.receive(frame)
	}
}

// Close ends a relay on request without sending further frames.
func (l *Link) Close(request contracts.StorageProbeRelayClose) {
	if current := l.relay(request.RelayID); current != nil {
		current.finish("", false)
	}
}

// CloseAll ends every relay of a link whose session has gone.
func (l *Link) CloseAll() {
	l.mu.Lock()
	l.closed = true
	relays := make([]*relay, 0, len(l.relays))
	for _, current := range l.relays {
		relays = append(relays, current)
	}
	l.mu.Unlock()
	for _, current := range relays {
		current.finish("", false)
	}
}

func (l *Link) relay(relayID string) *relay {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.relays[relayID]
}

func (l *Link) forget(relayID string, current *relay) {
	l.mu.Lock()
	if l.relays[relayID] == current {
		delete(l.relays, relayID)
	}
	l.mu.Unlock()
}

func (l *Link) refuse(relayID, reason string) {
	_ = l.emitter.Close(contracts.StorageProbeRelayClose{RelayID: relayID, Reason: reason})
}

// authorize applies the worker rules in contract order and reserves a
// concurrency slot. It returns the relay's remaining lifetime; a non-empty
// reason refuses the relay without dialing.
func (s *Service) authorize(orchestratorHotkey string, open contracts.StorageProbeRelayOpen) (contracts.StorageProbeRelayIntent, time.Duration, string) {
	intent, err := contracts.ParseStorageProbeRelayIntent(open.Intent)
	if err != nil {
		return intent, 0, contracts.StorageProbeRelayIntentInvalid
	}
	if intent.KeyID != s.keyID || !contracts.VerifyStorageProbeRelayIntent(s.key, intent, open.Signature) {
		return intent, 0, contracts.StorageProbeRelayIntentInvalid
	}
	if intent.ValidateFields() != nil || intent.RelayID != open.RelayID || intent.Environment != beamCoreEnvironment ||
		intent.WorkerID != s.workerID || orchestratorHotkey == "" || intent.OrchestratorHotkey != orchestratorHotkey {
		return intent, 0, contracts.StorageProbeRelayIntentInvalid
	}
	now := s.now()
	if intent.CheckTime(now) != nil {
		return intent, 0, contracts.StorageProbeRelayIntentExpired
	}
	_, expiresAt, _ := intent.Window()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, until := range s.seen {
		if !now.Before(until) {
			delete(s.seen, key)
		}
	}
	relayKey, nonceKey := "relay:"+intent.RelayID, "nonce:"+intent.Nonce
	_, relaySeen := s.seen[relayKey]
	_, nonceSeen := s.seen[nonceKey]
	s.seen[relayKey], s.seen[nonceKey] = expiresAt, expiresAt
	if relaySeen || nonceSeen {
		return intent, 0, contracts.StorageProbeRelayIntentReplayed
	}
	if s.active >= s.maxConcurrent {
		return intent, 0, contracts.StorageProbeRelayCapacityExhausted
	}
	s.active++
	return intent, expiresAt.Sub(now), ""
}

func (s *Service) release() {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
}

type relay struct {
	link     *Link
	intent   contracts.StorageProbeRelayIntent
	lifetime time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
	wake     chan struct{}

	done   atomic.Bool
	emitMu sync.Mutex

	mu      sync.Mutex
	conn    net.Conn
	opened  bool
	seqUp   int64
	bytesUp int64
	pending []byte
}

// newRelay bounds every step of the relay by the intent's remaining lifetime.
func newRelay(link *Link, intent contracts.StorageProbeRelayIntent, lifetime time.Duration) *relay {
	ctx, cancel := context.WithTimeout(context.Background(), lifetime)
	return &relay{link: link, intent: intent, lifetime: lifetime, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
}

// start runs the relay and closes it with timeout when its lifetime ends.
func (r *relay) start() {
	context.AfterFunc(r.ctx, func() {
		if errors.Is(r.ctx.Err(), context.DeadlineExceeded) {
			r.finish(contracts.StorageProbeRelayTimeout, true)
		}
	})
	go r.run()
}

// run vets every address of the target, dials one vetted address on the
// intent's port and then copies bytes from the storage host into sequenced
// frames.
func (r *relay) run() {
	service := r.link.service
	addresses, err := r.addresses()
	if err != nil || len(addresses) == 0 {
		r.finish(r.failure(contracts.StorageProbeRelayDNSFailed), true)
		return
	}
	for _, address := range addresses {
		if !contracts.PublicAddressAllowed(address) {
			r.finish(contracts.StorageProbeRelayTargetNotAllowed, true)
			return
		}
	}
	dialCtx, cancelDial := context.WithTimeout(r.ctx, connectTimeout)
	// ValidateFields bounds the port to 1..65535.
	conn, err := service.dial(dialCtx, netip.AddrPortFrom(preferredAddress(addresses), uint16(r.intent.Port)))
	cancelDial()
	if err != nil {
		r.finish(r.failure(contracts.StorageProbeRelayDialFailed), true)
		return
	}
	r.mu.Lock()
	if r.done.Load() {
		r.mu.Unlock()
		_ = conn.Close()
		return
	}
	r.conn = conn
	r.opened = true
	r.mu.Unlock()
	if !r.emit(func(emitter Emitter) error {
		return emitter.Opened(contracts.StorageProbeRelayOpened{RelayID: r.intent.RelayID})
	}) {
		return
	}
	go r.writeUpstream(conn)
	r.readDownstream(conn)
}

// addresses returns the target's addresses: an IP literal is its own single
// address; a hostname is resolved once.
func (r *relay) addresses() ([]netip.Addr, error) {
	if address, literal := contracts.StorageProbeRelayIPLiteral(r.intent.Host); literal {
		return []netip.Addr{address}, nil
	}
	ctx, cancel := context.WithTimeout(r.ctx, connectTimeout)
	defer cancel()
	return r.link.service.resolve(ctx, r.intent.Host)
}

// failure reports timeout when the relay's lifetime ended during the step.
func (r *relay) failure(reason string) string {
	if errors.Is(r.ctx.Err(), context.DeadlineExceeded) {
		return contracts.StorageProbeRelayTimeout
	}
	return reason
}

// preferredAddress dials IPv4 first because many hosts have no IPv6 route.
func preferredAddress(addresses []netip.Addr) netip.Addr {
	for _, address := range addresses {
		if address.Unmap().Is4() {
			return address.Unmap()
		}
	}
	return addresses[0]
}

func (r *relay) readDownstream(conn net.Conn) {
	buffer := make([]byte, r.intent.MaxFrameBytes)
	var sequence, total int64
	for {
		remaining := r.intent.MaxBytesDown - total
		read, err := conn.Read(buffer[:min(r.intent.MaxFrameBytes, remaining+1)])
		if read > 0 {
			forward := min(int64(read), remaining)
			if forward > 0 {
				frame := contracts.StorageProbeRelayData{RelayID: r.intent.RelayID, Seq: sequence,
					Data: base64.StdEncoding.EncodeToString(buffer[:forward])}
				if !r.emit(func(emitter Emitter) error { return emitter.Data(frame) }) {
					return
				}
				sequence++
				total += forward
			}
			if int64(read) > remaining {
				r.finish(contracts.StorageProbeRelayByteCapReached, true)
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				r.finish(contracts.StorageProbeRelayEOF, true)
			} else {
				r.finish(contracts.StorageProbeRelayIOError, true)
			}
			return
		}
	}
}

func (r *relay) writeUpstream(conn net.Conn) {
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.wake:
		}
		r.mu.Lock()
		chunk := r.pending
		r.pending = nil
		r.mu.Unlock()
		if len(chunk) == 0 {
			continue
		}
		if _, err := conn.Write(chunk); err != nil {
			r.finish(contracts.StorageProbeRelayIOError, true)
			return
		}
	}
}

// receive validates and queues one frame without blocking the link.
func (r *relay) receive(frame contracts.StorageProbeRelayData) {
	r.mu.Lock()
	if r.done.Load() {
		r.mu.Unlock()
		return
	}
	data, err := contracts.DecodeStorageProbeRelayData(frame.Data, r.intent.MaxFrameBytes)
	if !r.opened || err != nil || frame.Seq != r.seqUp {
		r.mu.Unlock()
		r.finish(contracts.StorageProbeRelayProtocolError, true)
		return
	}
	if r.bytesUp+int64(len(data)) > r.intent.MaxBytesUp {
		r.mu.Unlock()
		r.finish(contracts.StorageProbeRelayByteCapReached, true)
		return
	}
	r.seqUp++
	r.bytesUp += int64(len(data))
	r.pending = append(r.pending, data...)
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// emit sends one frame unless the relay has finished; frames never follow a close.
func (r *relay) emit(send func(Emitter) error) bool {
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if r.done.Load() {
		return false
	}
	if err := send(r.link.emitter); err != nil {
		r.finish("", false)
		return false
	}
	return true
}

// finish ends the relay once: it closes the storage connection, releases the
// concurrency slot and, when notify is set, sends close with reason.
func (r *relay) finish(reason string, notify bool) {
	if !r.done.CompareAndSwap(false, true) {
		return
	}
	r.cancel()
	r.mu.Lock()
	conn := r.conn
	r.pending = nil
	r.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	r.link.service.release()
	r.link.forget(r.intent.RelayID, r)
	if notify {
		r.emitMu.Lock()
		_ = r.link.emitter.Close(contracts.StorageProbeRelayClose{RelayID: r.intent.RelayID, Reason: reason})
		r.emitMu.Unlock()
	}
}
