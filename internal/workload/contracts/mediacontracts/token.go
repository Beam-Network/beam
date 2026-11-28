package mediacontracts

import (
	"crypto/ed25519"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	TokenPrefix             = "bmt1."
	TokenMaxLength          = 4096
	TokenMaxPayloadBytes    = 2048
	TokenClockSkewSeconds   = 60
	TokenMaxLifetimeSeconds = 86400
	DefaultTokenAudience    = "beam-media"
	MaxTokenInteger         = 9007199254740991
)

const (
	ServiceKeyIDPrefix      = "beam-svc-"
	ManagedKeyIDPrefix      = "mk-"
	NodeIdentityPrefix      = "node:"
	AuditIdentityPrefix     = "audit:"
	MemberIdentityPrefix    = "member:"
	StreamKeyIdentityPrefix = "stream_key:"
)

var (
	serviceKeyIDPattern = regexp.MustCompile(`^beam-svc-[a-z0-9-]{1,48}$`)
	managedKeyIDPattern = regexp.MustCompile(`^mk-[0-9a-z]{26}$`)
)

const (
	RoleSubscribe = "subscribe"
	RolePublish   = "publish"
	RoleAdmin     = "admin"
)

var TrackSources = []string{"microphone", "camera", "screen", "screen_audio", "custom", TrackSourceMusic}

const TrackSourceMusic = "music"

const (
	KindHuman = "human"
	KindBot   = "bot"
)

var ParticipantKinds = []string{KindHuman, KindBot}

func ParticipantKindOf(claims TokenClaims) string {
	if claims.Kind == nil {
		return KindHuman
	}
	return *claims.Kind
}

var BroadRegions = []string{"wnam", "enam", "sam", "weur", "eeur", "apac", "oc", "afr", "me"}

const (
	ErrTokenMalformed          = "token_malformed"
	ErrTokenUnsupportedVersion = "token_unsupported_version"
	ErrTokenKeyUnknown         = "token_key_unknown"
	ErrTokenSignatureInvalid   = "token_signature_invalid"
	ErrTokenClaimsInvalid      = "token_claims_invalid"
	ErrTokenAudienceMismatch   = "token_audience_mismatch"
	ErrTokenProjectMismatch    = "token_project_mismatch"
	ErrTokenSessionMismatch    = "token_session_mismatch"
	ErrTokenNotYetValid        = "token_not_yet_valid"
	ErrTokenExpired            = "token_expired"
	ErrTokenRevoked            = "token_revoked"
	ErrTokenRoleForbidden      = "token_role_forbidden"
	ErrTokenGrantOnly          = "token_grant_only"
	ErrTokenTrackForbidden     = "token_track_forbidden"
)

type TokenError struct {
	Code   string
	Detail string
}

func (err *TokenError) Error() string {
	if err.Detail == "" {
		return err.Code
	}
	return err.Code + ": " + err.Detail
}

func tokenError(code, detail string) error { return &TokenError{Code: code, Detail: detail} }

func TokenErrorCode(err error) string {
	var tokenErr *TokenError
	if errors.As(err, &tokenErr) {
		return tokenErr.Code
	}
	return ""
}

type TokenClaims struct {
	Audience  string   `json:"aud"`
	KeyID     string   `json:"kid"`
	Project   string   `json:"project"`
	Session   string   `json:"session"`
	Role      string   `json:"role"`
	Identity  string   `json:"identity"`
	Name      *string  `json:"name,omitempty"`
	Tracks    []string `json:"tracks,omitempty"`
	Region    *string  `json:"region,omitempty"`
	Meta      *string  `json:"meta,omitempty"`
	Kind      *string  `json:"kind,omitempty"`
	IssuedAt  int64    `json:"iat"`
	NotBefore *int64   `json:"nbf,omitempty"`
	Expires   int64    `json:"exp"`
	Nonce     string   `json:"nonce"`
}

type TokenKey struct {
	KeyID     string  `json:"kid"`
	PublicKey string  `json:"public_key"`
	MaxRole   *string `json:"max_role,omitempty"`
}

