package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/wcp"
	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	relayVectorHotkey  = "5F3sa2TJAWMqDhXG6jhV4N8ko9SxwGy8TpaNS1repo5EYjQX"
	relayVectorRelayID = "6f1c2a4e-8b3d-4c5e-9f60-7a8b9c0d1e2f"
	relayVectorKey     = "A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg"
	relayVectorSig     = "8aT13ybEmUj6vAkr-l8TraRbN00CdyIJecH3tr_Iz-0d_tUrjYSp6r_W1s4W6xqZVywlajNKtN1ZUTWHoT2fBQ"
)

func relayVectorIntent() map[string]any {
	return map[string]any{
		"schema_version": "storage-probe-relay/v1", "key_id": "56475aa75463474c", "environment": "dev",
		"relay_id": relayVectorRelayID, "orchestrator_hotkey": relayVectorHotkey, "worker_id": "worker-7d1f",
		"host": "example-bucket.s3.us-east-1.amazonaws.com", "port": 443,
		"issued_at": "2026-10-01T12:00:00.000Z", "expires_at": "2026-10-01T12:00:45.000Z",
		"max_bytes_up": 65536, "max_bytes_down": 65536, "max_frame_bytes": 16384, "nonce": "AAECAwQFBgcICQoLDA0ODw",
	}
}

var relayVectorNow = time.Date(2026, 10, 1, 12, 0, 10, 0, time.UTC)

