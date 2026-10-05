package contracts

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Storage probe relay (`storage.probe.relay.v2`): BeamCore opens a short-lived
// relay through an orchestrator to one of its workers, which splices raw bytes
// to the storage host and port named by the intent. BeamCore runs TLS end to
// end over the relay, so participants only forward ciphertext. Every limit
// comes from a BeamCore-signed intent that the worker verifies before dialing.
const (
	StorageProbeRelayCapability      = "storage.probe.relay.v2"
	StorageProbeRelayProtocolVersion = 2
	StorageProbeRelayIntentSchema    = "storage-probe-relay/v2"
	StorageProbeRelaySignatureDomain = "beam:storage-probe-relay-intent/v2"
	StorageProbeRelayMaxTTL          = 60 * time.Second
	StorageProbeRelayMaxBytes        = 65_536
	StorageProbeRelayMaxFrameBytes   = 16_384
	StorageProbeRelayClockSkew       = 30 * time.Second
)

// BeamCore control message types. Data and close travel in both directions.
const (
	StorageProbeRelayOpenType   = "storage_probe_relay_open"
	StorageProbeRelayOpenedType = "storage_probe_relay_opened"
	StorageProbeRelayDataType   = "storage_probe_relay_data"
	StorageProbeRelayCloseType  = "storage_probe_relay_close"
)

// Close reasons.
const (
	StorageProbeRelayCompleted         = "completed"
	StorageProbeRelayEOF               = "eof"
	StorageProbeRelayTimeout           = "timeout"
	StorageProbeRelayCancelled         = "cancelled"
	StorageProbeRelayProtocolError     = "protocol_error"
	StorageProbeRelayIntentInvalid     = "intent_invalid"
	StorageProbeRelayIntentExpired     = "intent_expired"
	StorageProbeRelayIntentReplayed    = "intent_replayed"
	StorageProbeRelayTargetNotAllowed  = "target_not_allowed"
	StorageProbeRelayDNSFailed         = "dns_failed"
	StorageProbeRelayDialFailed        = "dial_failed"
	StorageProbeRelayByteCapReached    = "byte_cap_reached"
	StorageProbeRelayWorkerUnavailable = "worker_unavailable"
	StorageProbeRelayUnsupported       = "relay_unsupported"
	StorageProbeRelayCapacityExhausted = "capacity_exhausted"
	StorageProbeRelayIOError           = "io_error"
)

// StorageProbeRelayOpen asks a worker to open one relay. The intent is carried
// verbatim so the worker verifies exactly what BeamCore signed.
type StorageProbeRelayOpen struct {
	RelayID   string          `json:"relay_id"`
	Intent    json.RawMessage `json:"intent"`
	Signature string          `json:"signature"`
}

type StorageProbeRelayOpened struct {
	RelayID string `json:"relay_id"`
}

// StorageProbeRelayData carries 1..max_frame_bytes relayed bytes as standard
// base64 with padding; seq starts at 0 per relay and direction.
type StorageProbeRelayData struct {
	RelayID string `json:"relay_id"`
	Seq     int64  `json:"seq"`
	Data    string `json:"data"`
}

type StorageProbeRelayClose struct {
	RelayID string `json:"relay_id"`
	Reason  string `json:"reason"`
}

// StorageProbeRelayIntent is the BeamCore-signed relay authorization.
type StorageProbeRelayIntent struct {
	SchemaVersion      string `json:"schema_version"`
	KeyID              string `json:"key_id"`
	Environment        string `json:"environment"`
	RelayID            string `json:"relay_id"`
	OrchestratorHotkey string `json:"orchestrator_hotkey"`
	WorkerID           string `json:"worker_id"`
	Host               string `json:"host"`
	Port               int64  `json:"port"`
	IssuedAt           string `json:"issued_at"`
	ExpiresAt          string `json:"expires_at"`
	MaxBytesUp         int64  `json:"max_bytes_up"`
	MaxBytesDown       int64  `json:"max_bytes_down"`
	MaxFrameBytes      int64  `json:"max_frame_bytes"`
	Nonce              string `json:"nonce"`
}