type TokenDenyNonce struct {
	Nonce   string `json:"nonce"`
	Expires int64  `json:"exp"`
}

type TokenDenyIdentity struct {
	Identity string `json:"identity"`
	Until    int64  `json:"until"`
}

type TokenDenyList struct {
	Nonces     []TokenDenyNonce    `json:"nonces"`
	Identities []TokenDenyIdentity `json:"identities"`
}

type TokenVerifyContext struct {
	Audience string
	Project  string
	Session  string
	Keys     []TokenKey
	Deny     TokenDenyList
	Now      int64
}

var (
	tokenKeyIDPattern    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	tokenIDPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	tokenNoncePattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
	tokenAudiencePattern = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
	tokenIdentityPattern = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
	tokenVersionPattern  = regexp.MustCompile(`^bmt[0-9]+\.`)
)

var tokenClaimNames = map[string]bool{"aud": true, "kid": true, "project": true, "session": true, "role": true,
	"identity": true, "name": true, "tracks": true, "region": true, "meta": true, "kind": true, "iat": true, "nbf": true,
	"exp": true, "nonce": true}

func tokenTextAllowed(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 || r == 0xfffd {
			return false
		}
	}
	return true
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func ValidateTokenClaims(claims TokenClaims) error {
	invalid := func(detail string) error { return tokenError(ErrTokenClaimsInvalid, detail) }
	if !tokenAudiencePattern.MatchString(claims.Audience) {
		return invalid("aud")
	}
	if !tokenKeyIDPattern.MatchString(claims.KeyID) {
		return invalid("kid")
	}
	if strings.HasPrefix(claims.KeyID, ServiceKeyIDPrefix) && !serviceKeyIDPattern.MatchString(claims.KeyID) ||
		strings.HasPrefix(claims.KeyID, ManagedKeyIDPrefix) && !managedKeyIDPattern.MatchString(claims.KeyID) {
		return invalid("kid")
	}
	if !tokenIDPattern.MatchString(claims.Project) {
		return invalid("project")
	}
	if !tokenIDPattern.MatchString(claims.Session) {
		return invalid("session")
	}
	if claims.Role != RoleSubscribe && claims.Role != RolePublish && claims.Role != RoleAdmin {
		return invalid("role")
	}
	if !tokenIdentityPattern.MatchString(claims.Identity) {
		return invalid("identity")
	}
	serviceKey := strings.HasPrefix(claims.KeyID, ServiceKeyIDPrefix)
	serviceIdentity := strings.HasPrefix(claims.Identity, NodeIdentityPrefix) || strings.HasPrefix(claims.Identity, AuditIdentityPrefix)
	if serviceKey != serviceIdentity {
		return invalid("identity")
	}
	if strings.HasPrefix(claims.Identity, MemberIdentityPrefix) && !strings.HasPrefix(claims.KeyID, ManagedKeyIDPrefix) {
		return invalid("identity")
	}
	if strings.HasPrefix(claims.Identity, StreamKeyIdentityPrefix) && !strings.HasPrefix(claims.KeyID, ManagedKeyIDPrefix) {
		return invalid("identity")
	}
	if claims.Name != nil {
		if count := utf8.RuneCountInString(*claims.Name); count < 1 || count > 64 || !tokenTextAllowed(*claims.Name) {
			return invalid("name")
		}
	}
	if claims.Tracks != nil {
		if claims.Role == RoleSubscribe || len(claims.Tracks) < 1 || len(claims.Tracks) > len(TrackSources) {
			return invalid("tracks")
		}
		seen := map[string]bool{}
		for _, track := range claims.Tracks {
			if !contains(TrackSources, track) || seen[track] {
				return invalid("tracks")
			}
			seen[track] = true
		}
	}
	if claims.Region != nil && !contains(BroadRegions, *claims.Region) {
		return invalid("region")
	}
	if claims.Meta != nil && (len(*claims.Meta) > 512 || !tokenTextAllowed(*claims.Meta)) {
		return invalid("meta")
	}
	if claims.Kind != nil && !contains(ParticipantKinds, *claims.Kind) {
		return invalid("kind")
	}
	if claims.IssuedAt < 0 || claims.Expires > MaxTokenInteger || claims.Expires <= claims.IssuedAt ||
		claims.Expires-claims.IssuedAt > TokenMaxLifetimeSeconds {
		return invalid("exp")
	}
	if claims.NotBefore != nil && (*claims.NotBefore < claims.IssuedAt || *claims.NotBefore >= claims.Expires) {
		return invalid("nbf")
	}
	if !tokenNoncePattern.MatchString(claims.Nonce) {
		return invalid("nonce")
	}
	return nil
}