func beamCorePayload(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	encoded, err := msgpack.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type fakeRelayOpen struct {
	workerID  string
	open      contracts.StorageProbeRelayOpen
	expiresAt time.Time
}

type fakeRelayClose struct {
	request contracts.StorageProbeRelayClose
	notify  bool
}

type fakeRelayLink struct {
	mu         sync.Mutex
	available  bool
	openReason string
	sink       func(wcp.StorageProbeRelayEvent)
	opens      []fakeRelayOpen
	data       []contracts.StorageProbeRelayData
	closes     []fakeRelayClose
}

func (link *fakeRelayLink) StorageProbeRelayAvailable() bool {
	link.mu.Lock()
	defer link.mu.Unlock()
	return link.available
}

func (link *fakeRelayLink) SetStorageProbeRelaySink(sink func(wcp.StorageProbeRelayEvent)) {
	link.mu.Lock()
	defer link.mu.Unlock()
	link.sink = sink
}

func (link *fakeRelayLink) OpenStorageProbeRelay(workerID string, open contracts.StorageProbeRelayOpen, expiresAt time.Time) string {
	link.mu.Lock()
	defer link.mu.Unlock()
	link.opens = append(link.opens, fakeRelayOpen{workerID: workerID, open: open, expiresAt: expiresAt})
	return link.openReason
}

func (link *fakeRelayLink) ForwardStorageProbeRelayData(frame contracts.StorageProbeRelayData) {
	link.mu.Lock()
	defer link.mu.Unlock()
	link.data = append(link.data, frame)
}

func (link *fakeRelayLink) CloseStorageProbeRelay(request contracts.StorageProbeRelayClose, notify bool) {
	link.mu.Lock()
	defer link.mu.Unlock()
	link.closes = append(link.closes, fakeRelayClose{request: request, notify: notify})
}

func newRelayControl(t *testing.T, link *fakeRelayLink) (*roomControl, *fakeRoomControlConnection) {
	t.Helper()
	orchestratorRegistry, err := registry.New(orchestratordomain.Orchestrator{OrchestratorID: "orch-1", CreatedAt: relayVectorNow})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := dispatch.NewService(dispatch.Config{OrchestratorID: "orch-1"}, orchestratorRegistry, fakeControlWCP{}, dispatch.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	connection := &fakeRoomControlConnection{}
	control := newRoomControl(NATSConfig{Environment: "dev", Hotkey: relayVectorHotkey, GatewayURL: "https://orchestrator.test",
		RequestTimeout: time.Second}, nil, nil, nil, tasks)
	control.conn = connection
	control.relays = link
	return control, connection
}

func TestStorageProbeRelayCapabilityFollowsConnectedWorkers(t *testing.T) {
	link := &fakeRelayLink{}
	control, _ := newRelayControl(t, link)
	if contracts.AdvertisesCapabilityProtocol(control.capabilityManifest(relayVectorNow), contracts.StorageProbeRelayCapability) {
		t.Fatal("relay capability advertised without a relay-capable worker")
	}
	link.available = true
	manifest := control.capabilityManifest(relayVectorNow)
	if !contracts.AdvertisesCapabilityProtocol(manifest, contracts.StorageProbeRelayCapability) {
		t.Fatalf("relay capability missing: %+v", manifest)
	}
	found := false
	for _, protocol := range manifest.Protocols {
		if protocol.Name == contracts.StorageProbeRelayCapability {
			found = protocol.Min == 1 && protocol.Max == 1
		}
	}
	if !found || manifest.Capacity.AvailableConnections != 0 {
		t.Fatalf("relay must advertise protocol v1 without adding room capacity: %+v", manifest)
	}
}

func TestStorageProbeRelayBindSubscribesRuntimeRelaySubjects(t *testing.T) {
	control, connection := newRelayControl(t, &fakeRelayLink{})
	session, err := control.bind(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	prefix := "beam.orch.control.v2.dev.runtime.5f3sa2tjawmqdhxg6jhv4n8ko9sxwgy8tpans1repo5eyjqx."
	for _, messageType := range []string{"storage_probe_relay_open", "storage_probe_relay_data", "storage_probe_relay_close"} {
		subscribed := false
		for _, subject := range connection.subjects {
			subscribed = subscribed || subject == prefix+messageType
		}
		if !subscribed {
			t.Fatalf("missing subscription %s%s in %v", prefix, messageType, connection.subjects)
		}
	}
}

func TestStorageProbeRelayOpenForwardsTheSignedIntentUnchanged(t *testing.T) {
	link := &fakeRelayLink{}
	control, connection := newRelayControl(t, link)
	payload := beamCorePayload(t, map[string]any{"type": "storage_probe_relay_open", "relay_id": relayVectorRelayID,
		"intent": relayVectorIntent(), "signature": relayVectorSig})
	if err := control.handleStorageProbeRelayMessage("storage_probe_relay_open", payload, relayVectorNow); err != nil {
		t.Fatal(err)
	}
	if len(link.opens) != 1 || len(connection.publishedMessages()) != 0 {
		t.Fatalf("opens=%+v published=%+v", link.opens, connection.publishedMessages())
	}
	forwarded := link.opens[0]
	if forwarded.workerID != "worker-7d1f" || forwarded.open.RelayID != relayVectorRelayID || forwarded.open.Signature != relayVectorSig ||
		!forwarded.expiresAt.Equal(time.Date(2026, 10, 1, 12, 0, 45, 0, time.UTC)) {
		t.Fatalf("unexpected forwarded open: %+v", forwarded)
	}
	intent, err := contracts.ParseStorageProbeRelayIntent(forwarded.open.Intent)
	if err != nil {
		t.Fatal(err)
	}
	key, err := contracts.ParseStorageProbeRelayPublicKey(relayVectorKey)
	if err != nil {
		t.Fatal(err)
	}
	if !contracts.VerifyStorageProbeRelayIntent(key, intent, forwarded.open.Signature) {
		t.Fatal("forwarded intent no longer verifies: values changed in transit")
	}
}

func TestStorageProbeRelayOpenRefusalsArePublishedAsCloses(t *testing.T) {
	tests := []struct {
		name       string
		payload    func() map[string]any
		now        time.Time
		linkReason string
		reason     string
		linkCalled bool
	}{
		{name: "worker unavailable", now: relayVectorNow, linkReason: contracts.StorageProbeRelayWorkerUnavailable,
			reason: contracts.StorageProbeRelayWorkerUnavailable, linkCalled: true},
		{name: "relay unsupported", now: relayVectorNow, linkReason: contracts.StorageProbeRelayUnsupported,
			reason: contracts.StorageProbeRelayUnsupported, linkCalled: true},
		{name: "expired", now: relayVectorNow.Add(time.Minute), reason: contracts.StorageProbeRelayIntentExpired},
		{name: "unknown open field", now: relayVectorNow, reason: contracts.StorageProbeRelayIntentInvalid,
			payload: func() map[string]any {
				return map[string]any{"type": "storage_probe_relay_open", "relay_id": relayVectorRelayID,
					"intent": relayVectorIntent(), "signature": relayVectorSig, "extra": true}
			}},
		{name: "unknown intent field", now: relayVectorNow, reason: contracts.StorageProbeRelayIntentInvalid,
			payload: func() map[string]any {
				intent := relayVectorIntent()
				intent["extra"] = "x"
				return map[string]any{"type": "storage_probe_relay_open", "relay_id": relayVectorRelayID,
					"intent": intent, "signature": relayVectorSig}
			}},
		{name: "relay id mismatch", now: relayVectorNow, reason: contracts.StorageProbeRelayIntentInvalid,
			payload: func() map[string]any {
				intent := relayVectorIntent()
				intent["relay_id"] = "00000000-0000-4000-8000-000000000000"
				return map[string]any{"type": "storage_probe_relay_open", "relay_id": relayVectorRelayID,
					"intent": intent, "signature": relayVectorSig}
			}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			link := &fakeRelayLink{openReason: test.linkReason}
			control, connection := newRelayControl(t, link)
			payload := map[string]any{"type": "storage_probe_relay_open", "relay_id": relayVectorRelayID,
				"intent": relayVectorIntent(), "signature": relayVectorSig}
			if test.payload != nil {
				payload = test.payload()
			}
			if err := control.handleStorageProbeRelayMessage("storage_probe_relay_open", beamCorePayload(t, payload), test.now); err != nil {
				t.Fatal(err)
			}
			if (len(link.opens) == 1) != test.linkCalled {
				t.Fatalf("link opens=%d, want called=%t", len(link.opens), test.linkCalled)
			}
			published := connection.publishedMessages()
			if len(published) != 1 {
				t.Fatalf("published=%+v", published)
			}
			want := "beam.orch.control.v2.dev.orch.5f3sa2tjawmqdhxg6jhv4n8ko9sxwgy8tpans1repo5eyjqx.storage_probe_relay_close"
			message := published[0]
			if message.subject != want || len(message.payload) != 3 || message.payload["type"] != "storage_probe_relay_close" ||
				message.payload["relay_id"] != relayVectorRelayID || message.payload["reason"] != test.reason {
				t.Fatalf("unexpected close: %+v", message)
			}
		})
	}
}

func TestStorageProbeRelayFramesAndClosesFromBeamCore(t *testing.T) {
	link := &fakeRelayLink{}
	control, connection := newRelayControl(t, link)
	data := base64.StdEncoding.EncodeToString([]byte("tls-record"))
	frame := beamCorePayload(t, map[string]any{"type": "storage_probe_relay_data", "relay_id": relayVectorRelayID, "seq": 3, "data": data})
	if err := control.handleStorageProbeRelayMessage("storage_probe_relay_data", frame, relayVectorNow); err != nil {
		t.Fatal(err)
	}
	if len(link.data) != 1 || link.data[0] != (contracts.StorageProbeRelayData{RelayID: relayVectorRelayID, Seq: 3, Data: data}) {
		t.Fatalf("forwarded data=%+v", link.data)
	}
	malformed := beamCorePayload(t, map[string]any{"type": "storage_probe_relay_data", "relay_id": relayVectorRelayID,
		"seq": 4, "data": data, "extra": 1})
	if err := control.handleStorageProbeRelayMessage("storage_probe_relay_data", malformed, relayVectorNow); err == nil {
		t.Fatal("malformed frame was accepted")
	}
	closeFrame := beamCorePayload(t, map[string]any{"type": "storage_probe_relay_close", "relay_id": relayVectorRelayID, "reason": "completed"})
	if err := control.handleStorageProbeRelayMessage("storage_probe_relay_close", closeFrame, relayVectorNow); err != nil {
		t.Fatal(err)
	}
	if len(link.closes) != 2 ||
		link.closes[0] != (fakeRelayClose{request: contracts.StorageProbeRelayClose{RelayID: relayVectorRelayID, Reason: "protocol_error"}, notify: true}) ||
		link.closes[1] != (fakeRelayClose{request: contracts.StorageProbeRelayClose{RelayID: relayVectorRelayID, Reason: "completed"}, notify: false}) {
		t.Fatalf("closes=%+v", link.closes)
	}
	mistyped := beamCorePayload(t, map[string]any{"type": "storage_probe_relay_close", "relay_id": relayVectorRelayID, "seq": 0, "data": data})
	if err := control.handleStorageProbeRelayMessage("storage_probe_relay_data", mistyped, relayVectorNow); err == nil {
		t.Fatal("payload whose type differs from its subject was accepted")
	}
	if len(connection.publishedMessages()) != 0 {
		t.Fatalf("frames from BeamCore must not be answered: %+v", connection.publishedMessages())
	}
}

func TestStorageProbeRelayEventsArePublishedWithoutReply(t *testing.T) {
	control, connection := newRelayControl(t, &fakeRelayLink{})
	control.publishStorageProbeRelayEvent(wcp.StorageProbeRelayEvent{Type: contracts.StorageProbeRelayOpenedType, RelayID: relayVectorRelayID})
	control.publishStorageProbeRelayEvent(wcp.StorageProbeRelayEvent{Type: contracts.StorageProbeRelayDataType, RelayID: relayVectorRelayID,
		Seq: 7, Data: "AQID"})
	published := connection.publishedMessages()
	if len(published) != 2 || len(connection.requests) != 0 {
		t.Fatalf("published=%+v requests=%+v", published, connection.requests)
	}
	prefix := "beam.orch.control.v2.dev.orch.5f3sa2tjawmqdhxg6jhv4n8ko9sxwgy8tpans1repo5eyjqx."
	if published[0].subject != prefix+"storage_probe_relay_opened" || len(published[0].payload) != 2 {
		t.Fatalf("unexpected opened: %+v", published[0])
	}
	seq, ok := msgpackInteger(published[1].payload["seq"])
	if published[1].subject != prefix+"storage_probe_relay_data" || !ok || seq != 7 || published[1].payload["data"] != "AQID" {
		t.Fatalf("unexpected data frame: %+v", published[1])
	}
	if _, isFloat := published[1].payload["seq"].(float64); isFloat {
		t.Fatal("seq must be encoded as a MessagePack integer")
	}
	encoded, _ := json.Marshal(published[1].payload)
	if len(published[1].payload) != 4 {
		t.Fatalf("data frame has extra fields: %s", encoded)
	}
}
