package broadcast

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

const (
	MultivariantName = "master.m3u8"
	PlaylistName     = "playlist.m3u8"
	ManifestName     = "manifest.json"

	ContentTypePlaylist = "application/vnd.apple.mpegurl"
	ContentTypeMP4      = "video/mp4"
	ContentTypeJSON     = "application/json"

	CacheImmutable = "public, max-age=31536000, immutable"
	CacheLive      = "public, max-age=1"
	CachePrivate   = "private, max-age=2"
)

type HLSAccess struct {
	Child        string
	Nonce        string
	Identity     string
	Credential   string
	IssuedMS     int64
	Kind, Role   string
	TokenIAT     int64
	Token, Grant string
}

func AuthorizeMultivariant(node *Node, request *http.Request, now time.Time) (HLSAccess, int, string) {
	if bearer := Bearer(request); bearer != "" {
		if child, err := node.VerifyChild(bearer, now); err == nil {
			if node.Expired(now) {
				return HLSAccess{}, http.StatusServiceUnavailable, "topology_expired"
			}
			return HLSAccess{Child: child}, 0, ""
		}
	}
	token := request.URL.Query().Get(mc.HLSTokenQueryParameter)
	if token == "" {
		token = Bearer(request)
	}
	if token == "" {
		return HLSAccess{}, http.StatusUnauthorized, mc.ErrTokenMalformed
	}
	switch {
	case node.Expired(now):
		return HLSAccess{}, http.StatusServiceUnavailable, "topology_expired"
	case node.Draining():
		return HLSAccess{}, http.StatusServiceUnavailable, "draining"
	}
	claims, err := node.VerifyToken(token, now)
	if err == nil {
		err = mc.AuthorizeToken(claims, mc.RoleSubscribe, "")
	}
	if err != nil {
		code := mc.TokenErrorCode(err)
		if code == mc.ErrTokenRoleForbidden || code == mc.ErrTokenTrackForbidden {
			return HLSAccess{}, http.StatusForbidden, code
		}
		if code == "" {
			code = mc.ErrTokenMalformed
		}
		return HLSAccess{}, http.StatusUnauthorized, code
	}
	credential, err := mc.MintPlaybackCredential(node.PlaybackSecret(), mc.PlaybackCredential{SessionID: node.SessionID,
		Identity: claims.Identity, Nonce: claims.Nonce, IssuedAtMS: now.UnixMilli()})
	if err != nil {
		return HLSAccess{}, http.StatusInternalServerError, "invalid_request"
	}
	return HLSAccess{Nonce: claims.Nonce, Identity: claims.Identity, Credential: credential, IssuedMS: now.UnixMilli(),
		Kind: mc.ParticipantKindOf(claims), Role: claims.Role, TokenIAT: claims.IssuedAt, Token: token, Grant: usage.RequestGrant(request)}, 0, ""
}

func AuthorizeMediaPlaylist(node *Node, request *http.Request, now time.Time) (HLSAccess, int, string) {
	value := request.URL.Query().Get(mc.PlaybackQueryParameter)
	if bearer := Bearer(request); bearer != "" {
		child, err := node.VerifyChild(bearer, now)
		if err == nil {
			if node.Expired(now) {
				return HLSAccess{}, http.StatusServiceUnavailable, "topology_expired"
			}
			return HLSAccess{Child: child}, 0, ""
		}
		if value == "" {
			code := mc.TokenErrorCode(err)
			if code == "" {
				code = mc.ErrTokenRoleForbidden
			}
			return HLSAccess{}, http.StatusUnauthorized, code
		}
	}
	credential, err := mc.VerifyPlaybackCredential(value, node.PlaybackSecret(), node.SessionID, now.UnixMilli())
	switch {
	case errors.Is(err, mc.ErrCredentialMalformed):
		return HLSAccess{}, http.StatusUnauthorized, "credential_malformed"
	case errors.Is(err, mc.ErrCredentialInvalid):
		return HLSAccess{}, http.StatusUnauthorized, "credential_invalid"
	case errors.Is(err, mc.ErrCredentialSessionMismatch):
		return HLSAccess{}, http.StatusUnauthorized, "credential_session_mismatch"
	case errors.Is(err, mc.ErrCredentialExpired):
		return HLSAccess{}, http.StatusUnauthorized, "credential_expired"
	case err != nil:
		return HLSAccess{}, http.StatusUnauthorized, "credential_invalid"
	}
	if node.Revoked(credential.Nonce, credential.Identity, now) {
		return HLSAccess{}, http.StatusUnauthorized, mc.ErrTokenRevoked
	}
	return HLSAccess{Nonce: credential.Nonce, Identity: credential.Identity, IssuedMS: credential.IssuedAtMS}, 0, ""
}