func canonicalTokenPayload(claims TokenClaims) ([]byte, error) {
	return marshalCanonical(claims)
}

func MintToken(claims TokenClaims, privateKey ed25519.PrivateKey) (string, error) {
	if err := ValidateTokenClaims(claims); err != nil {
		return "", err
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", errors.New("mediacontracts: invalid Ed25519 private key")
	}
	payload, err := canonicalTokenPayload(claims)
	if err != nil {
		return "", err
	}
	if len(payload) > TokenMaxPayloadBytes {
		return "", tokenError(ErrTokenClaimsInvalid, "payload too large")
	}
	signingInput := TokenPrefix + B64URL(payload)
	signature := ed25519.Sign(privateKey, []byte(signingInput))
	return signingInput + "." + B64URL(signature), nil
}

func ParseTokenUnverified(token string) (TokenClaims, []byte, []byte, error) {
	if len(token) > TokenMaxLength {
		return TokenClaims{}, nil, nil, tokenError(ErrTokenMalformed, "too long")
	}
	if !strings.HasPrefix(token, TokenPrefix) {
		if tokenVersionPattern.MatchString(token) {
			return TokenClaims{}, nil, nil, tokenError(ErrTokenUnsupportedVersion, "")
		}
		return TokenClaims{}, nil, nil, tokenError(ErrTokenMalformed, "prefix")
	}
	parts := strings.Split(token[len(TokenPrefix):], ".")
	if len(parts) != 2 {
		return TokenClaims{}, nil, nil, tokenError(ErrTokenMalformed, "parts")
	}
	payload, err := DecodeB64URL(parts[0])
	if err != nil || len(payload) == 0 || len(payload) > TokenMaxPayloadBytes || !utf8.Valid(payload) {
		return TokenClaims{}, nil, nil, tokenError(ErrTokenMalformed, "payload")
	}
	signature, err := DecodeB64URL(parts[1])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return TokenClaims{}, nil, nil, tokenError(ErrTokenMalformed, "signature")
	}
	object, err := decodeObject(payload)
	if err != nil {
		return TokenClaims{}, nil, nil, tokenError(ErrTokenMalformed, "payload JSON")
	}
	if duplicate, err := topLevelDuplicateKey(payload); err != nil || duplicate {
		return TokenClaims{}, nil, nil, tokenError(ErrTokenMalformed, "duplicate claim")
	}
	claims, err := claimsFromObject(object)
	return claims, payload, signature, err
}

