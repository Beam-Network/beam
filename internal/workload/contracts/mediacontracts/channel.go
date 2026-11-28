package mediacontracts

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

var ChannelIDPattern = regexp.MustCompile(`^mch_[0-9a-z]{26}$`)

const ChannelStreamKeysMax = 8

const SessionStreamKeysMax = 16

var ChannelErrorCodes = []string{"channel_limit", "channel_standby_limit", "channel_name_taken", "stream_key_limit"}

const (
	IngestPortWHIP  = 443
	IngestPortRTMP  = 1935
	IngestPortRTMPS = 1936
	IngestPortSRT   = 9000
)

const SRTStreamKeyPlaceholder = "{stream_key}"

var ErrChannelID = errors.New("mediacontracts: invalid channel id")

func ChannelIngestHostname(channelID, domain string) (string, error) {
	if !ChannelIDPattern.MatchString(channelID) {
		return "", ErrChannelID
	}
	if !ValidHostname(domain) {
		return "", ErrEdgeDomain
	}
	return strings.TrimPrefix(channelID, "mch_") + ".ingest." + domain, nil
}

type ChannelIngestURLs struct {
	WHIPURL       string `json:"whip_url"`
	RTMPServerURL string `json:"rtmp_server_url"`
	SRTURL        string `json:"srt_url"`
}

func ChannelIngestURLsFor(channelID, domain string, key *string) (ChannelIngestURLs, error) {
	host, err := ChannelIngestHostname(channelID, domain)
	if err != nil {
		return ChannelIngestURLs{}, err
	}
	secret := SRTStreamKeyPlaceholder
	if key != nil {
		secret = *key
	}
	return ChannelIngestURLs{
		WHIPURL:       "https://ingest." + domain + "/v1/channels/" + channelID + "/whip",
		RTMPServerURL: fmt.Sprintf("rtmp://%s:%d/live", host, IngestPortRTMP),
		SRTURL:        fmt.Sprintf("srt://%s:%d?streamid=#!::r=%s,m=publish,s=%s", host, IngestPortSRT, channelID, secret),
	}, nil
}

const (
	IngestGrantQueryParameter     = "grant"
	IngestGrantTTLSeconds         = 30
	IngestGrantMaxLifetimeSeconds = 60
	IngestGrantNoncesMax          = 4096
	ErrGrantInvalid               = "grant_invalid"
	ErrGrantUsed                  = "grant_used"
	ErrGrantRequired              = "grant_required"
	ErrGrantRateLimited           = "rate_limited"
	WorkerFeatureChannelIngest    = "channel_ingest"
)

var streamKeyIDPatternGrant = regexp.MustCompile(`^msk_[0-9a-z]{26}$`)

func IngestGrantClaims(audience, kid, project, session, streamKeyID, nodeID string, iat int64, nonce string) TokenClaims {
	meta := nodeID
	return TokenClaims{Audience: audience, KeyID: kid, Project: project, Session: session, Role: RolePublish,
		Identity: StreamKeyIdentityPrefix + streamKeyID, Meta: &meta, IssuedAt: iat, Expires: iat + IngestGrantTTLSeconds, Nonce: nonce}
}

type IngestGrantNode struct {
	StreamKeyIDs []string
	NodeID       string
	Nonces       *IngestGrantNonces
}

func CheckIngestGrant(claims TokenClaims, node IngestGrantNode, now int64) (string, error) {
	if !managedKeyIDPattern.MatchString(claims.KeyID) {
		return "", tokenError(ErrGrantInvalid, "not signed by a managed key")
	}
	if claims.Role != RolePublish || claims.Tracks != nil {
		return "", tokenError(ErrGrantInvalid, "role")
	}
	if claims.Expires-claims.IssuedAt > IngestGrantMaxLifetimeSeconds {
		return "", tokenError(ErrGrantInvalid, "lifetime")
	}
	keyID, ok := strings.CutPrefix(claims.Identity, StreamKeyIdentityPrefix)
	if !ok {
		return "", tokenError(ErrGrantInvalid, "identity")
	}
	if !streamKeyIDPatternGrant.MatchString(keyID) || !contains(node.StreamKeyIDs, keyID) {
		return "", tokenError(ErrGrantInvalid, "stream key not in the topology")
	}
	if claims.Meta == nil || *claims.Meta != node.NodeID {
		return "", tokenError(ErrGrantInvalid, "another ingest node")
	}
	if node.Nonces == nil {
		return "", tokenError(ErrGrantInvalid, "no nonce memory")
	}
	switch node.Nonces.Use(claims.Nonce, claims.Expires, now) {
	case "used":
		return "", tokenError(ErrGrantUsed, "")
	case "full":
		return "", tokenError(ErrGrantRateLimited, "grant memory full")
	}
	return keyID, nil
}

