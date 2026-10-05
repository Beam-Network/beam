package storageprobe

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

const (
	testHotkey   = "5F3sa2TJAWMqDhXG6jhV4N8ko9SxwGy8TpaNS1repo5EYjQX"
	testWorkerID = "worker-7d1f"
	testHost     = "storage.example.com"
)

var testPublicAddress = netip.MustParseAddr("52.216.0.10")

type relayMessage struct {
	kind   string
	seq    int64
	data   []byte
	reason string
}

type recordingEmitter struct {
	messages chan relayMessage
}

func newRecordingEmitter() *recordingEmitter {
	return &recordingEmitter{messages: make(chan relayMessage, 256)}
}

func (e *recordingEmitter) Opened(contracts.StorageProbeRelayOpened) error {
	e.messages <- relayMessage{kind: "opened"}
	return nil
}

func (e *recordingEmitter) Data(frame contracts.StorageProbeRelayData) error {
	data, err := base64.StdEncoding.DecodeString(frame.Data)
	if err != nil {
		return err
	}
	e.messages <- relayMessage{kind: "data", seq: frame.Seq, data: data}
	return nil
}

func (e *recordingEmitter) Close(request contracts.StorageProbeRelayClose) error {
	e.messages <- relayMessage{kind: "close", reason: request.Reason}
	return nil
}

func (e *recordingEmitter) next(t *testing.T) relayMessage {
	t.Helper()
	select {
	case message := <-e.messages:
		return message
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a relay message")
		return relayMessage{}
	}
}

func (e *recordingEmitter) expectClose(t *testing.T, reason string) {
	t.Helper()
	for {
		message := e.next(t)
		if message.kind == "close" {
			if message.reason != reason {
				t.Fatalf("close reason=%s, want %s", message.reason, reason)
			}
			return
		}
	}
}

func (e *recordingEmitter) expectSilence(t *testing.T) {
	t.Helper()
	select {
	case message := <-e.messages:
		t.Fatalf("unexpected relay message after close: %+v", message)
	case <-time.After(150 * time.Millisecond):
	}
}

type testSigner struct {
	privateKey ed25519.PrivateKey
	publicKey  string
	keyID      string
}

func newTestSigner(t *testing.T) testSigner {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testSigner{privateKey: privateKey, publicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		keyID: contracts.StorageProbeRelayKeyID(publicKey)}
}

func randomRelayID(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	value := hex.EncodeToString(raw)
	return value[0:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:32]
}

func (s testSigner) intent(t *testing.T, now time.Time, change func(*contracts.StorageProbeRelayIntent)) contracts.StorageProbeRelayOpen {
	t.Helper()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	intent := contracts.StorageProbeRelayIntent{
		SchemaVersion: contracts.StorageProbeRelayIntentSchema, KeyID: s.keyID, Environment: "prod",
		RelayID: randomRelayID(t), OrchestratorHotkey: testHotkey, WorkerID: testWorkerID, Host: testHost,
		Port: 443, IssuedAt: now.UTC().Format("2006-01-02T15:04:05.000Z"),
		ExpiresAt:  now.Add(30 * time.Second).UTC().Format("2006-01-02T15:04:05.000Z"),
		MaxBytesUp: contracts.StorageProbeRelayMaxBytes, MaxBytesDown: contracts.StorageProbeRelayMaxBytes,
		MaxFrameBytes: contracts.StorageProbeRelayMaxFrameBytes, Nonce: base64.RawURLEncoding.EncodeToString(nonce),
	}
	if change != nil {
		change(&intent)
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.privateKey, intent.CanonicalMessage()))
	return contracts.StorageProbeRelayOpen{RelayID: intent.RelayID, Intent: raw, Signature: signature}
}

