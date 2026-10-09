package roomworkloads

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/handlers/workertls"
)

const (
	roomMessagesPath = "room-messages"
	roomStreamsPath  = "room-streams"
)

type DirectServerConfig struct {
	ListenAddress string
	AdvertiseURL  string
}

// DirectServer is the Worker's single TLS listener for Worker-hosted room
// messages and streams. Both kinds share one short-lived certificate store, so
// agents pin one fingerprint per lease; sessions are multiplexed by path.
type DirectServer struct {
	config DirectServerConfig
	now    func() time.Time

	mu           sync.Mutex
	listener     net.Listener
	http         *http.Server
	baseURL      string
	certificates *workertls.Certificates

	sessionsMu sync.RWMutex
	messages   map[string]*messageSession
	streams    map[string]*streamSession
}

func NewDirectServer(config DirectServerConfig) *DirectServer {
	return &DirectServer{config: config, now: time.Now, messages: make(map[string]*messageSession),
		streams: make(map[string]*streamSession)}
}

// ValidateDirectServerConfig requires a host:port listen address and a clean
// HTTPS advertise URL. {port} expands to the bound listener port.
func ValidateDirectServerConfig(config DirectServerConfig) error {
	if _, _, err := net.SplitHostPort(strings.TrimSpace(config.ListenAddress)); err != nil {
		return errors.New("direct room listen address must be host:port")
	}
	parsed, err := url.Parse(strings.ReplaceAll(config.AdvertiseURL, "{port}", "1"))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("direct room endpoints require a clean HTTPS advertise URL")
	}
	return nil
}

// Listen binds the listener once; later calls reuse it.
func (s *DirectServer) Listen() error {
	_, _, err := s.ready()
	return err
}

func (s *DirectServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.http == nil {
		return nil
	}
	return s.http.Close()
}

func (s *DirectServer) ready() (string, *workertls.Certificates, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.http != nil {
		return s.baseURL, s.certificates, nil
	}
	if err := ValidateDirectServerConfig(s.config); err != nil {
		return "", nil, err
	}
	listener, err := net.Listen("tcp", s.config.ListenAddress)
	if err != nil {
		return "", nil, err
	}
	baseURL, err := advertisedURL(s.config.AdvertiseURL, listener.Addr())
	if err != nil {
		_ = listener.Close()
		return "", nil, err
	}
	secure, certificates := workertls.WrapListener(listener, s.now)
	// The longest request is a 25 s long poll returning up to 4 MiB of frames.
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: time.Minute,
		WriteTimeout: time.Minute, IdleTimeout: 90 * time.Second}
	s.listener, s.baseURL, s.certificates = secure, baseURL, certificates
	server := s.http
	go func() { _ = server.Serve(secure) }()
	return baseURL, certificates, nil
}

func directSessionURL(baseURL, kindPath, sessionID string) string {
	return strings.TrimRight(baseURL, "/") + "/v1/" + kindPath + "/" + url.PathEscape(sessionID)
}

func (s *DirectServer) registerMessage(id string, session *messageSession) error {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if _, exists := s.messages[id]; exists {
		return errors.New("direct room message session is already active")
	}
	s.messages[id] = session
	return nil
}

func (s *DirectServer) unregisterMessage(id string) {
	s.sessionsMu.Lock()
	delete(s.messages, id)
	s.sessionsMu.Unlock()
}

func (s *DirectServer) registerStream(id string, session *streamSession) error {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if _, exists := s.streams[id]; exists {
		return errors.New("direct room stream session is already active")
	}
	s.streams[id] = session
	return nil
}

func (s *DirectServer) unregisterStream(id string, session *streamSession) {
	s.sessionsMu.Lock()
	if s.streams[id] == session {
		delete(s.streams, id)
	}
	s.sessionsMu.Unlock()
}

func (s *DirectServer) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.Trim(request.URL.EscapedPath(), "/"), "/")
	if len(parts) < 3 || parts[0] != "v1" {
		http.NotFound(response, request)
		return
	}
	for index := range parts {
		unescaped, err := url.PathUnescape(parts[index])
		if err != nil {
			http.NotFound(response, request)
			return
		}
		parts[index] = unescaped
	}
	switch parts[1] {
	case roomMessagesPath:
		s.sessionsMu.RLock()
		session := s.messages[parts[2]]
		s.sessionsMu.RUnlock()
		serveMessage(session, response, request, parts[3:])
	case roomStreamsPath:
		s.sessionsMu.RLock()
		session := s.streams[parts[2]]
		s.sessionsMu.RUnlock()
		serveStream(session, response, request, parts[3:])
	default:
		http.NotFound(response, request)
	}
}

func advertisedURL(configured string, address net.Addr) (string, error) {
	_, port, err := net.SplitHostPort(address.String())
	if err != nil {
		return "", err
	}
	value := strings.ReplaceAll(configured, "{port}", port)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("direct room advertise URL is invalid")
	}
	return strings.TrimRight(value, "/"), nil
}

// newAccessToken returns 32 random bytes in unpadded base64url.
func newAccessToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