type IngestGrantNonces struct {
	mu     sync.Mutex
	max    int
	nonces map[string]int64
}

func NewIngestGrantNonces(max int) *IngestGrantNonces {
	if max <= 0 {
		max = IngestGrantNoncesMax
	}
	return &IngestGrantNonces{max: max, nonces: map[string]int64{}}
}

func (cache *IngestGrantNonces) Use(nonce string, exp, now int64) string {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for seen, until := range cache.nonces {
		if now >= until {
			delete(cache.nonces, seen)
		}
	}
	if _, ok := cache.nonces[nonce]; ok {
		return "used"
	}
	if len(cache.nonces) >= cache.max {
		return "full"
	}
	cache.nonces[nonce] = exp + TokenClockSkewSeconds
	return "ok"
}

type ChannelProtection struct {
	Mode        string  `json:"mode"`
	KeyProvider *string `json:"key_provider"`
}

type ChannelTemplate struct {
	Ladder          *TranscodeLadder  `json:"ladder"`
	Recording       RecordingSettings `json:"recording"`
	Regions         *[]string         `json:"regions"`
	Protection      ChannelProtection `json:"protection"`
	StartGraceS     int64             `json:"start_grace_s"`
	IdleEndS        *int64            `json:"idle_end_s"`
	MaxParticipants *int64            `json:"max_participants"`
	Metadata        *string           `json:"metadata"`
}

type ChannelRecordingRequest struct {
	Enabled    bool   `json:"enabled"`
	DVRWindowS *int64 `json:"dvr_window_s,omitempty"`
}

type ChannelProtectionRequest struct {
	Mode string `json:"mode"`
}

type ChannelTemplateRequest struct {
	Ladder          *LadderRequest            `json:"ladder,omitempty"`
	Recording       *ChannelRecordingRequest  `json:"recording,omitempty"`
	Regions         []string                  `json:"regions,omitempty"`
	Protection      *ChannelProtectionRequest `json:"protection,omitempty"`
	StartGraceS     *int64                    `json:"start_grace_s,omitempty"`
	IdleEndS        *int64                    `json:"idle_end_s,omitempty"`
	MaxParticipants *int64                    `json:"max_participants,omitempty"`
	Metadata        *string                   `json:"metadata,omitempty"`
}

type CreateChannelRequest struct {
	ProjectID     string                  `json:"project_id"`
	Name          string                  `json:"name"`
	Template      *ChannelTemplateRequest `json:"template,omitempty"`
	IngestStandby *bool                   `json:"ingest_standby,omitempty"`
}

type UpdateChannelRequest struct {
	Name          *string                 `json:"name,omitempty"`
	Template      *ChannelTemplateRequest `json:"template,omitempty"`
	IngestStandby *bool                   `json:"ingest_standby,omitempty"`
}

type ChannelStandbyError struct {
	Error   string  `json:"error"`
	Detail  *string `json:"detail"`
	RetryAt string  `json:"retry_at"`
}

type MediaChannel struct {
	ID               string               `json:"id"`
	ProjectID        string               `json:"project_id"`
	Name             string               `json:"name"`
	Template         ChannelTemplate      `json:"template"`
	IngestStandby    bool                 `json:"ingest_standby"`
	CurrentSessionID *string              `json:"current_session_id"`
	StandbyError     *ChannelStandbyError `json:"standby_error"`
	CreatedAt        string               `json:"created_at"`
	UpdatedAt        string               `json:"updated_at"`
}

type MediaChannelList struct {
	Channels   []MediaChannel `json:"channels"`
	NextCursor *string        `json:"next_cursor"`
}

var ErrStreamKeyOwner = errors.New("mediacontracts: a stream key belongs to exactly one of a session and a channel")

var (
	mediaProjectIDPattern = regexp.MustCompile(`^mprj_[0-9a-z]{26}$`)
	mediaSessionIDPattern = regexp.MustCompile(`^msn_[0-9a-z]{26}$`)
)

func ValidateStreamKey(key StreamKey) error {
	if (key.SessionID == nil) == (key.ChannelID == nil) {
		return ErrStreamKeyOwner
	}
	if key.SessionID != nil && !mediaSessionIDPattern.MatchString(*key.SessionID) {
		return errors.New("mediacontracts: invalid session_id")
	}
	if key.ChannelID != nil && !ChannelIDPattern.MatchString(*key.ChannelID) {
		return ErrChannelID
	}
	if key.Ingest.Ready && (key.Ingest.WHIPURL == nil || key.Ingest.RetryAfterMS != nil) {
		return errors.New("mediacontracts: a ready ingest names its WHIP URL and carries no retry_after_ms")
	}
	return nil
}