// v1Intent signs a v1 intent exactly as the v1 contract did: v1 schema and the
// v1 signature domain.
func (s testSigner) v1Intent(t *testing.T, now time.Time) contracts.StorageProbeRelayOpen {
	t.Helper()
	open := s.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.SchemaVersion = "storage-probe-relay/v1" })
	intent, err := contracts.ParseStorageProbeRelayIntent(open.Intent)
	if err != nil {
		t.Fatal(err)
	}
	message := "beam:storage-probe-relay-intent/v1" + strings.TrimPrefix(string(intent.CanonicalMessage()), contracts.StorageProbeRelaySignatureDomain)
	open.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.privateKey, []byte(message)))
	return open
}

// storageHost stands in for the storage host: the dial seam records the vetted
// address and connects to this loopback listener instead.
type storageHost struct {
	listener net.Listener
	dialed   chan netip.AddrPort
	accepted chan net.Conn
}

func newStorageHost(t *testing.T) *storageHost {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	host := &storageHost{listener: listener, dialed: make(chan netip.AddrPort, 16), accepted: make(chan net.Conn, 16)}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			host.accepted <- conn
		}
	}()
	return host
}

func (h *storageHost) dial(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
	h.dialed <- address
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", h.listener.Addr().String())
}

func (h *storageHost) accept(t *testing.T) net.Conn {
	t.Helper()
	select {
	case conn := <-h.accepted:
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	case <-time.After(5 * time.Second):
		t.Fatal("storage host accepted no connection")
		return nil
	}
}

func newTestService(t *testing.T, signer testSigner, host *storageHost, change func(*Config)) *Service {
	t.Helper()
	config := Config{
		WorkerID: testWorkerID, PublicKey: signer.publicKey,
		Resolve: func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{testPublicAddress}, nil },
	}
	if host != nil {
		config.Dial = host.dial
	} else {
		config.Dial = func(context.Context, netip.AddrPort) (net.Conn, error) {
			t.Error("relay dialed although it must have been refused")
			return nil, errors.New("unexpected dial")
		}
	}
	if change != nil {
		change(&config)
	}
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func sendFrame(link *Link, relayID string, seq int64, data []byte) {
	link.Data(contracts.StorageProbeRelayData{RelayID: relayID, Seq: seq, Data: base64.StdEncoding.EncodeToString(data)})
}

func TestBeamCorePublicKeyMatchesContractKeyID(t *testing.T) {
	service, err := New(Config{WorkerID: testWorkerID})
	if err != nil {
		t.Fatal(err)
	}
	if service.keyID != "593c98f42ac3b155" {
		t.Fatalf("key id=%s, want 593c98f42ac3b155", service.keyID)
	}
	if _, err := New(Config{WorkerID: testWorkerID, PublicKey: "not-a-key"}); err == nil {
		t.Fatal("malformed key was accepted")
	}
}

