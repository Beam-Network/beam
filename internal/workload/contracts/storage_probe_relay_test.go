package contracts

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// Contract test vector: test-only seed 0x00..0x1f, never deployed.
const (
	storageProbeRelayVectorSeed      = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	storageProbeRelayVectorPublicKey = "A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg"
	storageProbeRelayVectorKeyID     = "56475aa75463474c"
	storageProbeRelayVectorSignature = "8aT13ybEmUj6vAkr-l8TraRbN00CdyIJecH3tr_Iz-0d_tUrjYSp6r_W1s4W6xqZVywlajNKtN1ZUTWHoT2fBQ"
	storageProbeRelayVectorIntent    = `{
		"schema_version": "storage-probe-relay/v1",
		"key_id": "56475aa75463474c",
		"environment": "dev",
		"relay_id": "6f1c2a4e-8b3d-4c5e-9f60-7a8b9c0d1e2f",
		"orchestrator_hotkey": "5F3sa2TJAWMqDhXG6jhV4N8ko9SxwGy8TpaNS1repo5EYjQX",
		"worker_id": "worker-7d1f",
		"host": "example-bucket.s3.us-east-1.amazonaws.com",
		"port": 443,
		"issued_at": "2026-10-01T12:00:00.000Z",
		"expires_at": "2026-10-01T12:00:45.000Z",
		"max_bytes_up": 65536,
		"max_bytes_down": 65536,
		"max_frame_bytes": 16384,
		"nonce": "AAECAwQFBgcICQoLDA0ODw"
	}`
)

func TestStorageProbeRelayContractTestVector(t *testing.T) {
	seed, err := base64.RawURLEncoding.DecodeString(storageProbeRelayVectorSeed)
	if err != nil {
		t.Fatal(err)
	}
	derived := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if got := base64.RawURLEncoding.EncodeToString(derived); got != storageProbeRelayVectorPublicKey {
		t.Fatalf("public key=%s", got)
	}
	publicKey, err := ParseStorageProbeRelayPublicKey(storageProbeRelayVectorPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := StorageProbeRelayKeyID(publicKey); got != storageProbeRelayVectorKeyID {
		t.Fatalf("key_id=%s", got)
	}
	intent, err := ParseStorageProbeRelayIntent(json.RawMessage(storageProbeRelayVectorIntent))
	if err != nil {
		t.Fatal(err)
	}
	if err := intent.ValidateFields(); err != nil {
		t.Fatal(err)
	}
	if got := len(intent.CanonicalMessage()); got != 313 {
		t.Fatalf("canonical message is %d bytes, want 313", got)
	}
	if !VerifyStorageProbeRelayIntent(publicKey, intent, storageProbeRelayVectorSignature) {
		t.Fatal("contract test vector signature did not verify")
	}
	signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), intent.CanonicalMessage()))
	if signature != storageProbeRelayVectorSignature {
		t.Fatalf("re-signed vector differs: %s", signature)
	}

	tampered := intent
	tampered.Host = "other-bucket.s3.us-east-1.amazonaws.com"
	if VerifyStorageProbeRelayIntent(publicKey, tampered, storageProbeRelayVectorSignature) {
		t.Fatal("tampered host verified")
	}
	tampered = intent
	tampered.MaxBytesDown = 65535
	if VerifyStorageProbeRelayIntent(publicKey, tampered, storageProbeRelayVectorSignature) {
		t.Fatal("tampered cap verified")
	}
	otherPublic, _, _ := ed25519.GenerateKey(nil)
	if VerifyStorageProbeRelayIntent(otherPublic, intent, storageProbeRelayVectorSignature) {
		t.Fatal("signature verified with a key whose key_id differs")
	}
	if VerifyStorageProbeRelayIntent(publicKey, intent, storageProbeRelayVectorSignature+"A") ||
		VerifyStorageProbeRelayIntent(publicKey, intent, strings.Repeat("A", 86)) {
		t.Fatal("malformed signature verified")
	}
}