func claimsFromObject(object map[string]any) (TokenClaims, error) {
	invalid := func(detail string) error { return tokenError(ErrTokenClaimsInvalid, detail) }
	var claims TokenClaims
	kid, ok := object["kid"].(string)
	if !ok || !tokenKeyIDPattern.MatchString(kid) {
		return claims, invalid("kid")
	}
	claims.KeyID = kid
	for name := range object {
		if !tokenClaimNames[name] {
			return claims, invalid("unknown claim " + name)
		}
	}
	text := func(name string, target *string) error {
		value, ok := object[name].(string)
		if !ok {
			return invalid(name)
		}
		*target = value
		return nil
	}
	optionalText := func(name string, target **string) error {
		raw, present := object[name]
		if !present {
			return nil
		}
		value, ok := raw.(string)
		if !ok {
			return invalid(name)
		}
		*target = &value
		return nil
	}
	integer := func(name string) (int64, error) {
		value, ok := jsonInteger(object[name])
		if !ok {
			return 0, invalid(name)
		}
		return value, nil
	}
	for _, field := range []struct {
		name   string
		target *string
	}{{"aud", &claims.Audience}, {"project", &claims.Project}, {"session", &claims.Session},
		{"role", &claims.Role}, {"identity", &claims.Identity}, {"nonce", &claims.Nonce}} {
		if err := text(field.name, field.target); err != nil {
			return claims, err
		}
	}
	for _, field := range []struct {
		name   string
		target **string
	}{{"name", &claims.Name}, {"region", &claims.Region}, {"meta", &claims.Meta}, {"kind", &claims.Kind}} {
		if err := optionalText(field.name, field.target); err != nil {
			return claims, err
		}
	}
	if raw, present := object["tracks"]; present {
		list, ok := raw.([]any)
		if !ok {
			return claims, invalid("tracks")
		}
		claims.Tracks = make([]string, 0, len(list))
		for _, item := range list {
			value, ok := item.(string)
			if !ok {
				return claims, invalid("tracks")
			}
			claims.Tracks = append(claims.Tracks, value)
		}
	}
	var err error
	if claims.IssuedAt, err = integer("iat"); err != nil {
		return claims, err
	}
	if claims.Expires, err = integer("exp"); err != nil {
		return claims, err
	}
	if _, present := object["nbf"]; present {
		value, err := integer("nbf")
		if err != nil {
			return claims, err
		}
		claims.NotBefore = &value
	}
	return claims, nil
}

type TokenVerifier struct {
	audience, project, session string
	keys                       map[string]ed25519.PublicKey
	maxRoles                   map[string]string
	deniedNonces               map[string]bool
	deniedIdentities           map[string]int64
}

func NewTokenVerifier(context TokenVerifyContext) *TokenVerifier {
	verifier := &TokenVerifier{audience: context.Audience, project: context.Project, session: context.Session,
		keys: map[string]ed25519.PublicKey{}, maxRoles: map[string]string{}, deniedNonces: map[string]bool{},
		deniedIdentities: map[string]int64{}}
	for _, key := range context.Keys {
		if _, seen := verifier.keys[key.KeyID]; seen {
			continue
		}
		decoded, err := DecodeB64URL(key.PublicKey)
		if err != nil || len(decoded) != ed25519.PublicKeySize {
			decoded = nil
		}
		verifier.keys[key.KeyID] = decoded
		if key.MaxRole != nil {
			verifier.maxRoles[key.KeyID] = *key.MaxRole
		}
	}
	for _, denied := range context.Deny.Nonces {
		verifier.deniedNonces[denied.Nonce] = true
	}
	for _, denied := range context.Deny.Identities {
		if denied.Until > verifier.deniedIdentities[denied.Identity] {
			verifier.deniedIdentities[denied.Identity] = denied.Until
		}
	}
	return verifier
}

func VerifyToken(token string, context TokenVerifyContext) (TokenClaims, error) {
	return NewTokenVerifier(context).Verify(token, context.Now)
}

func (verifier *TokenVerifier) Verify(token string, now int64) (TokenClaims, error) {
	claims, err := verifier.verifyAny(token, now)
	if err != nil {
		return TokenClaims{}, err
	}
	if strings.HasPrefix(claims.Identity, StreamKeyIdentityPrefix) {
		return TokenClaims{}, tokenError(ErrTokenGrantOnly, "")
	}
	return claims, nil
}

func (verifier *TokenVerifier) VerifyIngestGrant(token string, now int64, node IngestGrantNode) (TokenClaims, string, error) {
	claims, err := verifier.verifyAny(token, now)
	if err != nil {
		return TokenClaims{}, "", err
	}
	keyID, err := CheckIngestGrant(claims, node, now)
	if err != nil {
		return TokenClaims{}, "", err
	}
	return claims, keyID, nil
}