func TestRelayVerifiesContractTestVectors(t *testing.T) {
	vectorIntent := func(host string, port int) json.RawMessage {
		return json.RawMessage(`{"schema_version":"storage-probe-relay/v2","key_id":"56475aa75463474c","environment":"prod",` +
			`"relay_id":"6f1c2a4e-8b3d-4c5e-9f60-7a8b9c0d1e2f","orchestrator_hotkey":"` + testHotkey + `",` +
			`"worker_id":"worker-7d1f","host":"` + host + `","port":` + strconv.Itoa(port) + `,` +
			`"issued_at":"2026-10-01T12:00:00.000Z","expires_at":"2026-10-01T12:00:45.000Z","max_bytes_up":65536,` +
			`"max_bytes_down":65536,"max_frame_bytes":16384,"nonce":"AAECAwQFBgcICQoLDA0ODw"}`)
	}
	for _, vector := range []struct {
		name, host, signature string
		port                  int
		dialed                netip.AddrPort
	}{
		{name: "hostname", host: "example-bucket.s3.us-east-1.amazonaws.com", port: 443,
			signature: "Y-47UO2ad7PXHce8W93_QYeTDDUmGn-EDgkdAzzFYrJB7p1cE0Ohxoqw9yzDbsNnrDrpTjC3zWvQ4KxgSFFiDQ",
			dialed:    netip.AddrPortFrom(testPublicAddress, 443)},
		{name: "IP literal", host: "2606:4700:4700::1111", port: 9443,
			signature: "tAkRGsmw1op6UcPeofax9qspnZPYDlW4BliA5HwUr_rNu5gC3kYYeRPmBxx0QEKMtNphj9x6PofdTqfPnXFLCQ",
			dialed:    netip.MustParseAddrPort("[2606:4700:4700::1111]:9443")},
	} {
		t.Run(vector.name, func(t *testing.T) {
			open := contracts.StorageProbeRelayOpen{RelayID: "6f1c2a4e-8b3d-4c5e-9f60-7a8b9c0d1e2f",
				Intent: vectorIntent(vector.host, vector.port), Signature: vector.signature}
			host := newStorageHost(t)
			service, err := New(Config{
				WorkerID: testWorkerID, PublicKey: "A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg",
				Now: func() time.Time { return time.Date(2026, 10, 1, 12, 0, 10, 0, time.UTC) },
				Resolve: func(_ context.Context, name string) ([]netip.Addr, error) {
					if name != "example-bucket.s3.us-east-1.amazonaws.com" {
						t.Errorf("resolved %q", name)
					}
					return []netip.Addr{testPublicAddress}, nil
				},
				Dial: host.dial,
			})
			if err != nil {
				t.Fatal(err)
			}
			emitter := newRecordingEmitter()
			link := service.Bind(testHotkey, emitter)
			defer link.CloseAll()

			tampered := open
			tampered.Signature = "9" + vector.signature[1:]
			link.Open(tampered)
			emitter.expectClose(t, contracts.StorageProbeRelayIntentInvalid)

			link.Open(open)
			if message := emitter.next(t); message.kind != "opened" {
				t.Fatalf("contract test vector was not opened: %+v", message)
			}
			if dialed := <-host.dialed; dialed != vector.dialed {
				t.Fatalf("dialed %s, want %s", dialed, vector.dialed)
			}
		})
	}
}

func TestRelayDialsTheVettedAddressOnTheIntentPort(t *testing.T) {
	signer := newTestSigner(t)
	for _, test := range []struct {
		host   string
		port   int64
		dialed string
	}{
		{host: testHost, port: 9000, dialed: testPublicAddress.String() + ":9000"},
		{host: testHost, port: 65535, dialed: testPublicAddress.String() + ":65535"},
		{host: "52.216.0.20", port: 443, dialed: "52.216.0.20:443"},
		{host: "52.216.0.20", port: 1, dialed: "52.216.0.20:1"},
		{host: "2606:4700::1111", port: 8443, dialed: "[2606:4700::1111]:8443"},
		{host: "64:ff9b::808:808", port: 443, dialed: "[64:ff9b::808:808]:443"},
	} {
		t.Run(test.host+"/"+strconv.FormatInt(test.port, 10), func(t *testing.T) {
			host := newStorageHost(t)
			service := newTestService(t, signer, host, func(config *Config) {
				config.Resolve = func(_ context.Context, name string) ([]netip.Addr, error) {
					if name != testHost {
						t.Errorf("IP literal %q was resolved", name)
					}
					return []netip.Addr{testPublicAddress}, nil
				}
			})
			emitter := newRecordingEmitter()
			link := service.Bind(testHotkey, emitter)
			defer link.CloseAll()
			link.Open(signer.intent(t, time.Now(), func(intent *contracts.StorageProbeRelayIntent) {
				intent.Host, intent.Port = test.host, test.port
			}))
			if message := emitter.next(t); message.kind != "opened" {
				t.Fatalf("relay not opened: %+v", message)
			}
			if dialed := <-host.dialed; dialed != netip.MustParseAddrPort(test.dialed) {
				t.Fatalf("dialed %s, want %s", dialed, test.dialed)
			}
		})
	}
}