func validChannelName(name string) bool {
	count := utf8.RuneCountInString(name)
	return utf8.ValidString(name) && count >= 1 && count <= 128
}

func validRegions(regions []string) bool {
	if len(regions) < 1 || len(regions) > 9 {
		return false
	}
	for _, region := range regions {
		if !contains(BroadRegions, region) {
			return false
		}
	}
	return true
}

func validateTemplateBounds(dvr *int64, regions *[]string, maxParticipants *int64, metadata *string, startGrace, idleEnd *int64) error {
	switch {
	case dvr != nil && (*dvr < 0 || *dvr > 43_200):
		return errors.New("mediacontracts: dvr_window_s out of range")
	case regions != nil && !validRegions(*regions):
		return errors.New("mediacontracts: regions are 1 to 9 broad regions")
	case maxParticipants != nil && (*maxParticipants < 1 || *maxParticipants > 100_000):
		return errors.New("mediacontracts: max_participants out of range")
	case metadata != nil && len(*metadata) > 2_048:
		return errors.New("mediacontracts: metadata over 2,048 bytes")
	case startGrace != nil && (*startGrace < SessionStartGraceMinS || *startGrace > SessionStartGraceMaxS):
		return errors.New("mediacontracts: start_grace_s out of range")
	case idleEnd != nil && (*idleEnd < SessionIdleEndMinS || *idleEnd > SessionIdleEndMaxS):
		return errors.New("mediacontracts: idle_end_s out of range")
	}
	return nil
}

func ValidateMediaChannel(channel MediaChannel) error {
	if !ChannelIDPattern.MatchString(channel.ID) {
		return ErrChannelID
	}
	if !mediaProjectIDPattern.MatchString(channel.ProjectID) {
		return errors.New("mediacontracts: invalid project id")
	}
	if channel.CurrentSessionID != nil && !mediaSessionIDPattern.MatchString(*channel.CurrentSessionID) {
		return errors.New("mediacontracts: invalid current_session_id")
	}
	if !validChannelName(channel.Name) {
		return errors.New("mediacontracts: channel name of 1 to 128 characters")
	}
	if channel.StandbyError != nil && (!apiErrorCodePattern.MatchString(channel.StandbyError.Error) || channel.StandbyError.RetryAt == "") {
		return errors.New("mediacontracts: invalid standby_error")
	}
	template := channel.Template
	if template.Protection.Mode != "public" && template.Protection.Mode != "protected" || template.Protection.KeyProvider != nil {
		return errors.New("mediacontracts: channel protection is public or protected, without a key provider")
	}
	dvr, grace := template.Recording.DVRWindowS, template.StartGraceS
	if err := validateTemplateBounds(&dvr, template.Regions, template.MaxParticipants, template.Metadata, &grace, template.IdleEndS); err != nil {
		return err
	}
	if template.Ladder != nil {
		return ValidateLadder(*template.Ladder)
	}
	return nil
}

func ValidateChannelTemplateRequest(template ChannelTemplateRequest) error {
	if template.Protection != nil && template.Protection.Mode != "public" && template.Protection.Mode != "protected" {
		return errors.New("mediacontracts: channel protection is public or protected")
	}
	var dvr *int64
	if template.Recording != nil {
		dvr = template.Recording.DVRWindowS
	}
	var regions *[]string
	if template.Regions != nil {
		regions = &template.Regions
	}
	if err := validateTemplateBounds(dvr, regions, template.MaxParticipants, template.Metadata, template.StartGraceS, template.IdleEndS); err != nil {
		return err
	}
	if template.Ladder != nil {
		return ValidateLadderRequest(*template.Ladder)
	}
	return nil
}

func ValidateCreateChannelRequest(request CreateChannelRequest) error {
	if !mediaProjectIDPattern.MatchString(request.ProjectID) {
		return errors.New("mediacontracts: invalid project_id")
	}
	if !validChannelName(request.Name) {
		return errors.New("mediacontracts: channel name of 1 to 128 characters")
	}
	if request.Template != nil {
		return ValidateChannelTemplateRequest(*request.Template)
	}
	return nil
}

func ValidateUpdateChannelRequest(request UpdateChannelRequest) error {
	if request.Name != nil && !validChannelName(*request.Name) {
		return errors.New("mediacontracts: channel name of 1 to 128 characters")
	}
	if request.Template != nil {
		return ValidateChannelTemplateRequest(*request.Template)
	}
	return nil
}
