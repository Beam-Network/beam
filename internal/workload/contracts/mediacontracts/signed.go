package mediacontracts

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

const (
	UsageReportSchema     = "beam-media-usage/v1"
	UsageReportV2Schema   = "beam-media-usage/v2"
	SegmentManifestSchema = "beam-segment-manifest/v1"
)

var ErrEnvelope = errors.New("mediacontracts: invalid signed envelope")

type SignedEnvelope struct {
	Schema    string `json:"schema"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

func SignEnvelope(schema string, payload []byte, privateKey ed25519.PrivateKey) SignedEnvelope {
	encoded := B64URL(payload)
	signature := ed25519.Sign(privateKey, []byte(schema+"."+encoded))
	return SignedEnvelope{Schema: schema, Payload: encoded, Signature: B64URL(signature)}
}

func OpenEnvelope(envelope SignedEnvelope, schema string, publicKey []byte) ([]byte, error) {
	if envelope.Schema != schema || len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrEnvelope
	}
	payload, err := DecodeB64URL(envelope.Payload)
	if err != nil {
		return nil, ErrEnvelope
	}
	signature, err := DecodeB64URL(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, ErrEnvelope
	}
	if !ed25519.Verify(publicKey, []byte(schema+"."+envelope.Payload), signature) {
		return nil, ErrEnvelope
	}
	return payload, nil
}

type UsageConnection struct {
	ConnectionID string    `json:"connection_id"`
	TokenNonce   *string   `json:"token_nonce"`
	Identity     *string   `json:"identity"`
	Direction    string    `json:"direction"`
	Transport    string    `json:"transport"`
	Bytes        int64     `json:"bytes"`
	MediaMS      int64     `json:"media_ms"`
	LayersMS     *LayersMS `json:"layers_ms"`
	LossPPM      *int64    `json:"loss_ppm"`
	RTTMS        *int64    `json:"rtt_ms"`
}

type LayersMS struct {
	High      int64 `json:"high"`
	Medium    int64 `json:"medium"`
	Low       int64 `json:"low"`
	AudioOnly int64 `json:"audio_only"`
}

type UsageTotals struct {
	IngressBytes int64 `json:"ingress_bytes"`
	EgressBytes  int64 `json:"egress_bytes"`
	ConnectionMS int64 `json:"connection_ms"`
}

type UsageReport struct {
	SchemaVersion string            `json:"schema_version"`
	SessionID     string            `json:"session_id"`
	NodeID        string            `json:"node_id"`
	WorkerID      string            `json:"worker_id"`
	Role          string            `json:"role"`
	TopologyEpoch int64             `json:"topology_epoch"`
	Sequence      int64             `json:"sequence"`
	PeriodStartMS int64             `json:"period_start_ms"`
	PeriodEndMS   int64             `json:"period_end_ms"`
	Connections   []UsageConnection `json:"connections"`
	Totals        UsageTotals       `json:"totals"`
}

type SegmentEntry struct {
	URI    string `json:"uri"`
	MSN    int64  `json:"msn"`
	Part   *int64 `json:"part"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type SegmentInit struct {
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
}

type SegmentWindow struct {
	FirstMSN int64 `json:"first_msn"`
	LastMSN  int64 `json:"last_msn"`
}

type SegmentPage struct {
	Index int64 `json:"index"`
	Count int64 `json:"count"`
}

type SegmentManifest struct {
	SchemaVersion string         `json:"schema_version"`
	SessionID     string         `json:"session_id"`
	NodeID        string         `json:"node_id"`
	RenditionID   string         `json:"rendition_id"`
	TopologyEpoch int64          `json:"topology_epoch"`
	Sequence      int64          `json:"sequence"`
	IssuedAtMS    int64          `json:"issued_at_ms"`
	Ended         bool           `json:"ended"`
	Window        SegmentWindow  `json:"window"`
	Page          *SegmentPage   `json:"page"`
	Init          SegmentInit    `json:"init"`
	Entries       []SegmentEntry `json:"entries"`
}

const (
	ManifestLiveWindowSegments = 16
	ManifestVODPageSegments    = 256
	ManifestMaxEntries         = 2048
)

var segmentURIPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

var InitURIPattern = regexp.MustCompile(`^init_[0-9a-f]{16}\.mp4$`)

func SegmentInitURI(init []byte) string {
	digest := sha256.Sum256(init)
	return "init_" + hex.EncodeToString(digest[:8]) + ".mp4"
}

func CheckSegment(manifest SegmentManifest, uri string, body []byte) bool {
	digest := sha256.Sum256(body)
	expected := ""
	if manifest.Init.URI == uri {
		expected = manifest.Init.SHA256
	}
	for _, entry := range manifest.Entries {
		if entry.URI == uri {
			expected = entry.SHA256
		}
	}
	if !segmentURIPattern.MatchString(uri) || expected == "" {
		return false
	}
	want, err := DecodeHex(expected)
	return err == nil && subtle.ConstantTimeCompare(want, digest[:]) == 1
}

func DecodeUsageReport(envelope SignedEnvelope, nodeKey []byte) (UsageReport, error) {
	var report UsageReport
	payload, err := OpenEnvelope(envelope, UsageReportSchema, nodeKey)
	if err != nil {
		return report, err
	}
	if err := UnmarshalStrict(payload, &report); err != nil || report.SchemaVersion != UsageReportSchema {
		return report, ErrEnvelope
	}
	return report, nil
}

func DecodeSegmentManifest(envelope SignedEnvelope, packagerKey []byte) (SegmentManifest, error) {
	var manifest SegmentManifest
	payload, err := OpenEnvelope(envelope, SegmentManifestSchema, packagerKey)
	if err != nil {
		return manifest, err
	}
	if err := UnmarshalStrict(payload, &manifest); err != nil || manifest.SchemaVersion != SegmentManifestSchema {
		return manifest, ErrEnvelope
	}
	if !validManifestShape(manifest) {
		return manifest, ErrManifestInvalid
	}
	return manifest, nil
}

var (
	ErrManifestInvalid  = errors.New("manifest: invalid shape")
	ErrManifestMismatch = errors.New("manifest: other session, rendition or node")
	ErrManifestStale    = errors.New("manifest: sequence older than one already used")
)

type ManifestExpectation struct {
	SessionID   string
	RenditionID string
	NodeID      string
	MinSequence int64
	Page        *int64
}

func VerifySegmentManifest(envelope SignedEnvelope, packagerKey []byte, expect ManifestExpectation) (SegmentManifest, error) {
	manifest, err := DecodeSegmentManifest(envelope, packagerKey)
	if err != nil {
		return manifest, err
	}
	if manifest.SessionID != expect.SessionID || manifest.RenditionID != expect.RenditionID || manifest.NodeID != expect.NodeID {
		return manifest, ErrManifestMismatch
	}
	if expect.Page == nil {
		if manifest.Page != nil {
			return manifest, ErrManifestMismatch
		}
		if manifest.Sequence < expect.MinSequence {
			return manifest, ErrManifestStale
		}
		return manifest, nil
	}
	if manifest.Page == nil || manifest.Page.Index != *expect.Page || !manifest.Ended {
		return manifest, ErrManifestMismatch
	}
	return manifest, nil
}

var (
	manifestSessionPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	manifestRenditionPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
)

func validManifestShape(manifest SegmentManifest) bool {
	if !manifestSessionPattern.MatchString(manifest.SessionID) || !nodeIDPattern.MatchString(manifest.NodeID) ||
		!manifestRenditionPattern.MatchString(manifest.RenditionID) || manifest.TopologyEpoch < 1 || manifest.IssuedAtMS < 0 {
		return false
	}
	if len(manifest.Entries) > ManifestMaxEntries || manifest.Sequence < 1 || manifest.Window.FirstMSN < 0 ||
		manifest.Window.LastMSN < manifest.Window.FirstMSN || !InitURIPattern.MatchString(manifest.Init.URI) ||
		!sha256HexPattern.MatchString(manifest.Init.SHA256) || manifest.Init.URI != "init_"+manifest.Init.SHA256[:16]+".mp4" {
		return false
	}
	if manifest.Page != nil && (manifest.Page.Index < 0 || manifest.Page.Count < 1 || manifest.Page.Index >= manifest.Page.Count) {
		return false
	}
	for _, entry := range manifest.Entries {
		if !segmentURIPattern.MatchString(entry.URI) || !sha256HexPattern.MatchString(entry.SHA256) || entry.MSN < manifest.Window.FirstMSN ||
			entry.MSN > manifest.Window.LastMSN || entry.Bytes < 0 || (entry.Part != nil && *entry.Part < 0) {
			return false
		}
	}
	return true
}

const (
	WebhookSecretPrefix     = "whsec_"
	WebhookToleranceSeconds = 300
)

var (
	ErrWebhookSecret    = errors.New("webhook: invalid secret")
	ErrWebhookTimestamp = errors.New("webhook: timestamp outside tolerance")
	ErrWebhookSignature = errors.New("webhook: no matching signature")
)

func webhookKey(secret string) ([]byte, error) {
	if !strings.HasPrefix(secret, WebhookSecretPrefix) || strings.ContainsAny(secret, "\r\n") {
		return nil, ErrWebhookSecret
	}
	key, err := base64.StdEncoding.Strict().DecodeString(secret[len(WebhookSecretPrefix):])
	if err != nil || len(key) < 24 || len(key) > 64 {
		return nil, ErrWebhookSecret
	}
	return key, nil
}

func SignWebhook(secret, messageID string, timestamp int64, body []byte) (string, error) {
	key, err := webhookKey(secret)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(messageID + "." + strconv.FormatInt(timestamp, 10) + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

func VerifyWebhook(secret, messageID, timestampHeader, signatureHeader string, body []byte, now int64) error {
	timestamp, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil || strconv.FormatInt(timestamp, 10) != timestampHeader {
		return ErrWebhookTimestamp
	}
	if timestamp < now-WebhookToleranceSeconds || timestamp > now+WebhookToleranceSeconds {
		return ErrWebhookTimestamp
	}
	expected, err := SignWebhook(secret, messageID, timestamp, body)
	if err != nil {
		return err
	}
	for _, candidate := range strings.Fields(signatureHeader) {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1 {
			return nil
		}
	}
	return ErrWebhookSignature
}

const (
	PlaybackTagDomain         = "bpv1."
	PlaybackTagQueryParameter = "v"
)

var (
	playbackTagPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16}$`)
	playlistURIPattern = regexp.MustCompile(`[:,]URI="[^"]*"`)
	taggedPlaylistTags = []string{"#EXT-X-MAP:", "#EXT-X-PART:", "#EXT-X-PRELOAD-HINT:"}
)

func PlaybackTag(secret []byte, nonce string) string {
	if len(secret) != 32 {
		return ""
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(PlaybackTagDomain + nonce))
	return B64URL(mac.Sum(nil)[:12])
}

func RequestPlaybackTag(values []string) string {
	if len(values) != 1 || !playbackTagPattern.MatchString(values[0]) {
		return ""
	}
	return values[0]
}

func TagMediaPlaylist(body, tag string) string {
	if !playbackTagPattern.MatchString(tag) {
		return body
	}
	tagged := func(uri string) string {
		separator := "?"
		if strings.Contains(uri, "?") {
			separator = "&"
		}
		return uri + separator + PlaybackTagQueryParameter + "=" + tag
	}
	lines := strings.Split(body, "\n")
	for index, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		end := raw[len(line):]
		switch {
		case line == "":
		case !strings.HasPrefix(line, "#"):
			lines[index] = tagged(line) + end
		default:
			for _, name := range taggedPlaylistTags {
				if !strings.HasPrefix(line, name) {
					continue
				}
				if at := playlistURIPattern.FindStringIndex(line); at != nil {
					lines[index] = line[:at[0]+6] + tagged(line[at[0]+6:at[1]-1]) + line[at[1]-1:] + end
				}
			}
		}
	}
	return strings.Join(lines, "\n")
}