func TestRelayRefusesInvalidIntentsWithoutDialing(t *testing.T) {
	signer := newTestSigner(t)
	other := newTestSigner(t)
	now := time.Now()
	type refusal struct {
		name   string
		open   func() contracts.StorageProbeRelayOpen
		hotkey string
		reason string
	}
	tests := []refusal{
		{name: "unknown key", reason: contracts.StorageProbeRelayIntentInvalid,
			open: func() contracts.StorageProbeRelayOpen { return other.intent(t, now, nil) }},
		{name: "environment", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.Environment = "dev" })
		}},
		{name: "worker", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.WorkerID = "worker-other" })
		}},
		{name: "orchestrator hotkey", reason: contracts.StorageProbeRelayIntentInvalid, hotkey: "5OtherHotkey",
			open: func() contracts.StorageProbeRelayOpen { return signer.intent(t, now, nil) }},
		{name: "relay id mismatch", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			open := signer.intent(t, now, nil)
			open.RelayID = randomRelayID(t)
			return open
		}},
		{name: "unknown intent field", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			open := signer.intent(t, now, nil)
			open.Intent = json.RawMessage(strings.Replace(string(open.Intent), "{", `{"extra":1,`, 1))
			return open
		}},
		{name: "expired", reason: contracts.StorageProbeRelayIntentExpired, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now.Add(-40*time.Second), nil)
		}},
		{name: "issued in the future", reason: contracts.StorageProbeRelayIntentExpired, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now.Add(time.Minute), nil)
		}},
		{name: "lifetime above 60s", reason: contracts.StorageProbeRelayIntentExpired, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) {
				intent.ExpiresAt = now.Add(61 * time.Second).UTC().Format("2006-01-02T15:04:05.000Z")
			})
		}},
		{name: "v1 intent", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.v1Intent(t, now)
		}},
		{name: "v1 schema signed under v2", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.SchemaVersion = "storage-probe-relay/v1" })
		}},
		{name: "bracketed IPv6 literal", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.Host = "[2606:4700::1111]" })
		}},
		{name: "uncompressed IPv6 literal", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.Host = "2606:4700:0:0:0:0:0:1111" })
		}},
		{name: "uppercase IPv6 literal", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.Host = "2606:4700::ABCD" })
		}},
		{name: "zoned IPv6 literal", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.Host = "fe80::1%eth0" })
		}},
		{name: "leading zero IPv4 literal", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.Host = "52.216.0.010" })
		}},
		{name: "IPv4-mapped IPv6 literal", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.Host = "::ffff:8.8.8.8" })
		}},
		{name: "single label", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.Host = "localhost" })
		}},
		{name: "port zero", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.Port = 0 })
		}},
		{name: "port above 65535", reason: contracts.StorageProbeRelayIntentInvalid, open: func() contracts.StorageProbeRelayOpen {
			return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.Port = 65536 })
		}},
	}
	for _, literal := range []string{
		"10.0.0.1", "127.0.0.1", "169.254.169.254", "100.64.1.1", "192.168.0.10", "0.0.0.0", "203.0.113.7",
		"::1", "fd00::1", "fe80::1", "::", "64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe",
	} {
		tests = append(tests, refusal{name: "non-public literal " + literal, reason: contracts.StorageProbeRelayTargetNotAllowed,
			open: func() contracts.StorageProbeRelayOpen {
				return signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) { intent.Host, intent.Port = literal, 8443 })
			}})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := newTestService(t, signer, nil, func(config *Config) {
				config.Resolve = func(_ context.Context, name string) ([]netip.Addr, error) {
					t.Errorf("refused relay resolved %q", name)
					return nil, errors.New("unexpected resolve")
				}
			})
			emitter := newRecordingEmitter()
			hotkey := test.hotkey
			if hotkey == "" {
				hotkey = testHotkey
			}
			link := service.Bind(hotkey, emitter)
			defer link.CloseAll()
			link.Open(test.open())
			emitter.expectClose(t, test.reason)
			emitter.expectSilence(t)
		})
	}
}

