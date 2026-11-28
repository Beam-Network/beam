package usage

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const (
	DefaultPeriod           = 60 * time.Second
	MaxConnectionsPerReport = 2_000
	maxReportPayloadBytes   = 640 << 10
	sampleInterval          = time.Second
)

const (
	DirectionIngress = "ingress"
	DirectionEgress  = "egress"

	TransportWHIP     = "whip"
	TransportWHEP     = "whep"
	TransportHLS      = "hls"
	TransportRTMP     = "rtmp"
	TransportRTMPS    = "rtmps"
	TransportSRT      = "srt"
	TransportChild    = "child"
	TransportUpstream = "upstream"
)

type Layer int

const (
	LayerNone Layer = iota
	LayerAudioOnly
	LayerLow
	LayerMedium
	LayerHigh
)

type Sink func(mediacontracts.UsageReportMessage) error

type Config struct {
	WorkloadID string
	SessionID  string
	NodeID     string
	WorkerID   string
	Role       string
	Epoch      func() int64
	Period     time.Duration
	Mono       func() time.Duration
	Sink       Sink
	Now        func() time.Time
	Sequences  SequenceStore
	Features   func() (usageV2, receipts bool)
}

type SequenceStore interface {
	Load() int64
	Save(sequence int64) error
}

type PeriodStore interface {
	LoadPeriodEnd() int64
	SavePeriodEnd(endMS int64) error
}

const maxClockLead = int64(2 * time.Minute / time.Millisecond)

type ConnectionInfo struct {
	TokenNonce      *string
	Identity        *string
	Direction       string
	Transport       string
	Layers          bool
	ID              string
	Proof           *mediacontracts.UsageProof
	Kind            *string
	ReceiptKey      string
	Detached        bool
	RequestTargetMS int64
	Versions        Versions
}

type Versions int

const (
	BothVersions Versions = iota
	OnlyV1
	OnlyV2
)

type Reporter struct {
	config  Config
	private ed25519.PrivateKey
	public  string

	reportMu sync.Mutex
	sequence int64

	mu                  sync.Mutex
	connections         map[string]*Connection
	periodStart         int64
	lastTick            int64
	closed              bool
	renditions          map[string]*rendition
	refused             atomic.Int64
	receiptRefusals     atomic.Int64
	clock               atomic.Int64
	born                time.Time
	periodMono, nextDue int64
	lead                time.Duration
	leadSet             bool
	jobs                *jobQueue
}

type Connection struct {
	reporter *Reporter
	id       string
	info     ConnectionInfo

	bytes atomic.Int64

	mu           sync.Mutex
	opened       time.Time
	closedAt     time.Time
	layer        Layer
	lossPPM      *int64
	rttMS        *int64
	lastBytes    int64
	lastSample   time.Time
	periodBytes  int64
	mediaMS      int64
	connectionMS int64
	layersMS     mediacontracts.LayersMS

	accounting
}

func NewKey() (ed25519.PrivateKey, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	return private, err
}

func New(config Config, key ed25519.PrivateKey) (*Reporter, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("usage: invalid node key")
	}
	if config.SessionID == "" || config.NodeID == "" || config.WorkerID == "" || config.Role == "" || config.WorkloadID == "" {
		return nil, errors.New("usage: node identity is incomplete")
	}
	if config.Period <= 0 {
		config.Period = DefaultPeriod
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Epoch == nil {
		config.Epoch = func() int64 { return 1 }
	}
	reporter := &Reporter{config: config, private: key, public: mediacontracts.B64URL(key.Public().(ed25519.PublicKey)),
		connections: make(map[string]*Connection), renditions: map[string]*rendition{}, born: config.Now()}
	if config.Sequences != nil {
		reporter.sequence = max(config.Sequences.Load(), 0)
	}
	reporter.periodStart = reporter.NowMS()
	if periods, ok := config.Sequences.(PeriodStore); ok {
		if end := periods.LoadPeriodEnd(); end-reporter.periodStart <= maxClockLead {
			reporter.periodStart = max(reporter.periodStart, end)
		}
	}
	reporter.lastTick = reporter.clock.Load()
	reporter.ahead()
	reporter.periodMono = reporter.monoMS()
	reporter.nextDue = reporter.periodMono + config.Period.Milliseconds()
	return reporter, nil
}

func (r *Reporter) NodeKey() string { return r.public }

func (r *Reporter) Sign(schema string, payload []byte) mediacontracts.SignedEnvelope {
	return mediacontracts.SignEnvelope(schema, payload, r.private)
}

func (r *Reporter) Open(info ConnectionInfo) *Connection {
	now := r.config.Now()
	id := info.ID
	if id == "" {
		id = NewConnectionID()
	}
	connection := &Connection{reporter: r, id: id, info: info, opened: now, lastSample: now}
	connection.proof = info.Proof
	at := r.NowMS()
	r.mu.Lock()
	connection.openPresence(at, r.periodStart)
	if !r.closed {
		r.connections[connection.id] = connection
	}
	r.mu.Unlock()
	return connection
}

func (c *Connection) ID() string { return c.id }

func (c *Connection) AddBytes(n int) {
	if c != nil && n > 0 {
		c.bytes.Add(int64(n))
	}
}

func (c *Connection) SetLayer(layer Layer) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.layer = layer
	c.mu.Unlock()
}

func (c *Connection) SetQuality(lossPPM, rttMS *int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if lossPPM != nil {
		value := min(max(*lossPPM, 0), 1_000_000)
		c.lossPPM = &value
	}
	if rttMS != nil {
		value := max(*rttMS, 0)
		c.rttMS = &value
	}
	c.mu.Unlock()
}

func (c *Connection) Close() {
	if c == nil {
		return
	}
	c.closeAt(c.reporter.NowMS())
}

func (c *Connection) closeAt(atMS int64) {
	now := c.reporter.config.Now()
	c.mu.Lock()
	c.closeLocked(now, atMS)
	c.mu.Unlock()
}

func (c *Connection) closeLocked(now time.Time, atMS int64) {
	if !c.closedAt.IsZero() || c.draining {
		return
	}
	c.sampleLocked(now)
	c.sampleV2(c.reporter.NowMS())
	c.presence.end(atMS)
	if line, ok := c.presence.(*requests); ok && line.inflight > 0 {
		c.draining = true
		return
	}
	c.closedAt = now
}

func (c *Connection) sampleLocked(now time.Time) {
	if !c.closedAt.IsZero() || !now.After(c.lastSample) {
		return
	}
	elapsed := now.Sub(c.lastSample).Milliseconds()
	total := c.bytes.Load()
	delta := total - c.lastBytes
	c.lastBytes = total
	c.lastSample = now
	c.periodBytes += delta
	c.connectionMS += elapsed
	if delta > 0 {
		c.mediaMS += elapsed
		switch c.layer {
		case LayerHigh:
			c.layersMS.High += elapsed
		case LayerMedium:
			c.layersMS.Medium += elapsed
		case LayerLow:
			c.layersMS.Low += elapsed
		case LayerAudioOnly:
			c.layersMS.AudioOnly += elapsed
		}
	}
}
