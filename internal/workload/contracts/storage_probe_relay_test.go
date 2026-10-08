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

// Contract test vectors: test-only seed 0x00..0x1f, never deployed. The
// hostname vector signs a 313-byte message, the IP-literal vector (the same
// intent addressed to 2606:4700:4700::1111 on port 9443) a 293-byte message.
// Both match the vectors of the BeamCore relay contract.
const (
	storageProbeRelayVectorSeed             = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	storageProbeRelayVectorPublicKey        = "A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg"
	storageProbeRelayVectorKeyID            = "56475aa75463474c"
	storageProbeRelayVectorSignature        = "XpeeggB4jEPxb7r5qD1zQthVOBhRZxTQPBgHN9K1qL-ztwV3R_GcugRotH8knlcWuu0fvH2dTsqE3E16nddoDw"
	storageProbeRelayLiteralVectorSignature = "0y9syYovFinfeXrKvYGcspgWzKzEWXN2ojnOTNYbPYXp4QFeWikyCkvTX9nqfdiUdFv2BoJNeokLuPduauNtDA"
	storageProbeRelayVectorIntent           = `{
		"schema_version": "storage-probe-relay/v2",
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

var storageProbeRelayLiteralVectorIntent = strings.NewReplacer(
	`"example-bucket.s3.us-east-1.amazonaws.com"`, `"2606:4700:4700::1111"`, `"port": 443`, `"port": 9443`,
).Replace(storageProbeRelayVectorIntent)

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
	for _, vector := range []struct {
		name, intent, signature string
		messageBytes            int
	}{
		{"hostname", storageProbeRelayVectorIntent, storageProbeRelayVectorSignature, 313},
		{"IP literal", storageProbeRelayLiteralVectorIntent, storageProbeRelayLiteralVectorSignature, 293},
	} {
		intent, err := ParseStorageProbeRelayIntent(json.RawMessage(vector.intent))
		if err != nil {
			t.Fatal(err)
		}
		if err := intent.ValidateFields(); err != nil {
			t.Fatalf("%s vector: %v", vector.name, err)
		}
		message := intent.CanonicalMessage()
		if !strings.HasPrefix(string(message), "beam:storage-probe-relay-intent/v2\x00storage-probe-relay/v2\n") {
			t.Fatalf("%s vector is not signed under the v2 domain: %q", vector.name, message)
		}
		if len(message) != vector.messageBytes {
			t.Fatalf("%s vector canonical message is %d bytes, want %d", vector.name, len(message), vector.messageBytes)
		}
		if !VerifyStorageProbeRelayIntent(publicKey, intent, vector.signature) {
			t.Fatalf("%s vector signature did not verify", vector.name)
		}
		signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), message))
		if signature != vector.signature {
			t.Fatalf("re-signed %s vector differs: %s", vector.name, signature)
		}
	}

	intent, err := ParseStorageProbeRelayIntent(json.RawMessage(storageProbeRelayVectorIntent))
	if err != nil {
		t.Fatal(err)
	}
	tampered := intent
	tampered.Host = "other-bucket.s3.us-east-1.amazonaws.com"
	if VerifyStorageProbeRelayIntent(publicKey, tampered, storageProbeRelayVectorSignature) {
		t.Fatal("tampered host verified")
	}
	tampered = intent
	tampered.Port = 8443
	if VerifyStorageProbeRelayIntent(publicKey, tampered, storageProbeRelayVectorSignature) {
		t.Fatal("tampered port verified")
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

// The v1 contract vector (schema storage-probe-relay/v1, signed under the v1
// domain) is refused: its schema is unsupported and its signature does not
// verify under the v2 domain.
func TestStorageProbeRelayRejectsV1Intents(t *testing.T) {
	const v1Signature = "8aT13ybEmUj6vAkr-l8TraRbN00CdyIJecH3tr_Iz-0d_tUrjYSp6r_W1s4W6xqZVywlajNKtN1ZUTWHoT2fBQ"
	v1Intent := strings.Replace(storageProbeRelayVectorIntent, "storage-probe-relay/v2", "storage-probe-relay/v1", 1)
	intent, err := ParseStorageProbeRelayIntent(json.RawMessage(v1Intent))
	if err != nil {
		t.Fatal(err)
	}
	if intent.ValidateFields() == nil {
		t.Fatal("v1 intent schema was accepted")
	}
	publicKey, err := ParseStorageProbeRelayPublicKey(storageProbeRelayVectorPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if VerifyStorageProbeRelayIntent(publicKey, intent, v1Signature) {
		t.Fatal("v1 signature verified under the v2 domain")
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
		"v1 schema":       func(intent *StorageProbeRelayIntent) { intent.SchemaVersion = "storage-probe-relay/v1" },
		"relay id":        func(intent *StorageProbeRelayIntent) { intent.RelayID = strings.ToUpper(intent.RelayID) },
		"nonce":           func(intent *StorageProbeRelayIntent) { intent.Nonce = "short" },
		"frame cap":       func(intent *StorageProbeRelayIntent) { intent.MaxFrameBytes = StorageProbeRelayMaxFrameBytes + 1 },
		"byte cap":        func(intent *StorageProbeRelayIntent) { intent.MaxBytesUp = 0 },
		"timestamp":       func(intent *StorageProbeRelayIntent) { intent.IssuedAt = "2026-10-01T12:00:00Z" },
		"worker space":    func(intent *StorageProbeRelayIntent) { intent.WorkerID = "worker 1" },
		"port zero":       func(intent *StorageProbeRelayIntent) { intent.Port = 0 },
		"negative port":   func(intent *StorageProbeRelayIntent) { intent.Port = -443 },
		"port above max":  func(intent *StorageProbeRelayIntent) { intent.Port = 65536 },
		"uppercase host":  func(intent *StorageProbeRelayIntent) { intent.Host = "Storage.example.com" },
		"bracketed IPv6":  func(intent *StorageProbeRelayIntent) { intent.Host = "[2606:4700::1111]" },
		"host with port":  func(intent *StorageProbeRelayIntent) { intent.Host = "storage.example.com:9000" },
		"leading zero IP": func(intent *StorageProbeRelayIntent) { intent.Host = "52.216.0.010" },
		"mapped IPv6":     func(intent *StorageProbeRelayIntent) { intent.Host = "::ffff:8.8.8.8" },
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

func TestStorageProbeRelayHostAcceptsOnlyCanonicalIPLiterals(t *testing.T) {
	for _, host := range []string{
		"52.216.0.10", "8.8.8.8", "0.0.0.0", "10.0.0.1", "2606:4700::1111", "2001:db8::1", "2001:db8:0:1:1:1:1:1",
		"2001:db8::1:0:0:1", "::", "::1", "fe80::1", "64:ff9b::7f00:1", "64:ff9b::808:808",
	} {
		address, literal := StorageProbeRelayIPLiteral(host)
		if !literal || address.String() != host || !validStorageProbeRelayHost(host) {
			t.Fatalf("canonical literal %q rejected", host)
		}
	}
	for _, host := range []string{
		"052.216.0.10", "52.216.0.010", "52.216.0", "52.216.0.10.", "2606:4700:0:0:0:0:0:1111", "2606:4700::0:1111",
		"2606:4700:0000::1111", "2606:4700::ABCD", "[2606:4700::1111]", "2001:db8:0:0:1::1", "2001:db8::0:1",
		"0:0:0:0:0:0:0:1", "fe80::1%eth0", "::ffff:808:808", "::8.8.8.8", "64:ff9b::8.8.8.8", " 8.8.8.8",
		"::ffff:8.8.8.8", "::ffff:127.0.0.1", "::ffff:0:808:808",
	} {
		if _, literal := StorageProbeRelayIPLiteral(host); literal {
			t.Fatalf("non-canonical literal %q accepted", host)
		}
		if validStorageProbeRelayHost(host) {
			t.Fatalf("non-canonical literal %q accepted as an intent host", host)
		}
	}
	if _, literal := StorageProbeRelayIPLiteral("storage.example.com"); literal || !validStorageProbeRelayHost("storage.example.com") {
		t.Fatal("hostname was classified as an IP literal")
	}
}

func TestPublicAddressAllowed(t *testing.T) {
	for _, address := range []string{"8.8.8.8", "52.216.0.1", "2606:4700::1111", "64:ff9b::808:808", "::ffff:8.8.8.8"} {
		if !PublicAddressAllowed(netip.MustParseAddr(address)) {
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
		if PublicAddressAllowed(netip.MustParseAddr(address)) {
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
	if StorageProbeRelayCapability != "storage.probe.relay.v2" || len(manifest.Protocols) != 1 ||
		manifest.Protocols[0] != (ProtocolRange{Name: "storage.probe.relay.v2", Min: 2, Max: 2}) {
		t.Fatalf("relay must advertise storage.probe.relay.v2 with protocol 2..2: %+v", manifest)
	}
	if !AdvertisesCapabilityProtocol(manifest, StorageProbeRelayCapability) {
		t.Fatal("relay capability with protocol v2 was not recognized")
	}
	if SupportsCapability(manifest, StorageProbeRelayCapability) {
		t.Fatal("capacity-gated support ignored zero capacity")
	}
	manifest.Protocols = []ProtocolRange{{Name: StorageProbeRelayCapability, Min: 1, Max: 1}}
	if AdvertisesCapabilityProtocol(manifest, StorageProbeRelayCapability) {
		t.Fatal("relay capability with protocol v1 was recognized")
	}
	manifest.Protocols = nil
	if AdvertisesCapabilityProtocol(manifest, StorageProbeRelayCapability) {
		t.Fatal("capability without its protocol range was recognized")
	}
	v1 := NewWorkerCapabilityManifest("worker-1", "test", []string{"storage.probe.relay.v1"}, 1, 1, time.Now())
	if AdvertisesCapabilityProtocol(v1, StorageProbeRelayCapability) {
		t.Fatal("a storage.probe.relay.v1 manifest was recognized as relay-capable")
	}
}
