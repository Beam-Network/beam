package edge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
)

const (
	multivariantTTL = 2 * time.Second
	liveTTL         = 500 * time.Millisecond
	blockingTTL     = 6 * time.Second
	upstreamTimeout = 40 * time.Second
	maxObjectBytes  = 64 << 20
	upstreamMargin  = 10 * time.Second
)

type Role struct {
	env     *broadcast.Env
	node    *broadcast.Node
	cache   *Cache
	viewers broadcast.Viewers
	players *broadcast.Players
	holds   broadcast.HoldLimiter
	maxPart int64
	timeout time.Duration
	ctx     context.Context

	mu      sync.Mutex
	flights map[string]*flight
}

type flight struct {
	done   chan struct{}
	status int
	body   []byte
	err    error
}

func Run(ctx context.Context, env *broadcast.Env) error {
	config := env.Config
	role := &Role{env: env, node: env.Node, cache: sharedCacheFor(config.CacheDir, config.CacheBytes, config.CacheMemoryBytes),
		ctx: ctx, flights: map[string]*flight{}, maxPart: broadcast.MaxPartIndex(env.Node.Topology()),
		timeout: max(upstreamTimeout, broadcast.HoldLimit(env.Node.Topology())+upstreamMargin),
		players: broadcast.NewPlayers(env.Usage.Reporter(), env.Node, env.StateDir)}
	defer role.players.Start(ctx)()
	remove := env.Router.Handle(&broadcast.Route{Kind: broadcast.RouteHLS, SessionID: role.node.SessionID, Node: role.node,
		Handler: http.HandlerFunc(role.serve)})
	defer remove()
	announce := func() {
		env.Announce([]mc.TopologyEndpoint{{Kind: "hls", URL: env.URL("/v1/hls/" + role.node.SessionID)}})
	}
	announce()
	env.Players = role.viewers.Players
	go env.RunLoad(ctx, 2*time.Second, announce)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-role.node.Changed():
			if _, present := role.node.Self(); !present {
				return nil
			}
		}
	}
}

func (r *Role) serve(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/v1/hls/"+r.node.SessionID+"/"+broadcast.ReceiptName {
		r.players.ServeReceipt(response, request)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "GET only")
		return
	}
	now := time.Now()
	rest := strings.TrimPrefix(request.URL.Path, "/v1/hls/"+r.node.SessionID+"/")
	if rest == "" || strings.Contains(rest, "..") {
		broadcast.WriteError(response, http.StatusNotFound, "not_found", "")
		return
	}
	blocking, err := broadcast.ParseBlocking(request.URL.Query())
	if err != nil {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var access broadcast.HLSAccess
	var answer *broadcast.Answer
	written, arrived := 0, r.players.Now()
	defer func() { answer.Done(written) }()
	contentType, cacheControl := broadcast.ContentTypeMP4, broadcast.CacheImmutable
	ttl := time.Duration(0)
	upstreamQuery := ""
	switch {
	case rest == broadcast.MultivariantName:
		var status int
		var code string
		access, status, code = broadcast.AuthorizeMultivariant(r.node, request, now)
		if status != 0 {
			broadcast.WriteError(response, status, code, "")
			return
		}
		answer = r.players.Multivariant(&access, arrived)
		contentType, cacheControl, ttl = broadcast.ContentTypePlaylist, broadcast.CachePrivate, multivariantTTL
		if access.Child != "" {
			cacheControl = "public, max-age=2"
		}
	case strings.HasSuffix(rest, "/"+broadcast.PlaylistName):
		var status int
		var code string
		access, status, code = broadcast.AuthorizeMediaPlaylist(r.node, request, now)
		if status != 0 {
			broadcast.WriteError(response, status, code, "")
			return
		}
		if answer, code = r.players.Playlist(access, renditionOf(rest), arrived); code != "" {
			broadcast.WriteError(response, http.StatusForbidden, code, "")
			return
		}
		contentType, cacheControl, ttl, upstreamQuery = broadcast.ContentTypePlaylist, broadcast.CacheLive, liveTTL, blocking.Query()
		if blocking.MSN >= 0 {
			ttl = blockingTTL
		}
	case strings.HasSuffix(rest, "/"+broadcast.ManifestName):
		contentType, cacheControl, ttl, upstreamQuery = broadcast.ContentTypeJSON, broadcast.CacheLive, liveTTL, blocking.Query()
		if blocking.MSN >= 0 {
			ttl = blockingTTL
		}
		if blocking.Page >= 0 {
			ttl = 0
			cacheControl = broadcast.CacheImmutable
		}
		access = r.childAccess(request, now)
		answer = r.players.Object(request, access, renditionOf(rest), false, arrived)
	case mc.InitURIPattern.MatchString(rest[strings.LastIndexByte(rest, '/')+1:]) || strings.HasSuffix(rest, ".m4s"):
		access = r.childAccess(request, now)
		answer = r.players.Object(request, access, renditionOf(rest), strings.HasSuffix(rest, ".m4s"), arrived)
	default:
		broadcast.WriteError(response, http.StatusNotFound, "not_found", "")
		return
	}
	if blocking.Part > r.maxPart || partIndex(rest) > r.maxPart {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "part index out of range")
		return
	}
	key := r.node.SessionID + "/" + rest
	if upstreamQuery != "" {
		key += "?" + upstreamQuery
	}
	status, body, err := r.fetchHeld(response, request, access, key, rest, upstreamQuery, ttl, now)
	if errors.Is(err, errRateLimited) {
		broadcast.WriteRateLimited(response)
		return
	}
	if err != nil {
		code := "upstream_unavailable"
		statusCode := http.StatusBadGateway
		if errors.Is(err, errExpired) {
			code, statusCode = "topology_expired", http.StatusServiceUnavailable
		}
		broadcast.WriteError(response, statusCode, code, "")
		return
	}
	if status != http.StatusOK {
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		response.WriteHeader(status)
		_, _ = response.Write(body)
		return
	}
	if strings.HasSuffix(rest, "/"+broadcast.PlaylistName) {
		body = r.players.Tag(body, access)
	}
	if rest == broadcast.MultivariantName {
		r.players.LearnVariants(body)
		rewritten, err := broadcast.RewriteMultivariant(body, access.Credential)
		if err != nil {
			broadcast.WriteError(response, http.StatusBadGateway, "upstream_unavailable", "invalid multivariant playlist")
			return
		}
		body = rewritten
	}
	connection := r.viewers.Connection(r.env.Usage, request, access)
	written = broadcast.ServeBytes(response, request, body, contentType, cacheControl)
	connection.Add(written)
	r.env.EgressB.Add(int64(written))
}