func TestStorageProbeRelayIntentDecodingIsStrict(t *testing.T) {
	var fields map[string]any
	if err := json.Unmarshal([]byte(storageProbeRelayVectorIntent), &fields); err != nil {
		t.Fatal(err)
	}
	mutate := func(change func(map[string]any)) json.RawMessage {
		copied := make(map[string]any, len(fields))
		for key, value := range fields {
			copied[key] = value
		}
		change(copied)
		encoded, err := json.Marshal(copied)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	for name, raw := range map[string]json.RawMessage{
		"unknown field":   mutate(func(value map[string]any) { value["extra"] = "x" }),
		"missing field":   mutate(func(value map[string]any) { delete(value, "nonce") }),
		"null field":      mutate(func(value map[string]any) { value["port"] = nil }),
		"string integer":  mutate(func(value map[string]any) { value["port"] = "443" }),
		"fractional caps": mutate(func(value map[string]any) { value["max_bytes_up"] = 1.5 }),
	} {
		if _, err := ParseStorageProbeRelayIntent(raw); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	for name, change := range map[string]func(*StorageProbeRelayIntent){
		"schema":       func(intent *StorageProbeRelayIntent) { intent.SchemaVersion = "storage-probe-relay/v2" },
		"relay id":     func(intent *StorageProbeRelayIntent) { intent.RelayID = strings.ToUpper(intent.RelayID) },
		"nonce":        func(intent *StorageProbeRelayIntent) { intent.Nonce = "short" },
		"frame cap":    func(intent *StorageProbeRelayIntent) { intent.MaxFrameBytes = StorageProbeRelayMaxFrameBytes + 1 },
		"byte cap":     func(intent *StorageProbeRelayIntent) { intent.MaxBytesUp = 0 },
		"timestamp":    func(intent *StorageProbeRelayIntent) { intent.IssuedAt = "2026-10-01T12:00:00Z" },
		"worker space": func(intent *StorageProbeRelayIntent) { intent.WorkerID = "worker 1" },
	} {
		intent, err := ParseStorageProbeRelayIntent(json.RawMessage(storageProbeRelayVectorIntent))
		if err != nil {
			t.Fatal(err)
		}
		change(&intent)
		if intent.ValidateFields() == nil {
			t.Fatalf("%s violation was accepted", name)
		}
	}
}

func TestStorageProbeRelayIntentTimeWindow(t *testing.T) {
	intent, err := ParseStorageProbeRelayIntent(json.RawMessage(storageProbeRelayVectorIntent))
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := intent.CheckTime(issuedAt.Add(10 * time.Second)); err != nil {
		t.Fatalf("current intent rejected: %v", err)
	}
	if err := intent.CheckTime(issuedAt.Add(-29 * time.Second)); err != nil {
		t.Fatalf("intent within the clock skew rejected: %v", err)
	}
	if intent.CheckTime(issuedAt.Add(-31*time.Second)) == nil {
		t.Fatal("intent issued beyond the clock skew was accepted")
	}
	if intent.CheckTime(issuedAt.Add(45*time.Second)) == nil {
		t.Fatal("intent was accepted at expires_at")
	}
	intent.ExpiresAt = "2026-10-01T12:01:00.001Z"
	if intent.CheckTime(issuedAt.Add(time.Second)) == nil {
		t.Fatal("lifetime above 60s was accepted")
	}
	intent.ExpiresAt = intent.IssuedAt
	if intent.CheckTime(issuedAt.Add(-time.Second)) == nil {
		t.Fatal("zero lifetime was accepted")
	}
}

func TestStorageProbeRelayHostnameRules(t *testing.T) {
	for _, host := range []string{"example-bucket.s3.us-east-1.amazonaws.com", "storage.example.com", "a.b", "x1.example.io"} {
		if !IsStorageProbeRelayHostname(host) {
			t.Fatalf("%s rejected", host)
		}
	}
	for _, host := range []string{
		"", "localhost", "Storage.example.com", "203.0.113.7", "::1", "[::1]", "[2001:db8::1]", "fe80::1%eth0",
		"10.0.0.1", "example.123", "-bad.example.com", "bad-.example.com", "under_score.example.com",
		"trailing.example.com.", "example..com", "storage.example.com:443", strings.Repeat("a", 64) + ".example.com",
	} {
		if IsStorageProbeRelayHostname(host) {
			t.Fatalf("%q accepted", host)
		}
	}
}

func TestStorageProbeRelayAddressVetting(t *testing.T) {
	for _, address := range []string{"8.8.8.8", "52.216.0.1", "2606:4700::1111", "64:ff9b::808:808", "::ffff:8.8.8.8"} {
		if !StorageProbeRelayAddressAllowed(netip.MustParseAddr(address)) {
			t.Fatalf("public address %s rejected", address)
		}
	}
	for _, address := range []string{
		"0.0.0.0", "0.1.2.3", "127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "100.127.255.254", "192.0.0.8", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "239.255.255.250", "240.0.0.1", "255.255.255.255",
		"::", "::1", "fc00::1", "fd12:3456::1", "fe80::1", "ff02::1", "2001:db8::1", "2001::1", "2002:7f00:1::1",
		"3fff::1", "fec0::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::127.0.0.1",
		"64:ff9b::7f00:1", "64:ff9b::a00:1", "64:ff9b:1::1", "::ffff:0:a00:1", "100::1",
	} {
		if StorageProbeRelayAddressAllowed(netip.MustParseAddr(address)) {
			t.Fatalf("non-public address %s accepted", address)
		}
	}
}

func TestStorageProbeRelayFrameDecoding(t *testing.T) {
	if data, err := DecodeStorageProbeRelayData("aGVsbG8=", 16); err != nil || string(data) != "hello" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	for _, encoded := range []string{"", "aGVsbG8", "aGVsbG8-", "aGVsbG9=", base64.StdEncoding.EncodeToString(make([]byte, 17))} {
		if _, err := DecodeStorageProbeRelayData(encoded, 16); err == nil {
			t.Fatalf("frame %q accepted", encoded)
		}
	}
}

func TestAdvertisesCapabilityProtocolIgnoresCapacity(t *testing.T) {
	manifest := NewWorkerCapabilityManifest("worker-1", "test", []string{StorageProbeRelayCapability}, 1, 0, time.Now())
	if !AdvertisesCapabilityProtocol(manifest, StorageProbeRelayCapability) {
		t.Fatal("relay capability with protocol v1 was not recognized")
	}
	if SupportsCapability(manifest, StorageProbeRelayCapability) {
		t.Fatal("capacity-gated support ignored zero capacity")
	}
	manifest.Protocols = nil
	if AdvertisesCapabilityProtocol(manifest, StorageProbeRelayCapability) {
		t.Fatal("capability without its protocol range was recognized")
	}
}
