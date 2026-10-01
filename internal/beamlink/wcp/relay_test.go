package wcp

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	orchestratorregistry "github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/resources"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/handlers/storageprobe"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

const relayTestHotkey = "5F3sa2TJAWMqDhXG6jhV4N8ko9SxwGy8TpaNS1repo5EYjQX"

var relayTestPublicAddress = netip.MustParseAddr("52.216.0.10")

type relayTestBed struct {
	orchestrator *Server
	privateKey   ed25519.PrivateKey
	keyID        string
	storage      *httptest.Server
	dialed       chan netip.AddrPort
	events       chan StorageProbeRelayEvent
	workers      map[string]context.CancelFunc
}

// newRelayTestBed runs an Orchestrator WCP server whose Workers relay to a
// loopback TLS server standing in for a storage host on a public address.
func newRelayTestBed(t *testing.T) *relayTestBed {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	storage := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, "storage-answer")
	}))
	// Relays torn down mid-handshake are expected here.
	storage.Config.ErrorLog = log.New(io.Discard, "", 0)
	storage.StartTLS()
	t.Cleanup(storage.Close)
	registry, err := orchestratorregistry.New(orchestratordomain.Orchestrator{OrchestratorID: "orchestrator-1", Hotkey: relayTestHotkey})
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := NewServer("orchestrator-1", registry, 1)
	if err != nil {
		t.Fatal(err)
	}
	bed := &relayTestBed{orchestrator: orchestrator, privateKey: privateKey, keyID: contracts.StorageProbeRelayKeyID(publicKey),
		storage: storage, dialed: make(chan netip.AddrPort, 8), events: make(chan StorageProbeRelayEvent, 256),
		workers: make(map[string]context.CancelFunc)}
	orchestrator.SetStorageProbeRelaySink(func(event StorageProbeRelayEvent) { bed.events <- event })
	serverTLS, clientTLS := testTLSConfigs(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
	})
	go func() { _ = orchestrator.Serve(ctx, listener) }()
	bed.startWorker(t, ctx, registry, listener.Addr().String(), clientTLS, "worker-relay", base64.RawURLEncoding.EncodeToString(publicKey))
	bed.startWorker(t, ctx, registry, listener.Addr().String(), clientTLS, "worker-plain", "")
	return bed
}