func TestRelayRefusesNonPublicResolvedAddresses(t *testing.T) {
	signer := newTestSigner(t)
	for _, resolved := range [][]string{
		{"10.1.2.3"}, {"127.0.0.1"}, {"169.254.169.254"}, {"100.64.1.1"}, {"192.168.0.10"}, {"::1"},
		{"fd00::1"}, {"fe80::1"}, {"::ffff:127.0.0.1"}, {"64:ff9b::a00:1"}, {"0.0.0.0"}, {"198.18.0.1"},
		{testPublicAddress.String(), "10.0.0.1"},
	} {
		t.Run(strings.Join(resolved, ","), func(t *testing.T) {
			addresses := make([]netip.Addr, 0, len(resolved))
			for _, address := range resolved {
				addresses = append(addresses, netip.MustParseAddr(address))
			}
			service := newTestService(t, signer, nil, func(config *Config) {
				config.Resolve = func(context.Context, string) ([]netip.Addr, error) { return addresses, nil }
			})
			emitter := newRecordingEmitter()
			link := service.Bind(testHotkey, emitter)
			defer link.CloseAll()
			link.Open(signer.intent(t, time.Now(), nil))
			emitter.expectClose(t, contracts.StorageProbeRelayTargetNotAllowed)
		})
	}
}

func TestRelayReportsResolutionAndDialFailures(t *testing.T) {
	signer := newTestSigner(t)
	service := newTestService(t, signer, nil, func(config *Config) {
		config.Resolve = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	})
	emitter := newRecordingEmitter()
	link := service.Bind(testHotkey, emitter)
	link.Open(signer.intent(t, time.Now(), nil))
	emitter.expectClose(t, contracts.StorageProbeRelayDNSFailed)
	link.CloseAll()

	service = newTestService(t, signer, nil, func(config *Config) {
		config.Dial = func(context.Context, netip.AddrPort) (net.Conn, error) { return nil, errors.New("connection refused") }
	})
	link = service.Bind(testHotkey, emitter)
	defer link.CloseAll()
	link.Open(signer.intent(t, time.Now(), nil))
	emitter.expectClose(t, contracts.StorageProbeRelayDialFailed)
}

func TestRelayRejectsReplayAndBoundsConcurrency(t *testing.T) {
	signer := newTestSigner(t)
	host := newStorageHost(t)
	service := newTestService(t, signer, host, func(config *Config) { config.MaxConcurrent = 1 })
	emitter := newRecordingEmitter()
	link := service.Bind(testHotkey, emitter)
	defer link.CloseAll()

	first := signer.intent(t, time.Now(), nil)
	link.Open(first)
	if message := emitter.next(t); message.kind != "opened" {
		t.Fatalf("first relay: %+v", message)
	}
	host.accept(t)
	link.Open(first)
	emitter.expectClose(t, contracts.StorageProbeRelayIntentReplayed)
	var firstIntent contracts.StorageProbeRelayIntent
	if err := json.Unmarshal(first.Intent, &firstIntent); err != nil {
		t.Fatal(err)
	}
	link.Open(signer.intent(t, time.Now(), func(intent *contracts.StorageProbeRelayIntent) { intent.Nonce = firstIntent.Nonce }))
	emitter.expectClose(t, contracts.StorageProbeRelayIntentReplayed)
	link.Open(signer.intent(t, time.Now(), nil))
	emitter.expectClose(t, contracts.StorageProbeRelayCapacityExhausted)

	link.Close(contracts.StorageProbeRelayClose{RelayID: first.RelayID, Reason: contracts.StorageProbeRelayCompleted})
	emitter.expectSilence(t)
	link.Open(signer.intent(t, time.Now(), nil))
	if message := emitter.next(t); message.kind != "opened" {
		t.Fatalf("slot was not released by close: %+v", message)
	}
}

