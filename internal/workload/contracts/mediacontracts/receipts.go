package mediacontracts

import (
	"crypto/ed25519"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	ReceiptGrantPrefix = "brg1."
	ReceiptPrefix      = "bmr1."
	ReceiptSchema      = "beam-media-receipt/v1"

	ReceiptGrantMaxLength = 1024
	ReceiptMaxLength      = 640

	ReceiptIntervalMS          = 20_000
	ReceiptMinIntervalMS       = 5_000
	ReceiptMissingAfterMS      = 120_000
	ReceiptMismatchWindowMS    = 60_000
	ReceiptMismatchBytes       = 2_000_000
	ReceiptMismatchConnectedMS = 10_000
	ReceiptMismatchDivisor     = 5
)

var (
	ErrReceiptGrantMalformed  = errors.New("receipt_grant_malformed")
	ErrReceiptGrantKeyUnknown = errors.New("receipt_grant_key_unknown")
	ErrReceiptGrantSignature  = errors.New("receipt_grant_signature_invalid")
	ErrReceiptGrantMismatch   = errors.New("receipt_grant_mismatch")
	ErrReceiptGrantExpired    = errors.New("receipt_grant_expired")
)

var (
	ErrReceiptMalformed   = errors.New("receipt_malformed")
	ErrReceiptConnection  = errors.New("receipt_connection")
	ErrReceiptStale       = errors.New("receipt_stale")
	ErrReceiptRateLimited = errors.New("rate_limited")
	ErrReceiptSignature   = errors.New("receipt_signature_invalid")
)

func ReceiptWireError(err error) string {
	switch {
	case errors.Is(err, ErrReceiptConnection):
		return "receipt_connection"
	case errors.Is(err, ErrReceiptRateLimited):
		return "rate_limited"
	default:
		return "receipt_invalid"
	}
}

var (
	receiptGrantTextPattern  = regexp.MustCompile(`^brg1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{86}$`)
	receiptTextPattern       = regexp.MustCompile(`^bmr1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{86}$`)
	UsageConnectionIDPattern = regexp.MustCompile(`^c_[A-Za-z0-9_-]{16,40}$`)
)

func validateReceiptText(value string) error {
	if len(value) > ReceiptMaxLength || !receiptTextPattern.MatchString(value) {
		return ErrReceiptMalformed
	}
	return nil
}

func validateReceiptGrantText(value string) error {
	if len(value) > ReceiptGrantMaxLength || !receiptGrantTextPattern.MatchString(value) {
		return ErrReceiptGrantMalformed
	}
	return nil
}

func ValidateReceiptKey(key string) error {
	if decoded, err := DecodeB64URL(key); err != nil || len(decoded) != ed25519.PublicKeySize {
		return errors.New("invalid receipt_key")
	}
	return nil
}

func ValidateJoinReceipts(join JoinResponse) error {
	if join.Receipts == nil {
		return nil
	}
	if join.Receipts.IntervalMS < ReceiptMinIntervalMS || join.Receipts.IntervalMS > 60_000 {
		return errors.New("receipts.interval_ms out of range")
	}
	return validateReceiptGrantText(join.Receipts.Grant)
}

func ValidateHLSReceiptPost(post HLSReceiptPost) error {
	if err := validateReceiptGrantText(post.Grant); err != nil || post.Receipt == nil {
		return err
	}
	return validateReceiptText(*post.Receipt)
}

func splitSigned(value, prefix string, maxLength int) (signed string, payload, signature []byte, ok bool) {
	if len(value) > maxLength || !strings.HasPrefix(value, prefix) {
		return "", nil, nil, false
	}
	parts := strings.Split(value[len(prefix):], ".")
	if len(parts) != 2 {
		return "", nil, nil, false
	}
	payload, err := DecodeB64URL(parts[0])
	if err != nil || len(payload) == 0 || !utf8.Valid(payload) {
		return "", nil, nil, false
	}
	signature, err = DecodeB64URL(parts[1])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return "", nil, nil, false
	}
	return prefix + parts[0], payload, signature, true
}