func (verifier *TokenVerifier) verifyAny(token string, now int64) (TokenClaims, error) {
	claims, payload, signature, err := parseForVerify(token)
	if err != nil {
		return TokenClaims{}, err
	}
	publicKey := verifier.keys[claims.KeyID]
	if publicKey == nil {
		return TokenClaims{}, tokenError(ErrTokenKeyUnknown, claims.KeyID)
	}
	if !ed25519.Verify(publicKey, []byte(TokenPrefix+B64URL(payload)), signature) {
		return TokenClaims{}, tokenError(ErrTokenSignatureInvalid, "")
	}
	if claims.parseErr != nil {
		return TokenClaims{}, claims.parseErr
	}
	result := claims.TokenClaims
	if err := ValidateTokenClaims(result); err != nil {
		return TokenClaims{}, err
	}
	if result.Audience != verifier.audience {
		return TokenClaims{}, tokenError(ErrTokenAudienceMismatch, "")
	}
	if result.Project != verifier.project {
		return TokenClaims{}, tokenError(ErrTokenProjectMismatch, "")
	}
	if result.Session != verifier.session {
		return TokenClaims{}, tokenError(ErrTokenSessionMismatch, "")
	}
	notBefore := result.IssuedAt
	if result.NotBefore != nil && *result.NotBefore > notBefore {
		notBefore = *result.NotBefore
	}
	if now+TokenClockSkewSeconds < notBefore {
		return TokenClaims{}, tokenError(ErrTokenNotYetValid, "")
	}
	if now-TokenClockSkewSeconds >= result.Expires {
		return TokenClaims{}, tokenError(ErrTokenExpired, "")
	}
	if verifier.Revoked(result.Nonce, result.Identity, now) {
		return TokenClaims{}, tokenError(ErrTokenRevoked, "")
	}
	if maxRole, capped := verifier.maxRoles[result.KeyID]; capped && !RoleIncludes(maxRole, result.Role) {
		return TokenClaims{}, tokenError(ErrTokenRoleForbidden, "above the key's max_role")
	}
	return result, nil
}

func (verifier *TokenVerifier) Revoked(nonce, identity string, now int64) bool {
	if verifier.deniedNonces[nonce] {
		return true
	}
	until, denied := verifier.deniedIdentities[identity]
	return denied && now < until
}

type parsedClaims struct {
	TokenClaims
	parseErr error
}

func parseForVerify(token string) (parsedClaims, []byte, []byte, error) {
	claims, payload, signature, err := ParseTokenUnverified(token)
	if err == nil {
		return parsedClaims{TokenClaims: claims}, payload, signature, nil
	}
	if TokenErrorCode(err) != ErrTokenClaimsInvalid || claims.KeyID == "" {
		return parsedClaims{}, nil, nil, err
	}
	return parsedClaims{TokenClaims: TokenClaims{KeyID: claims.KeyID}, parseErr: err}, payload, signature, nil
}

func RoleIncludes(have, need string) bool {
	rank := map[string]int{RoleSubscribe: 1, RolePublish: 2, RoleAdmin: 3}
	return rank[have] != 0 && rank[have] >= rank[need]
}

func AuthorizeToken(claims TokenClaims, role, trackSource string) error {
	if !RoleIncludes(claims.Role, role) {
		return tokenError(ErrTokenRoleForbidden, "")
	}
	if trackSource != "" {
		if !contains(TrackSources, trackSource) {
			return tokenError(ErrTokenTrackForbidden, trackSource)
		}
		if claims.Tracks != nil && !contains(claims.Tracks, trackSource) {
			return tokenError(ErrTokenTrackForbidden, trackSource)
		}
	}
	return nil
}

const E2EEMembersOnly = "e2ee_members_only"

func MembersOnlyRefused(mode string, keyProvider *string, identity string) bool {
	if mode != "e2ee" || keyProvider == nil || *keyProvider != "mls" {
		return false
	}
	return !strings.HasPrefix(identity, MemberIdentityPrefix) && !strings.HasPrefix(identity, NodeIdentityPrefix)
}