func TestRelayCopiesSequencedFramesAndEndsOnEOF(t *testing.T) {
	signer := newTestSigner(t)
	host := newStorageHost(t)
	service := newTestService(t, signer, host, nil)
	emitter := newRecordingEmitter()
	link := service.Bind(testHotkey, emitter)
	defer link.CloseAll()
	open := signer.intent(t, time.Now(), func(intent *contracts.StorageProbeRelayIntent) { intent.MaxFrameBytes = 4 })
	link.Open(open)
	if message := emitter.next(t); message.kind != "opened" {
		t.Fatalf("relay not opened: %+v", message)
	}
	storage := host.accept(t)
	sendFrame(link, open.RelayID, 0, []byte("ping"))
	sendFrame(link, open.RelayID, 1, []byte("!"))
	received := make([]byte, 5)
	if _, err := io.ReadFull(storage, received); err != nil || string(received) != "ping!" {
		t.Fatalf("storage received %q err=%v", received, err)
	}
	if _, err := storage.Write([]byte("pong-reply")); err != nil {
		t.Fatal(err)
	}
	_ = storage.Close()
	var downstream bytes.Buffer
	var expected int64
	for {
		message := emitter.next(t)
		if message.kind == "close" {
			if message.reason != contracts.StorageProbeRelayEOF {
				t.Fatalf("close reason=%s, want eof", message.reason)
			}
			break
		}
		if message.kind != "data" || message.seq != expected || len(message.data) == 0 || len(message.data) > 4 {
			t.Fatalf("unexpected downstream frame: %+v", message)
		}
		expected++
		downstream.Write(message.data)
	}
	if downstream.String() != "pong-reply" {
		t.Fatalf("downstream=%q", downstream.String())
	}
	emitter.expectSilence(t)
}

func TestRelayEnforcesFrameRules(t *testing.T) {
	signer := newTestSigner(t)
	tests := []struct {
		name   string
		send   func(*Link, string)
		reason string
	}{
		{name: "oversize frame", reason: contracts.StorageProbeRelayProtocolError,
			send: func(link *Link, relayID string) { sendFrame(link, relayID, 0, []byte("12345")) }},
		{name: "out of order", reason: contracts.StorageProbeRelayProtocolError,
			send: func(link *Link, relayID string) { sendFrame(link, relayID, 1, []byte("1")) }},
		{name: "empty frame", reason: contracts.StorageProbeRelayProtocolError, send: func(link *Link, relayID string) {
			link.Data(contracts.StorageProbeRelayData{RelayID: relayID, Seq: 0, Data: ""})
		}},
		{name: "unpadded base64", reason: contracts.StorageProbeRelayProtocolError, send: func(link *Link, relayID string) {
			link.Data(contracts.StorageProbeRelayData{RelayID: relayID, Seq: 0, Data: "YWI"})
		}},
		{name: "upstream byte cap", reason: contracts.StorageProbeRelayByteCapReached, send: func(link *Link, relayID string) {
			sendFrame(link, relayID, 0, []byte("1234"))
			sendFrame(link, relayID, 1, []byte("1234"))
			sendFrame(link, relayID, 2, []byte("12"))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := newStorageHost(t)
			service := newTestService(t, signer, host, nil)
			emitter := newRecordingEmitter()
			link := service.Bind(testHotkey, emitter)
			defer link.CloseAll()
			open := signer.intent(t, time.Now(), func(intent *contracts.StorageProbeRelayIntent) {
				intent.MaxFrameBytes, intent.MaxBytesUp = 4, 9
			})
			link.Open(open)
			if message := emitter.next(t); message.kind != "opened" {
				t.Fatalf("relay not opened: %+v", message)
			}
			storage := host.accept(t)
			test.send(link, open.RelayID)
			emitter.expectClose(t, test.reason)
			_ = storage.SetReadDeadline(time.Now().Add(2 * time.Second))
			forwarded, _ := io.ReadAll(storage)
			if len(forwarded) > 9 {
				t.Fatalf("forwarded %d bytes beyond the upstream cap", len(forwarded))
			}
		})
	}
}