const (
	PlaybackCredentialPrefix   = "bpc1."
	PlaybackCredentialLifetime = 86_400_000
	PlaybackCredentialSkew     = 60_000
	PlaybackQueryParameter     = "pc"
)

var (
	ErrCredentialMalformed       = errors.New("playback credential: malformed")
	ErrCredentialInvalid         = errors.New("playback credential: bad signature")
	ErrCredentialSessionMismatch = errors.New("playback credential: other session")
	ErrCredentialExpired         = errors.New("playback credential: expired")
)

type PlaybackCredential struct {
	SessionID  string `json:"session_id"`
	Identity   string `json:"identity"`
	Nonce      string `json:"nonce"`
	IssuedAtMS int64  `json:"iat_ms"`
}

func playbackMAC(secret []byte, signed string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signed))
	return mac.Sum(nil)
}

func MintPlaybackCredential(secret []byte, credential PlaybackCredential) (string, error) {
	if len(secret) != 32 {
		return "", ErrCredentialMalformed
	}
	payload, err := MarshalWire(credential)
	if err != nil {
		return "", err
	}
	signed := PlaybackCredentialPrefix + B64URL(payload)
	return signed + "." + B64URL(playbackMAC(secret, signed)), nil
}

func VerifyPlaybackCredential(value string, secret []byte, sessionID string, nowMS int64) (PlaybackCredential, error) {
	var credential PlaybackCredential
	if len(value) > 1024 || !strings.HasPrefix(value, PlaybackCredentialPrefix) || len(secret) != 32 {
		return credential, ErrCredentialMalformed
	}
	parts := strings.Split(value[len(PlaybackCredentialPrefix):], ".")
	if len(parts) != 2 {
		return credential, ErrCredentialMalformed
	}
	payload, err := DecodeB64URL(parts[0])
	if err != nil {
		return credential, ErrCredentialMalformed
	}
	mac, err := DecodeB64URL(parts[1])
	if err != nil || len(mac) != sha256.Size {
		return credential, ErrCredentialMalformed
	}
	if !hmac.Equal(mac, playbackMAC(secret, PlaybackCredentialPrefix+parts[0])) {
		return credential, ErrCredentialInvalid
	}
	if err := UnmarshalStrict(payload, &credential); err != nil || !tokenIdentityPattern.MatchString(credential.Identity) ||
		!tokenNoncePattern.MatchString(credential.Nonce) {
		return PlaybackCredential{}, ErrCredentialMalformed
	}
	if canonical, err := MarshalWire(credential); err != nil || string(canonical) != string(payload) {
		return PlaybackCredential{}, ErrCredentialMalformed
	}
	if credential.SessionID != sessionID {
		return credential, ErrCredentialSessionMismatch
	}
	if credential.IssuedAtMS > nowMS+PlaybackCredentialSkew || nowMS-credential.IssuedAtMS >= PlaybackCredentialLifetime {
		return credential, ErrCredentialExpired
	}
	return credential, nil
}