func (b *relayTestBed) startWorker(t *testing.T, parent context.Context, registry *orchestratorregistry.Registry,
	address string, clientTLS *tls.Config, workerID, relayKey string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.Identity{WorkerID: workerID, OrchestratorID: "orchestrator-1", NodeID: NodeID(publicKey), InstanceID: "instance-1"}
	if err := registry.Join(orchestratordomain.Membership{
		OrchestratorID: "orchestrator-1", WorkerID: workerID, NodeID: identity.NodeID, Status: "active",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	capacity := domain.Resources{CPUMillis: 1000, MemoryBytes: 64 << 20, Connections: 10, Streams: 10}
	governor, _ := resources.NewGovernor(capacity)
	store := runtime.NewMemoryStore()
	capabilities := []string{"action.execute"}
	var binder StorageProbeRelayBinder
	if relayKey != "" {
		capabilities = append(capabilities, contracts.StorageProbeRelayCapability)
		service, err := storageprobe.New(storageprobe.Config{
			WorkerID: workerID, PublicKey: relayKey,
			Resolve: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{relayTestPublicAddress}, nil
			},
			Dial: func(ctx context.Context, vetted netip.AddrPort) (net.Conn, error) {
				b.dialed <- vetted
				var dialer net.Dialer
				return dialer.DialContext(ctx, "tcp", b.storage.Listener.Addr().String())
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		binder = func(hotkey string, emitter StorageProbeRelayEmitter) StorageProbeRelaySession {
			return service.Bind(hotkey, emitter)
		}
	}
	engine, err := runtime.NewEngine(workerID, capabilities, runtime.NewRegistry(), governor, store)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		Address: address, TLSConfig: clientTLS, PrivateKey: privateKey, Identity: identity, SoftwareVersion: "test",
		Capabilities: capabilities, TotalResources: capacity, Engine: engine, Governor: governor, Store: store,
		ReconnectMinimum: 10 * time.Millisecond, ReconnectMaximum: 50 * time.Millisecond, StorageProbeRelays: binder,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(parent)
	b.workers[workerID] = cancel
	go func() { _ = client.Run(ctx) }()
	waitFor(t, 3*time.Second, func() bool { return b.orchestrator.Connected(workerID) })
}

func (b *relayTestBed) open(t *testing.T, workerID string) (contracts.StorageProbeRelayOpen, time.Time) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(raw[:16])
	now := time.Now()
	expiresAt := now.Add(20 * time.Second)
	intent := contracts.StorageProbeRelayIntent{
		SchemaVersion: contracts.StorageProbeRelayIntentSchema, KeyID: b.keyID, Environment: "prod",
		RelayID:            id[0:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:32],
		OrchestratorHotkey: relayTestHotkey, WorkerID: workerID, Host: "example.com", Port: 443,
		IssuedAt: now.UTC().Format("2006-01-02T15:04:05.000Z"), ExpiresAt: expiresAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		MaxBytesUp: 65536, MaxBytesDown: 65536, MaxFrameBytes: 512, Nonce: base64.RawURLEncoding.EncodeToString(raw[16:]),
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(b.privateKey, intent.CanonicalMessage()))
	return contracts.StorageProbeRelayOpen{RelayID: intent.RelayID, Intent: encoded, Signature: signature}, expiresAt
}

func (b *relayTestBed) nextEvent(t *testing.T) StorageProbeRelayEvent {
	t.Helper()
	select {
	case event := <-b.events:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a relay event")
		return StorageProbeRelayEvent{}
	}
}

// beamCoreRelayConn is BeamCore's view of a relay: a byte stream made of
// sequenced data frames, over which it runs TLS with the storage host.
type beamCoreRelayConn struct {
	server    *Server
	relayID   string
	frame     int
	seq       int64
	chunks    chan []byte
	pending   []byte
	mu        sync.Mutex
	closed    chan string
	closeOnce sync.Once
}

func newBeamCoreRelayConn(t *testing.T, bed *relayTestBed, relayID string, frame int) *beamCoreRelayConn {
	conn := &beamCoreRelayConn{server: bed.orchestrator, relayID: relayID, frame: frame, chunks: make(chan []byte, 256),
		closed: make(chan string, 1)}
	go func() {
		var expected int64
		for event := range bed.events {
			if event.RelayID != relayID {
				continue
			}
			switch event.Type {
			case contracts.StorageProbeRelayDataType:
				data, err := base64.StdEncoding.DecodeString(event.Data)
				if err != nil || event.Seq != expected {
					t.Errorf("worker frame seq=%d want %d err=%v", event.Seq, expected, err)
				}
				expected++
				conn.chunks <- data
			case contracts.StorageProbeRelayCloseType:
				conn.closed <- event.Reason
				close(conn.chunks)
				return
			}
		}
	}()
	return conn
}

func (c *beamCoreRelayConn) Read(buffer []byte) (int, error) {
	if len(c.pending) == 0 {
		chunk, ok := <-c.chunks
		if !ok {
			return 0, io.EOF
		}
		c.pending = chunk
	}
	read := copy(buffer, c.pending)
	c.pending = c.pending[read:]
	return read, nil
}

func (c *beamCoreRelayConn) Write(buffer []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for offset := 0; offset < len(buffer); offset += c.frame {
		chunk := buffer[offset:min(offset+c.frame, len(buffer))]
		c.server.ForwardStorageProbeRelayData(contracts.StorageProbeRelayData{RelayID: c.relayID, Seq: c.seq,
			Data: base64.StdEncoding.EncodeToString(chunk)})
		c.seq++
	}
	return len(buffer), nil
}

func (c *beamCoreRelayConn) Close() error {
	c.closeOnce.Do(func() {
		c.server.CloseStorageProbeRelay(contracts.StorageProbeRelayClose{RelayID: c.relayID, Reason: contracts.StorageProbeRelayCompleted}, false)
	})
	return nil
}

func (c *beamCoreRelayConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *beamCoreRelayConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (c *beamCoreRelayConn) SetDeadline(_ time.Time) error      { return nil }
func (c *beamCoreRelayConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *beamCoreRelayConn) SetWriteDeadline(_ time.Time) error { return nil }

func TestStorageProbeRelayCarriesEndToEndTLSThroughOrchestratorAndWorker(t *testing.T) {
	bed := newRelayTestBed(t)
	waitFor(t, 3*time.Second, bed.orchestrator.StorageProbeRelayAvailable)
	open, expiresAt := bed.open(t, "worker-relay")
	if reason := bed.orchestrator.OpenStorageProbeRelay("worker-relay", open, expiresAt); reason != "" {
		t.Fatalf("open refused: %s", reason)
	}
	if event := bed.nextEvent(t); event.Type != contracts.StorageProbeRelayOpenedType || event.RelayID != open.RelayID {
		t.Fatalf("expected opened, got %+v", event)
	}
	if dialed := <-bed.dialed; dialed != netip.AddrPortFrom(relayTestPublicAddress, 443) {
		t.Fatalf("worker dialed %s, want the vetted address on 443", dialed)
	}

	relayConn := newBeamCoreRelayConn(t, bed, open.RelayID, 512)
	roots := x509.NewCertPool()
	roots.AddCert(bed.storage.Certificate())
	tlsConn := tls.Client(relayConn, &tls.Config{ServerName: "example.com", RootCAs: roots, MinVersion: tls.VersionTLS12})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake over the relay: %v", err)
	}
	if peer := tlsConn.ConnectionState().PeerCertificates[0]; !peer.Equal(bed.storage.Certificate()) {
		t.Fatal("relay did not reach the storage host's certificate")
	}
	if _, err := io.WriteString(tlsConn, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "storage-answer" {
		t.Fatalf("storage answer status=%d body=%q", response.StatusCode, body)
	}
	select {
	case reason := <-relayConn.closed:
		if reason != contracts.StorageProbeRelayEOF {
			t.Fatalf("relay close reason=%s, want eof", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not close after the storage host closed")
	}
}

func TestStorageProbeRelayRoutingRefusalsAndLinkLoss(t *testing.T) {
	bed := newRelayTestBed(t)
	waitFor(t, 3*time.Second, bed.orchestrator.StorageProbeRelayAvailable)

	open, expiresAt := bed.open(t, "worker-missing")
	if reason := bed.orchestrator.OpenStorageProbeRelay("worker-missing", open, expiresAt); reason != contracts.StorageProbeRelayWorkerUnavailable {
		t.Fatalf("unknown worker reason=%q", reason)
	}
	open, expiresAt = bed.open(t, "worker-plain")
	if reason := bed.orchestrator.OpenStorageProbeRelay("worker-plain", open, expiresAt); reason != contracts.StorageProbeRelayUnsupported {
		t.Fatalf("worker without the capability reason=%q", reason)
	}

	open, expiresAt = bed.open(t, "worker-relay")
	if reason := bed.orchestrator.OpenStorageProbeRelay("worker-relay", open, expiresAt); reason != "" {
		t.Fatalf("open refused: %s", reason)
	}
	if event := bed.nextEvent(t); event.Type != contracts.StorageProbeRelayOpenedType {
		t.Fatalf("expected opened, got %+v", event)
	}
	if reason := bed.orchestrator.OpenStorageProbeRelay("worker-relay", open, expiresAt); reason != "" {
		t.Fatalf("duplicate open of an active relay was refused: %s", reason)
	}
	bed.workers["worker-relay"]()
	for {
		event := bed.nextEvent(t)
		if event.Type == contracts.StorageProbeRelayCloseType {
			if event.RelayID != open.RelayID || event.Reason != contracts.StorageProbeRelayWorkerUnavailable {
				t.Fatalf("expected worker_unavailable close, got %+v", event)
			}
			break
		}
	}
	waitFor(t, 3*time.Second, func() bool { return !bed.orchestrator.StorageProbeRelayAvailable() })
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met")
}

func testTLSConfigs(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
	pool := x509.NewCertPool()
	parsed, _ := x509.ParseCertificate(der)
	pool.AddCert(parsed)
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, NextProtos: []string{ALPN}},
		&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: "localhost"}
}