func TestRelayRejectsDataBeforeOpened(t *testing.T) {
	signer := newTestSigner(t)
	release := make(chan struct{})
	host := newStorageHost(t)
	service := newTestService(t, signer, host, func(config *Config) {
		config.Dial = func(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
			<-release
			return host.dial(ctx, address)
		}
	})
	emitter := newRecordingEmitter()
	link := service.Bind(testHotkey, emitter)
	defer link.CloseAll()
	open := signer.intent(t, time.Now(), nil)
	link.Open(open)
	sendFrame(link, open.RelayID, 0, []byte("early"))
	close(release)
	emitter.expectClose(t, contracts.StorageProbeRelayProtocolError)
	emitter.expectSilence(t)
}

func TestRelayEnforcesDownstreamByteCap(t *testing.T) {
	signer := newTestSigner(t)
	host := newStorageHost(t)
	service := newTestService(t, signer, host, nil)
	emitter := newRecordingEmitter()
	link := service.Bind(testHotkey, emitter)
	defer link.CloseAll()
	open := signer.intent(t, time.Now(), func(intent *contracts.StorageProbeRelayIntent) {
		intent.MaxFrameBytes, intent.MaxBytesDown = 4, 10
	})
	link.Open(open)
	if message := emitter.next(t); message.kind != "opened" {
		t.Fatalf("relay not opened: %+v", message)
	}
	storage := host.accept(t)
	if _, err := storage.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatal(err)
	}
	var forwarded bytes.Buffer
	for {
		message := emitter.next(t)
		if message.kind == "close" {
			if message.reason != contracts.StorageProbeRelayByteCapReached {
				t.Fatalf("close reason=%s, want byte_cap_reached", message.reason)
			}
			break
		}
		forwarded.Write(message.data)
	}
	if forwarded.String() != "0123456789" {
		t.Fatalf("forwarded %q, want exactly max_bytes_down bytes", forwarded.String())
	}
}

func TestRelayClosesAtExpiry(t *testing.T) {
	signer := newTestSigner(t)
	host := newStorageHost(t)
	service := newTestService(t, signer, host, nil)
	emitter := newRecordingEmitter()
	link := service.Bind(testHotkey, emitter)
	defer link.CloseAll()
	now := time.Now()
	open := signer.intent(t, now, func(intent *contracts.StorageProbeRelayIntent) {
		intent.ExpiresAt = now.Add(700 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z")
	})
	link.Open(open)
	if message := emitter.next(t); message.kind != "opened" {
		t.Fatalf("relay not opened: %+v", message)
	}
	storage := host.accept(t)
	emitter.expectClose(t, contracts.StorageProbeRelayTimeout)
	_ = storage.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := storage.Read(make([]byte, 1)); err == nil {
		t.Fatal("storage connection stayed open after expiry")
	}
}

func TestRelayCloseFromBeamCoreSendsNoFurtherFrames(t *testing.T) {
	signer := newTestSigner(t)
	host := newStorageHost(t)
	service := newTestService(t, signer, host, nil)
	emitter := newRecordingEmitter()
	link := service.Bind(testHotkey, emitter)
	open := signer.intent(t, time.Now(), nil)
	link.Open(open)
	if message := emitter.next(t); message.kind != "opened" {
		t.Fatalf("relay not opened: %+v", message)
	}
	storage := host.accept(t)
	link.Close(contracts.StorageProbeRelayClose{RelayID: open.RelayID, Reason: contracts.StorageProbeRelayCompleted})
	_ = storage.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := storage.Read(make([]byte, 1)); err == nil {
		t.Fatal("storage connection stayed open after BeamCore close")
	}
	_, _ = storage.Write([]byte("late"))
	emitter.expectSilence(t)

	link.Open(signer.intent(t, time.Now(), nil))
	if message := emitter.next(t); message.kind != "opened" {
		t.Fatalf("relay after a closed relay did not open: %+v", message)
	}
	host.accept(t)
	link.CloseAll()
	emitter.expectSilence(t)
}