func renditionOf(rest string) string {
	rendition, _, _ := strings.Cut(rest, "/")
	return rendition
}

func (r *Role) childAccess(request *http.Request, now time.Time) broadcast.HLSAccess {
	if bearer := broadcast.Bearer(request); bearer != "" {
		if child, err := r.node.VerifyChild(bearer, now); err == nil {
			return broadcast.HLSAccess{Child: child}
		}
	}
	return broadcast.HLSAccess{}
}

var (
	errExpired     = errors.New("topology view expired")
	errRateLimited = errors.New("too many held requests")
)

func partIndex(rest string) int64 {
	name := rest[strings.LastIndexByte(rest, '/')+1:]
	var msn, part int64
	if _, err := fmt.Sscanf(name, "seg_%d.%d.m4s", &msn, &part); err == nil && fmt.Sprintf("seg_%d.%d.m4s", msn, part) == name {
		return part
	}
	return -1
}

func (r *Role) fetchHeld(response http.ResponseWriter, request *http.Request, access broadcast.HLSAccess, key, rest, query string, ttl time.Duration, now time.Time) (int, []byte, error) {
	if body, ok := r.cache.Get(key); ok {
		return http.StatusOK, body, nil
	}
	release, ok := r.holds.Acquire(broadcast.HolderOf(request, access))
	if !ok {
		return 0, nil, errRateLimited
	}
	defer release()
	broadcast.Hold(response, r.timeout)
	return r.fetch(key, rest, query, ttl, now)
}

func (r *Role) fetch(key, rest, query string, ttl time.Duration, now time.Time) (int, []byte, error) {
	if body, ok := r.cache.Get(key); ok {
		return http.StatusOK, body, nil
	}
	if r.node.Expired(now) {
		return 0, nil, errExpired
	}
	r.mu.Lock()
	current := r.flights[key]
	if current == nil {
		current = &flight{done: make(chan struct{})}
		r.flights[key] = current
		r.mu.Unlock()
		current.status, current.body, current.err = r.upstream(rest, query)
		if current.err == nil && current.status == http.StatusOK {
			r.cache.Put(key, current.body, ttl)
		}
		r.mu.Lock()
		delete(r.flights, key)
		r.mu.Unlock()
		close(current.done)
	} else {
		r.mu.Unlock()
		<-current.done
	}
	return current.status, current.body, current.err
}

func (r *Role) upstream(rest, query string) (int, []byte, error) {
	parent, ok := r.node.Parent()
	endpoint := broadcast.Endpoint(parent, "hls")
	if !ok || endpoint == "" {
		return 0, nil, errors.New("no parent with an hls endpoint")
	}
	target := strings.TrimRight(endpoint, "/") + "/" + rest
	if query != "" {
		target += "?" + query
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+r.node.ServiceToken())
	received := 0
	link := r.players.Upstream(parent.NodeID, r.players.Now())
	defer func() { link.Done(received) }()
	response, err := broadcast.UpstreamClient(r.env.Config, parent.TLSCertificateSHA256).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := broadcast.ReadLimited(response.Body, maxObjectBytes)
	if response.StatusCode == http.StatusOK {
		received = len(body)
	}
	if err != nil {
		return 0, nil, err
	}
	upstream := r.env.Usage.RequestV1("upstream:"+parent.NodeID, nil, broadcast.StringPtr(mc.NodeIdentityPrefix+parent.NodeID),
		broadcast.DirectionIngress, broadcast.TransportUpstream)
	upstream.Add(len(body))
	r.env.IngressB.Add(int64(len(body)))
	if response.StatusCode >= 500 && response.StatusCode != http.StatusServiceUnavailable {
		return 0, nil, errors.New("parent answered " + response.Status)
	}
	return response.StatusCode, body, nil
}