func signText(signed string, key ed25519.PrivateKey) string {
	return signed + "." + B64URL(ed25519.Sign(key, []byte(signed)))
}

type ReceiptGrant struct {
	KeyID    string `json:"kid"`
	Session  string `json:"session"`
	Nonce    string `json:"nonce"`
	Identity string `json:"identity"`
	Role     string `json:"role"`
	Kind     string `json:"kind"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
	Key      string `json:"key"`
}

func validReceiptGrant(grant ReceiptGrant) bool {
	key, err := DecodeB64URL(grant.Key)
	return err == nil && len(key) == ed25519.PublicKeySize && ReceiptGrantKeyIDPattern.MatchString(grant.KeyID) &&
		tokenIDPattern.MatchString(grant.Session) && tokenNoncePattern.MatchString(grant.Nonce) && tokenIdentityPattern.MatchString(grant.Identity) &&
		(grant.Role == RoleSubscribe || grant.Role == RolePublish || grant.Role == RoleAdmin) && contains(ParticipantKinds, grant.Kind) &&
		grant.IssuedAt >= 0 && grant.Expires > grant.IssuedAt && grant.Expires-grant.IssuedAt <= TokenMaxLifetimeSeconds && grant.Expires <= MaxTokenInteger
}

func ReceiptGrantFor(claims TokenClaims, keyID, receiptKey string) ReceiptGrant {
	return ReceiptGrant{KeyID: keyID, Session: claims.Session, Nonce: claims.Nonce, Identity: claims.Identity, Role: claims.Role,
		Kind: ParticipantKindOf(claims), IssuedAt: claims.IssuedAt, Expires: claims.Expires, Key: receiptKey}
}

func MintReceiptGrant(grant ReceiptGrant, key ed25519.PrivateKey) (string, error) {
	if !validReceiptGrant(grant) {
		return "", ErrReceiptGrantMalformed
	}
	payload, err := MarshalWire(grant)
	if err != nil {
		return "", err
	}
	return signText(ReceiptGrantPrefix+B64URL(payload), key), nil
}

func ParseReceiptGrant(value string) (ReceiptGrant, error) {
	var grant ReceiptGrant
	_, payload, _, ok := splitSigned(value, ReceiptGrantPrefix, ReceiptGrantMaxLength)
	if !ok || UnmarshalStrict(payload, &grant) != nil || !validReceiptGrant(grant) {
		return ReceiptGrant{}, ErrReceiptGrantMalformed
	}
	if canonical, err := MarshalWire(grant); err != nil || string(canonical) != string(payload) {
		return ReceiptGrant{}, ErrReceiptGrantMalformed
	}
	return grant, nil
}

type ReceiptGrantContext struct {
	Keys     []ReceiptGrantKey
	Session  string
	Nonce    string
	Identity string
	Role     string
	Kind     string
	Now      int64
}

func VerifyReceiptGrant(value string, context ReceiptGrantContext) (ReceiptGrant, error) {
	grant, err := ParseReceiptGrant(value)
	if err != nil {
		return ReceiptGrant{}, err
	}
	var public []byte
	for _, key := range context.Keys {
		if key.KeyID == grant.KeyID {
			public, _ = DecodeB64URL(key.PublicKey)
			break
		}
	}
	if len(public) != ed25519.PublicKeySize {
		return ReceiptGrant{}, ErrReceiptGrantKeyUnknown
	}
	signed, _, signature, _ := splitSigned(value, ReceiptGrantPrefix, ReceiptGrantMaxLength)
	if !ed25519.Verify(public, []byte(signed), signature) {
		return ReceiptGrant{}, ErrReceiptGrantSignature
	}
	if grant.Session != context.Session || grant.Nonce != context.Nonce || grant.Identity != context.Identity || grant.Role != context.Role || grant.Kind != context.Kind {
		return ReceiptGrant{}, ErrReceiptGrantMismatch
	}
	if context.Now >= 0 && (context.Now+TokenClockSkewSeconds < grant.IssuedAt || context.Now-TokenClockSkewSeconds >= grant.Expires) {
		return ReceiptGrant{}, ErrReceiptGrantExpired
	}
	return grant, nil
}

type Receipt struct {
	Session     string `json:"session"`
	Node        string `json:"node"`
	Connection  string `json:"connection"`
	Nonce       string `json:"nonce"`
	Sequence    int64  `json:"sequence"`
	ConnectedMS int64  `json:"connected_ms"`
	MediaBytes  int64  `json:"media_bytes"`
}

func validReceipt(receipt Receipt) bool {
	inRange := func(value, min int64) bool { return value >= min && value <= maxSafe }
	return tokenIDPattern.MatchString(receipt.Session) && nodeIDPattern.MatchString(receipt.Node) &&
		UsageConnectionIDPattern.MatchString(receipt.Connection) && tokenNoncePattern.MatchString(receipt.Nonce) &&
		inRange(receipt.Sequence, 1) && inRange(receipt.ConnectedMS, 0) && inRange(receipt.MediaBytes, 0)
}

func ReceiptSigningInput(receipt Receipt) (string, error) {
	if !validReceipt(receipt) {
		return "", ErrReceiptMalformed
	}
	payload, err := MarshalWire(receipt)
	if err != nil {
		return "", err
	}
	return ReceiptPrefix + B64URL(payload), nil
}

func SignReceipt(receipt Receipt, key ed25519.PrivateKey) (string, error) {
	signed, err := ReceiptSigningInput(receipt)
	if err != nil {
		return "", err
	}
	return signText(signed, key), nil
}

func ParseReceipt(value string) (Receipt, error) {
	var receipt Receipt
	_, payload, _, ok := splitSigned(value, ReceiptPrefix, ReceiptMaxLength)
	if !ok || UnmarshalStrict(payload, &receipt) != nil || !validReceipt(receipt) {
		return Receipt{}, ErrReceiptMalformed
	}
	if canonical, err := MarshalWire(receipt); err != nil || string(canonical) != string(payload) {
		return Receipt{}, ErrReceiptMalformed
	}
	return receipt, nil
}

type ReceiptBinding struct {
	Session    string `json:"session"`
	Node       string `json:"node"`
	Connection string `json:"connection"`
	Nonce      string `json:"nonce"`
}

func (binding ReceiptBinding) matches(receipt Receipt) bool {
	return receipt.Session == binding.Session && receipt.Node == binding.Node && receipt.Connection == binding.Connection && receipt.Nonce == binding.Nonce
}

func VerifyReceipt(value, key string, binding ReceiptBinding) (Receipt, error) {
	receipt, err := ParseReceipt(value)
	if err != nil {
		return Receipt{}, err
	}
	if !binding.matches(receipt) {
		return Receipt{}, ErrReceiptConnection
	}
	public, err := DecodeB64URL(key)
	signed, _, signature, _ := splitSigned(value, ReceiptPrefix, ReceiptMaxLength)
	if err != nil || len(public) != ed25519.PublicKeySize || !ed25519.Verify(public, []byte(signed), signature) {
		return Receipt{}, ErrReceiptSignature
	}
	return receipt, nil
}

type ReceiptPoint struct {
	AtMS               int64
	ReceiptConnectedMS int64
	ReceiptBytes       int64
	NodeConnectedMS    int64
	NodeBytes          int64
}

func ReceiptDisagrees(reference, point ReceiptPoint) bool {
	below := func(node, receipt, floor int64) bool {
		return node-receipt > floor && ReceiptMismatchDivisor*(node-receipt) > node
	}
	return below(point.NodeBytes-reference.NodeBytes, point.ReceiptBytes-reference.ReceiptBytes, ReceiptMismatchBytes) ||
		below(point.NodeConnectedMS-reference.NodeConnectedMS, point.ReceiptConnectedMS-reference.ReceiptConnectedMS, ReceiptMismatchConnectedMS)
}

type ReceiptConnection struct {
	binding   ReceiptBinding
	key       string
	startMS   int64
	value     string
	receipt   Receipt
	atMS      int64
	points    []ReceiptPoint
	disagrees bool
	renewedMS int64
}

func NewReceiptConnection(binding ReceiptBinding, key string, startMS int64) *ReceiptConnection {
	return &ReceiptConnection{binding: binding, key: key, startMS: startMS, points: []ReceiptPoint{{AtMS: startMS}}, renewedMS: startMS}
}

func (c *ReceiptConnection) Latest() (value string, receivedMS int64) { return c.value, c.atMS }

func (c *ReceiptConnection) Sequence() int64 { return c.receipt.Sequence }

func (c *ReceiptConnection) Accept(value string, nowMS, nodeConnectedMS, nodeMediaBytes int64) (duplicate bool, err error) {
	if c.value != "" && value == c.value {
		return true, nil
	}
	receipt, err := ParseReceipt(value)
	if err != nil {
		return false, err
	}
	if !c.binding.matches(receipt) {
		return false, ErrReceiptConnection
	}
	if c.value != "" {
		if receipt.Sequence <= c.receipt.Sequence || receipt.ConnectedMS < c.receipt.ConnectedMS || receipt.MediaBytes < c.receipt.MediaBytes {
			return false, ErrReceiptStale
		}
		if nowMS-c.atMS < ReceiptMinIntervalMS {
			return false, ErrReceiptRateLimited
		}
	}
	if _, err := VerifyReceipt(value, c.key, c.binding); err != nil {
		return false, err
	}
	c.value, c.receipt, c.atMS = value, receipt, nowMS
	point := ReceiptPoint{AtMS: nowMS, ReceiptConnectedMS: receipt.ConnectedMS, ReceiptBytes: receipt.MediaBytes,
		NodeConnectedMS: nodeConnectedMS, NodeBytes: nodeMediaBytes}
	if !ReceiptDisagrees(ReceiptPoint{AtMS: c.startMS}, point) {
		c.renewedMS = nowMS
	}
	for len(c.points) > 1 && c.points[1].AtMS <= nowMS-ReceiptMismatchWindowMS {
		c.points = c.points[1:]
	}
	c.disagrees = c.points[0].AtMS <= nowMS-ReceiptMismatchWindowMS &&
		(ReceiptDisagrees(c.points[0], point) || ReceiptDisagrees(ReceiptPoint{AtMS: c.startMS}, point))
	c.points = append(c.points, point)
	return false, nil
}

func (c *ReceiptConnection) Refusal(nowMS int64) string {
	if c.disagrees {
		return "receipt_mismatch"
	}
	if nowMS-c.renewedMS > ReceiptMissingAfterMS {
		return "receipt_missing"
	}
	return ""
}

type RTCStreamBytes struct {
	Bytes              int64 `json:"bytes"`
	HeaderBytes        int64 `json:"header_bytes,omitempty"`
	RetransmittedBytes int64 `json:"retransmitted_bytes,omitempty"`
	FECBytes           int64 `json:"fec_bytes,omitempty"`
}

func RTCClientMediaBytes(streams []RTCStreamBytes) int64 {
	var total int64
	for _, stream := range streams {
		if bytes := stream.Bytes + stream.HeaderBytes - stream.RetransmittedBytes - stream.FECBytes; bytes > 0 {
			total += bytes
		}
	}
	return total
}

type RTCStreamReading struct {
	ID string `json:"id"`
	RTCStreamBytes
}

type RTCClientMediaTotal struct {
	streams map[string]rtcStreamTotal
	total   int64
}

type rtcStreamTotal struct{ bytes, media int64 }

func (t *RTCClientMediaTotal) Update(streams []RTCStreamReading) int64 {
	if t.streams == nil {
		t.streams = map[string]rtcStreamTotal{}
	}
	for _, stream := range streams {
		media := RTCClientMediaBytes([]RTCStreamBytes{stream.RTCStreamBytes})
		last, known := t.streams[stream.ID]
		switch {
		case !known || stream.Bytes < last.bytes:
			t.total += media
		case media > last.media:
			t.total += media - last.media
		}
		t.streams[stream.ID] = rtcStreamTotal{bytes: stream.Bytes, media: media}
	}
	return t.total
}