func RewriteMultivariant(body []byte, credential string) ([]byte, error) {
	if credential == "" {
		return body, nil
	}
	var parsed playlist.Multivariant
	if err := parsed.Unmarshal(body); err != nil {
		return nil, err
	}
	with := func(uri string) string {
		separator := "?"
		if strings.Contains(uri, "?") {
			separator = "&"
		}
		return uri + separator + mc.PlaybackQueryParameter + "=" + url.QueryEscape(credential)
	}
	for _, variant := range parsed.Variants {
		variant.URI = with(variant.URI)
	}
	for _, rendition := range parsed.Renditions {
		if rendition.URI != nil {
			value := with(*rendition.URI)
			rendition.URI = &value
		}
	}
	return parsed.Marshal()
}

type Blocking struct {
	MSN  int64
	Part int64
	Skip bool
	Page int64
}

func ParseBlocking(query url.Values) (Blocking, error) {
	blocking := Blocking{MSN: -1, Part: -1, Page: -1}
	parse := func(name string, target *int64) error {
		raw := query.Get(name)
		if raw == "" {
			return nil
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			return errors.New("invalid " + name)
		}
		*target = value
		return nil
	}
	if err := parse("_HLS_msn", &blocking.MSN); err != nil {
		return blocking, err
	}
	if err := parse("_HLS_part", &blocking.Part); err != nil {
		return blocking, err
	}
	if err := parse("page", &blocking.Page); err != nil {
		return blocking, err
	}
	if blocking.Part >= 0 && blocking.MSN < 0 {
		return blocking, errors.New("_HLS_part requires _HLS_msn")
	}
	skip := query.Get("_HLS_skip")
	blocking.Skip = skip == "YES" || skip == "v2"
	return blocking, nil
}

func (b Blocking) Query() string {
	values := url.Values{}
	if b.MSN >= 0 {
		values.Set("_HLS_msn", strconv.FormatInt(b.MSN, 10))
	}
	if b.Part >= 0 {
		values.Set("_HLS_part", strconv.FormatInt(b.Part, 10))
	}
	if b.Skip {
		values.Set("_HLS_skip", "YES")
	}
	if b.Page >= 0 {
		values.Set("page", strconv.FormatInt(b.Page, 10))
	}
	return values.Encode()
}

type Viewers struct {
	mu      sync.Mutex
	byIP    map[string]viewer
	players map[string]time.Time
}

const playerIdle = 30 * time.Second

func (v *Viewers) Players(now time.Time) int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	for nonce, at := range v.players {
		if now.Sub(at) > playerIdle {
			delete(v.players, nonce)
		}
	}
	return int64(len(v.players))
}

type viewer struct {
	nonce, identity string
	seen            time.Time
}

func (v *Viewers) Learn(client string, access HLSAccess) {
	if access.Nonce == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.byIP == nil {
		v.byIP = map[string]viewer{}
	}
	now := time.Now()
	v.byIP[client] = viewer{nonce: access.Nonce, identity: access.Identity, seen: now}
	if v.players == nil {
		v.players = map[string]time.Time{}
	}
	v.players[access.Nonce] = now
	if len(v.byIP) > 200_000 {
		cutoff := time.Now().Add(-2 * time.Minute)
		for key, entry := range v.byIP {
			if entry.seen.Before(cutoff) {
				delete(v.byIP, key)
			}
		}
	}
}

func (v *Viewers) Connection(meter *Usage, request *http.Request, access HLSAccess) *Connection {
	if access.Child != "" {
		return meter.RequestV1("child:"+access.Child, nil, StringPtr(mc.NodeIdentityPrefix+access.Child), DirectionEgress, TransportChild)
	}
	nonce, identity := access.Nonce, access.Identity
	client := ClientKey(request)
	if nonce == "" {
		v.mu.Lock()
		entry, ok := v.byIP[client]
		v.mu.Unlock()
		if !ok {
			return meter.RequestV1("anonymous", nil, nil, DirectionEgress, TransportHLS)
		}
		nonce, identity = entry.nonce, entry.identity
	} else {
		v.Learn(client, access)
	}
	return meter.RequestV1("hls:"+nonce, StringPtr(nonce), StringPtr(identity), DirectionEgress, TransportHLS)
}

func ServeBytes(response http.ResponseWriter, request *http.Request, body []byte, contentType, cacheControl string) int {
	header := response.Header()
	header.Set("Content-Type", contentType)
	header.Set("Cache-Control", cacheControl)
	header.Set("Content-Length", strconv.Itoa(len(body)))
	response.WriteHeader(http.StatusOK)
	if request.Method == http.MethodHead {
		return 0
	}
	n, _ := response.Write(body)
	return n
}

func Unbounded(response http.ResponseWriter) {
	controller := http.NewResponseController(response)
	_ = controller.SetReadDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Time{})
}

const holdMargin = 15 * time.Second

func Hold(response http.ResponseWriter, limit time.Duration) {
	controller := http.NewResponseController(response)
	deadline := time.Now().Add(limit + holdMargin)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
}