var (
	storageProbeRelayIDPattern        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	storageProbeRelayKeyIDPattern     = regexp.MustCompile(`^[0-9a-f]{16}$`)
	storageProbeRelayEnvPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	storageProbeRelayHotkeyPattern    = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	storageProbeRelayTokenPattern     = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
	storageProbeRelayTimePattern      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
	storageProbeRelayNoncePattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	storageProbeRelaySignaturePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{86}$`)
	storageProbeRelayReasonPattern    = regexp.MustCompile(`^[a-z_]{1,64}$`)
	storageProbeRelayLabelPattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	storageProbeRelayNumericPattern   = regexp.MustCompile(`^[0-9]+$`)
)

var storageProbeRelayIntentFields = []string{
	"schema_version", "key_id", "environment", "relay_id", "orchestrator_hotkey", "worker_id", "host",
	"port", "issued_at", "expires_at", "max_bytes_up", "max_bytes_down", "max_frame_bytes", "nonce",
}

func ValidStorageProbeRelayID(relayID string) bool {
	return storageProbeRelayIDPattern.MatchString(relayID)
}

func ValidStorageProbeRelayReason(reason string) bool {
	return storageProbeRelayReasonPattern.MatchString(reason)
}

// ParseStorageProbeRelayIntent decodes an intent strictly: every field present
// and non-null with its JSON type, and no other field.
func ParseStorageProbeRelayIntent(raw json.RawMessage) (StorageProbeRelayIntent, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return StorageProbeRelayIntent{}, err
	}
	if len(fields) != len(storageProbeRelayIntentFields) {
		return StorageProbeRelayIntent{}, errors.New("storage probe relay intent has missing or unknown fields")
	}
	for _, name := range storageProbeRelayIntentFields {
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return StorageProbeRelayIntent{}, fmt.Errorf("storage probe relay intent field %s is missing", name)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var intent StorageProbeRelayIntent
	if err := decoder.Decode(&intent); err != nil {
		return StorageProbeRelayIntent{}, err
	}
	return intent, nil
}

// CanonicalMessage returns the exact bytes BeamCore signs.
func (intent StorageProbeRelayIntent) CanonicalMessage() []byte {
	fields := []string{
		intent.SchemaVersion, intent.KeyID, intent.Environment, intent.RelayID, intent.OrchestratorHotkey,
		intent.WorkerID, intent.Host, strconv.FormatInt(intent.Port, 10), intent.IssuedAt, intent.ExpiresAt,
		strconv.FormatInt(intent.MaxBytesUp, 10), strconv.FormatInt(intent.MaxBytesDown, 10),
		strconv.FormatInt(intent.MaxFrameBytes, 10), intent.Nonce,
	}
	return []byte(StorageProbeRelaySignatureDomain + "\x00" + strings.Join(fields, "\n"))
}

// ValidateFields checks the intent's field formats and caps, including the
// host form and the port range. Whether the target's addresses are public and
// the time window are checked separately because they map to their own close
// reasons.
func (intent StorageProbeRelayIntent) ValidateFields() error {
	switch {
	case intent.SchemaVersion != StorageProbeRelayIntentSchema:
		return errors.New("unsupported storage probe relay intent schema")
	case !storageProbeRelayKeyIDPattern.MatchString(intent.KeyID),
		!storageProbeRelayEnvPattern.MatchString(intent.Environment),
		!ValidStorageProbeRelayID(intent.RelayID),
		!storageProbeRelayHotkeyPattern.MatchString(intent.OrchestratorHotkey),
		!storageProbeRelayTokenPattern.MatchString(intent.WorkerID),
		!storageProbeRelayNoncePattern.MatchString(intent.Nonce):
		return errors.New("malformed storage probe relay intent identity")
	case !validStorageProbeRelayHost(intent.Host):
		return errors.New("storage probe relay intent host must be a hostname or a canonical IP literal")
	case intent.Port < 1 || intent.Port > math.MaxUint16:
		return errors.New("storage probe relay intent port is out of range")
	case intent.MaxBytesUp < 1 || intent.MaxBytesUp > StorageProbeRelayMaxBytes,
		intent.MaxBytesDown < 1 || intent.MaxBytesDown > StorageProbeRelayMaxBytes,
		intent.MaxFrameBytes < 1 || intent.MaxFrameBytes > StorageProbeRelayMaxFrameBytes:
		return errors.New("storage probe relay intent caps are out of range")
	}
	if _, _, err := intent.Window(); err != nil {
		return err
	}
	return nil
}

// Window parses issued_at and expires_at.
func (intent StorageProbeRelayIntent) Window() (time.Time, time.Time, error) {
	issuedAt, err := parseStorageProbeRelayTime(intent.IssuedAt)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	expiresAt, err := parseStorageProbeRelayTime(intent.ExpiresAt)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return issuedAt, expiresAt, nil
}

// CheckTime enforces the intent's time window at now.
func (intent StorageProbeRelayIntent) CheckTime(now time.Time) error {
	issuedAt, expiresAt, err := intent.Window()
	if err != nil {
		return err
	}
	lifetime := expiresAt.Sub(issuedAt)
	if lifetime <= 0 || lifetime > StorageProbeRelayMaxTTL {
		return errors.New("storage probe relay lifetime is outside 0..60s")
	}
	if issuedAt.After(now.Add(StorageProbeRelayClockSkew)) || !now.Before(expiresAt) {
		return errors.New("storage probe relay intent is outside its time window")
	}
	return nil
}

func parseStorageProbeRelayTime(value string) (time.Time, error) {
	if !storageProbeRelayTimePattern.MatchString(value) {
		return time.Time{}, errors.New("storage probe relay timestamp must be YYYY-MM-DDTHH:MM:SS.sssZ")
	}
	return time.Parse("2006-01-02T15:04:05.000Z", value)
}

// StorageProbeRelayKeyID is the first 16 lowercase hex characters of SHA-256
// over the raw 32-byte public key.
func StorageProbeRelayKeyID(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return hex.EncodeToString(digest[:])[:16]
}

// ParseStorageProbeRelayPublicKey decodes a base64url (unpadded) raw Ed25519 key.
func ParseStorageProbeRelayPublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("storage probe relay public key must be a base64url 32-byte Ed25519 key")
	}
	return ed25519.PublicKey(raw), nil
}

// VerifyStorageProbeRelayIntent checks the intent's Ed25519 signature with the
// key whose key_id equals intent.key_id.
func VerifyStorageProbeRelayIntent(publicKey ed25519.PublicKey, intent StorageProbeRelayIntent, signature string) bool {
	if len(publicKey) != ed25519.PublicKeySize || StorageProbeRelayKeyID(publicKey) != intent.KeyID ||
		!storageProbeRelaySignaturePattern.MatchString(signature) {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(publicKey, intent.CanonicalMessage(), raw)
}

// validStorageProbeRelayHost accepts an intent host: a lowercase DNS hostname
// or a canonical IP literal.
func validStorageProbeRelayHost(host string) bool {
	_, literal := StorageProbeRelayIPLiteral(host)
	return literal || IsStorageProbeRelayHostname(host)
}

// StorageProbeRelayIPLiteral returns the address of a host written as a
// canonical IP literal: IPv4 dotted decimal without leading zeros, or IPv6 in
// lowercase RFC 5952 compressed form without brackets or zone. IPv4-mapped
// IPv6 is never a literal target: BeamCore sends such a host as its IPv4
// literal.
func StorageProbeRelayIPLiteral(host string) (netip.Addr, bool) {
	address, err := netip.ParseAddr(host)
	if err != nil || address.Zone() != "" || address.String() != host || strings.HasPrefix(host, "::ffff:") {
		return netip.Addr{}, false
	}
	return address, true
}

// IsStorageProbeRelayHostname accepts a lowercase DNS hostname with at least
// two labels whose last label is not numeric. IP literals are not hostnames.
func IsStorageProbeRelayHostname(host string) bool {
	if host == "" || len(host) > 253 || host != strings.ToLower(host) {
		return false
	}
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 || storageProbeRelayNumericPattern.MatchString(labels[len(labels)-1]) {
		return false
	}
	for _, label := range labels {
		if !storageProbeRelayLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

// Address ranges that are never relay targets beyond what netip classifies:
// shared, reserved, benchmark, documentation and translation ranges.
var storageProbeRelayBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("::ffff:0:0:0/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

var storageProbeRelayNAT64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// StorageProbeRelayAddressAllowed reports whether a target address, resolved or
// an IP literal, is a public unicast address. IPv4-mapped and NAT64-translated
// addresses are judged by the IPv4 address they carry.
func StorageProbeRelayAddressAllowed(address netip.Addr) bool {
	address = address.Unmap()
	if storageProbeRelayNAT64Prefix.Contains(address) {
		embedded := address.As16()
		address = netip.AddrFrom4([4]byte{embedded[12], embedded[13], embedded[14], embedded[15]})
	}
	if !address.IsValid() || address.Zone() != "" || !address.IsGlobalUnicast() || address.IsPrivate() {
		return false
	}
	for _, prefix := range storageProbeRelayBlockedPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

// DecodeStorageProbeRelayData decodes one frame's standard padded base64
// payload and enforces 1..maxFrameBytes decoded bytes.
func DecodeStorageProbeRelayData(encoded string, maxFrameBytes int64) ([]byte, error) {
	if encoded == "" || int64(len(encoded)) > (maxFrameBytes+2)/3*4 {
		return nil, errors.New("storage probe relay frame size is invalid")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || int64(len(data)) > maxFrameBytes {
		return nil, errors.New("storage probe relay frame data is invalid")
	}
	return data, nil
}

// AdvertisesCapabilityProtocol reports whether a manifest lists the capability
// together with its current protocol version, regardless of capacity.
func AdvertisesCapabilityProtocol(manifest CapabilityManifest, capability string) bool {
	capability = strings.TrimSpace(capability)
	if capability == "" {
		return false
	}
	foundCapability := false
	for _, advertised := range manifest.Capabilities {
		if advertised == capability {
			foundCapability = true
			break
		}
	}
	if !foundCapability {
		return false
	}
	version := CapabilityProtocolVersion(capability)
	for _, protocol := range manifest.Protocols {
		if protocol.Name == capability && protocol.Min <= version && protocol.Max >= version {
			return true
		}
	}
	return false
}
