package broadcast

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/worker/planeapi/shared"
	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const (
	RouteIngest = "ingest"
	RoutePull   = "pull"
	RouteHLS    = "hls"
)

type Route struct {
	Kind      string
	SessionID string
	Node      *Node
	Match     func(rest string) bool
	Handler   http.Handler
}

type Router struct {
	mu     sync.RWMutex
	routes []*Route
}

func (s *Router) Handle(route *Route) func() {
	s.mu.Lock()
	s.routes = append(s.routes, route)
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, existing := range s.routes {
			if existing == route {
				s.routes = append(s.routes[:i], s.routes[i+1:]...)
				return
			}
		}
	}
}

var roleRank = map[string]int{mc.RoleEdge: 3, mc.RoleShield: 2, mc.RolePackage: 1}

func (s *Router) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	SetCORS(response)
	if request.Method == http.MethodOptions {
		response.WriteHeader(http.StatusNoContent)
		return
	}
	kind, session, rest, ok := splitPath(request.URL.Path)
	if !ok {
		WriteError(response, http.StatusNotFound, "not_found", "")
		return
	}
	s.mu.RLock()
	var candidates []*Route
	for index := len(s.routes) - 1; index >= 0; index-- {
		route := s.routes[index]
		if route.Kind == kind && route.SessionID == session && (route.Match == nil || route.Match(rest)) {
			candidates = append(candidates, route)
		}
	}
	s.mu.RUnlock()
	if len(candidates) == 0 {
		WriteError(response, http.StatusNotFound, "not_found", "unknown session")
		return
	}
	chosen := candidates[0]
	if len(candidates) > 1 {
		chosen = pickRoute(candidates, request)
	}
	chosen.Handler.ServeHTTP(response, request)
}

func pickRoute(candidates []*Route, request *http.Request) *Route {
	if request.URL.Query().Has(mc.IngestGrantQueryParameter) {
		return mostDownstream(candidates)
	}
	if token := Bearer(request); token != "" {
		if claims, _, _, err := mc.ParseTokenUnverified(token); err == nil && strings.HasPrefix(claims.Identity, mc.NodeIdentityPrefix) {
			child := strings.TrimPrefix(claims.Identity, mc.NodeIdentityPrefix)
			for _, route := range candidates {
				if route.Node.IsChild(child) {
					return route
				}
			}
		}
	}
	return mostDownstream(candidates)
}

func mostDownstream(candidates []*Route) *Route {
	best := candidates[0]
	for _, route := range candidates[1:] {
		if roleRank[route.Node.Role] > roleRank[best.Node.Role] {
			best = route
		}
	}
	return best
}

func splitPath(path string) (kind, session, rest string, ok bool) {
	if !strings.HasPrefix(path, "/v1/") {
		return "", "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/v1/"), "/", 3)
	if len(parts) < 2 || parts[1] == "" {
		return "", "", "", false
	}
	session, err := url.PathUnescape(parts[1])
	if err != nil {
		return "", "", "", false
	}
	if len(parts) == 3 {
		rest = parts[2]
	}
	return parts[0], session, rest, true
}

func SetCORS(response http.ResponseWriter) {
	header := response.Header()
	header.Set("Access-Control-Allow-Origin", "*")
	header.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Range, Beam-Track-Sources, If-Match")
	header.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PATCH, DELETE, OPTIONS")
	header.Set("Access-Control-Expose-Headers", "Location, Link, ETag, Content-Length, Content-Range")
	header.Set("Access-Control-Max-Age", "600")
}

func WriteError(response http.ResponseWriter, status int, code, detail string) {
	body := mc.APIError{Error: code}
	if detail != "" {
		body.Detail = &detail
	}
	encoded, _ := mc.MarshalWire(body)
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_, _ = response.Write(encoded)
}

func WriteTokenError(response http.ResponseWriter, err error) {
	code := mc.TokenErrorCode(err)
	switch {
	case errors.Is(err, ErrNotChild):
		WriteError(response, http.StatusForbidden, mc.ErrTokenRoleForbidden, "not a child node")
	case code == mc.ErrTokenRoleForbidden || code == mc.ErrTokenTrackForbidden:
		WriteError(response, http.StatusForbidden, code, "")
	case code != "":
		WriteError(response, http.StatusUnauthorized, code, "")
	default:
		WriteError(response, http.StatusUnauthorized, mc.ErrTokenMalformed, "")
	}
}

func Bearer(request *http.Request) string {
	value := request.Header.Get("Authorization")
	if len(value) > 7 && strings.EqualFold(value[:7], "bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return ""
}

func ClientKey(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = request.RemoteAddr
	}
	digest := sha256.Sum256([]byte(host + "\x00" + request.UserAgent()))
	return hex.EncodeToString(digest[:12])
}

var clients struct {
	sync.Mutex
	byPin map[string]*http.Client
}

func UpstreamClient(config Config, pin *string) *http.Client {
	key := ""
	if pin != nil {
		key = *pin
	}
	clients.Lock()
	defer clients.Unlock()
	if clients.byPin == nil {
		clients.byPin = map[string]*http.Client{}
	}
	if client := clients.byPin[key]; client != nil {
		return client
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if pin != nil {
		expected := strings.ToLower(*pin)
		tlsConfig.InsecureSkipVerify = true
		tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("parent sent no certificate")
			}
			digest := sha256.Sum256(state.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(digest[:])), []byte(expected)) != 1 {
				return errors.New("parent certificate does not match its pin")
			}
			return nil
		}
	} else if config.UpstreamInsecure {
		tlsConfig.InsecureSkipVerify = true
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true,
		DialContext:         shared.TransportDial((&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext),
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 45 * time.Second, MaxIdleConnsPerHost: 64,
		IdleConnTimeout: 90 * time.Second}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("media upstreams do not redirect")
	}}
	clients.byPin[key] = client
	return client
}

func Endpoint(node mc.TopologyNode, kind string) string {
	for _, endpoint := range node.Endpoints {
		if endpoint.Kind == kind {
			return endpoint.URL
		}
	}
	return ""
}
