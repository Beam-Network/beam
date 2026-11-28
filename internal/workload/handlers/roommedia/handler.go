package roommedia

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	planeshared "github.com/Beam-Network/beam/internal/worker/planeapi/shared"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	sfuv2 "github.com/Beam-Network/beam/internal/workload/handlers/roommedia/sfu"
	"github.com/Beam-Network/beam/internal/workload/handlers/workertls"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

type Config struct {
	ListenAddress           string
	AdvertiseURL            string
	PublicIP                string
	UDPPortMin              uint16
	UDPPortMax              uint16
	ICEServers              []string
	TURNSecret              string
	TURNCredentialTTL       time.Duration
	TURNUsernamePrefix      string
	MaxViewers              int
	PublisherReconnectGrace time.Duration

	Fleet         Fleet
	WatchTopology TopologyWatch
	NodeTLS       *NodeTLS
	StateDir      string
}

type Handler struct {
	config Config
	now    func() time.Time

	serverMu   sync.Mutex
	server     *sharedServer
	nodeServer *sharedServer

	attemptsMu sync.Mutex
	attempts   map[string]uint64

	rtcMu    sync.Mutex
	factory  *sfuv2.APIFactory
	sessions map[*sfuv2.Session]struct{}
	cpu      cpuMeter

	stopPrune     chan struct{}
	stopPruneOnce sync.Once
}

type sharedServer struct {
	listener     net.Listener
	http         *http.Server
	baseURL      string
	certificates *workertls.Certificates

	mu       sync.RWMutex
	sessions map[string]http.Handler
	mounts   map[string]mountEntry
	done     chan struct{}
	err      error
}

func NewHandler(config Config) *Handler {
	if strings.TrimSpace(config.ListenAddress) == "" {
		config.ListenAddress = "127.0.0.1:0"
	}
	if config.MaxViewers <= 0 {
		config.MaxViewers = 64
	}
	if config.TURNCredentialTTL <= 0 {
		config.TURNCredentialTTL = 3 * time.Hour
	}
	if strings.TrimSpace(config.TURNUsernamePrefix) == "" {
		config.TURNUsernamePrefix = "beam"
	}
	config.PublisherReconnectGrace, _ = sfuv2.ClampPublisherGrace(config.PublisherReconnectGrace)
	handler := &Handler{config: config, now: time.Now, sessions: map[*sfuv2.Session]struct{}{}, attempts: map[string]uint64{},
		stopPrune: make(chan struct{})}
	if strings.TrimSpace(config.StateDir) != "" {
		go handler.runStatePrune(handler.stopPrune)
	}
	return handler
}

func (*Handler) Kind() domain.Kind { return domain.KindRoomMedia }

func (h *Handler) Validate(spec domain.Spec) error {
	if profile := peekProfile(spec); isNodeProfile(profile) {
		return h.validateNodeSpec(spec)
	}
	worker, err := decodeSpec(spec)
	if err != nil {
		return err
	}
	if worker.Details.Profile != contracts.RoomMediaWebRTCWorkerProfile {
		return errors.New("room media worker requires the webrtc-worker-v1 profile")
	}
	if worker.Details.Protection.Scheme != contracts.RoomMediaProtectionSchemeV1 ||
		worker.Details.Protection.KeyScope != contracts.RoomMediaProtectionChannel || !worker.Details.Protection.Required {
		return errors.New("room media worker requires channel-scoped E2EE protection")
	}
	if worker.Details.SessionID == "" || len(worker.Details.Tracks) == 0 {
		return errors.New("room media worker requires a session and declared tracks")
	}
	targets := make([]string, 0, len(worker.Targets))
	for _, target := range worker.Targets {
		targets = append(targets, target.MemberID)
	}
	if err := contracts.ValidateWebRTCMediaAdmission(worker.Identity, targets, worker.Details, h.now().UTC()); err != nil {
		return err
	}
	return h.validateListener()
}

func (h *Handler) validateListener() error {
	if _, _, err := net.SplitHostPort(h.config.ListenAddress); err != nil {
		return errors.New("room media listen address must be host:port")
	}
	if err := ValidateAdvertiseURL(h.config.AdvertiseURL); err != nil {
		return err
	}
	if (h.config.UDPPortMin == 0) != (h.config.UDPPortMax == 0) ||
		(h.config.UDPPortMin != 0 && h.config.UDPPortMin > h.config.UDPPortMax) {
		return errors.New("room media UDP port range is invalid")
	}
	return nil
}

func (h *Handler) validateNodeSpec(spec domain.Spec) error {
	worker, view, err := decodeNodeSpec(spec)
	if err != nil {
		return err
	}
	if err := validateNode(worker, view, h.now().UTC()); err != nil {
		return err
	}
	if nodeRunner(worker.Details.Profile) == nil {
		return errors.New("this Worker does not serve " + worker.Details.Profile + " nodes")
	}
	return h.validateNodeListener()
}

func (h *Handler) validateNodeListener() error {
	if h.config.NodeTLS == nil || strings.TrimSpace(h.config.NodeTLS.Addr) == "" {
		return h.validateListener()
	}
	if _, _, err := net.SplitHostPort(h.config.NodeTLS.Addr); err != nil {
		return errors.New("media HTTPS address must be host:port")
	}
	if (h.config.UDPPortMin == 0) != (h.config.UDPPortMax == 0) ||
		(h.config.UDPPortMin != 0 && h.config.UDPPortMin > h.config.UDPPortMax) {
		return errors.New("room media UDP port range is invalid")
	}
	return nil
}

func (h *Handler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	if profile := peekProfile(spec); isNodeProfile(profile) {
		if err := h.validateNodeSpec(spec); err != nil {
			return domain.Result{}, err
		}
		return h.runNode(ctx, spec)
	}
	worker, err := decodeSpec(spec)
	if err != nil {
		return domain.Result{}, err
	}
	if err := h.Validate(spec); err != nil {
		return domain.Result{}, err
	}
	media, err := newSFU(h.config, worker.Details.SessionID, worker.Identity.ExpiresAt)
	if err != nil {
		return domain.Result{}, err
	}
	defer media.Close()
	shared, err := h.sharedServer()
	if err != nil {
		return domain.Result{}, err
	}
	fingerprint, err := shared.certificates.ForLease(worker.Identity.ExpiresAt)
	if err != nil {
		return domain.Result{}, err
	}
	publisherToken, err := randomToken(32)
	if err != nil {
		return domain.Result{}, err
	}
	viewerToken, err := randomToken(32)
	if err != nil {
		return domain.Result{}, err
	}
	baseURL := strings.TrimRight(shared.baseURL, "/") + "/v1/rooms/" + url.PathEscape(worker.Details.SessionID)
	runtime := &contracts.MediaRuntime{SchemaVersion: contracts.RoomMediaRuntimeSchema,
		Capability: contracts.RoomMediaWebRTCCapability, Transport: "worker_sfu", BaseURL: baseURL,
		PublisherAccessToken: publisherToken, ViewerAccessToken: viewerToken,
		TLSCertificateSHA256: fingerprint, ExpiresAt: worker.Identity.ExpiresAt}
	if err := shared.add(worker.Details.SessionID, authorize(publisherToken, viewerToken, media.Handler(runtime.BaseURL))); err != nil {
		return domain.Result{}, err
	}
	defer shared.remove(worker.Details.SessionID)
	report := func() contracts.MediaProgressDetails {
		return contracts.MediaProgressDetails{Sequence: media.sequence.Load(), Packets: media.packets.Load(),
			Bytes: media.bytes.Load(), Dropped: media.dropped.Load(), Tracks: media.trackCounters(), Runtime: runtime}
	}
	reportProgress(ctx, report())
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	terminal := "source_closed"
	for {
		select {
		case <-ctx.Done():
			terminal = "expired"
			if errors.Is(ctx.Err(), context.Canceled) {
				terminal = "source_closed"
			}
			progress := report()
			result := contracts.MediaResultDetails{Sequence: progress.Sequence, Packets: progress.Packets,
				Bytes: progress.Bytes, Dropped: progress.Dropped, Tracks: progress.Tracks, Runtime: runtime, TerminalReason: terminal}
			return mediaResult(result), ctx.Err()
		case <-shared.done:
			return domain.Result{}, shared.failure()
		case <-media.publisherClosed:
			progress := report()
			result := contracts.MediaResultDetails{Sequence: progress.Sequence, Packets: progress.Packets,
				Bytes: progress.Bytes, Dropped: progress.Dropped, Tracks: progress.Tracks, Runtime: runtime, TerminalReason: terminal}
			return mediaResult(result), nil
		case <-ticker.C:
			reportProgress(ctx, report())
		}
	}
}

// PrepareListener binds the pinned TLS signaling listener before the worker
// advertises WebRTC media. Configuration or port conflicts fail startup closed.
func (h *Handler) PrepareListener() error {
	_, err := h.sharedServer()
	return err
}

func (h *Handler) Close() error {
	h.stopPruneOnce.Do(func() { close(h.stopPrune) })
	h.rtcMu.Lock()
	factory := h.factory
	h.factory = nil
	h.rtcMu.Unlock()
	if factory != nil {
		_ = factory.Close()
	}
	h.serverMu.Lock()
	defer h.serverMu.Unlock()
	if h.nodeServer != nil {
		_ = h.nodeServer.http.Close()
	}
	if h.server == nil {
		return nil
	}
	return h.server.http.Close()
}

func (h *Handler) sharedServer() (*sharedServer, error) {
	h.serverMu.Lock()
	defer h.serverMu.Unlock()
	if h.server != nil {
		select {
		case <-h.server.done:
			return nil, h.server.failure()
		default:
			return h.server, nil
		}
	}
	listener, err := net.Listen("tcp", h.config.ListenAddress)
	if err != nil {
		return nil, err
	}
	baseURL, err := advertisedURL(h.config.AdvertiseURL, listener.Addr())
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	var shared *sharedServer
	secure, certificates := pinnedListener(listener, h.now, nil, func() bool { return shared.hasMounts() }, false)
	shared = &sharedServer{listener: secure, baseURL: baseURL, certificates: certificates,
		sessions: make(map[string]http.Handler), mounts: make(map[string]mountEntry), done: make(chan struct{})}
	shared.http = &http.Server{Handler: shared, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	h.server = shared
	planeshared.Go(shared.serve)
	return shared, nil
}

func (h *Handler) nodeListener() (*sharedServer, error) {
	if h.config.NodeTLS == nil || strings.TrimSpace(h.config.NodeTLS.Addr) == "" {
		return h.sharedServer()
	}
	h.serverMu.Lock()
	defer h.serverMu.Unlock()
	if h.nodeServer != nil {
		select {
		case <-h.nodeServer.done:
			return nil, h.nodeServer.failure()
		default:
			return h.nodeServer, nil
		}
	}
	listener, err := net.Listen("tcp", h.config.NodeTLS.Addr)
	if err != nil {
		return nil, err
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	host := "localhost"
	if h.config.Fleet != nil && h.config.Fleet.EdgeHostname() != "" {
		host = h.config.Fleet.EdgeHostname()
	} else if ip := net.ParseIP(strings.TrimSpace(h.config.PublicIP)); ip != nil {
		host = ip.String()
	}
	baseURL := "https://" + net.JoinHostPort(host, port)
	if port == "443" {
		baseURL = "https://" + host
		if strings.Contains(host, ":") {
			baseURL = "https://[" + host + "]"
		}
	}
	secure, certificates := pinnedListener(listener, h.now, h.config.NodeTLS.GetCertificate, func() bool { return true }, true)
	shared := &sharedServer{listener: secure, baseURL: baseURL, certificates: certificates,
		sessions: make(map[string]http.Handler), mounts: make(map[string]mountEntry), done: make(chan struct{})}
	var handler http.Handler = shared
	if h.config.NodeTLS.WrapHTTPS != nil {
		handler = h.config.NodeTLS.WrapHTTPS(shared)
	}
	shared.http = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	h.nodeServer = shared
	planeshared.Go(shared.serve)
	return shared, nil
}

func pinnedListener(listener net.Listener, now func() time.Time,
	core func(*tls.ClientHelloInfo) (*tls.Certificate, error), anyPin func() bool, browser bool) (net.Listener, *workertls.Certificates) {
	options := workertls.ListenerOptions{}
	if browser {
		options.MinVersion, options.NextProtos = tls.VersionTLS12, []string{"h2", "http/1.1"}
	}
	var certificates *workertls.Certificates
	options.Fallback = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if core != nil {
			if certificate, err := core(hello); err == nil && certificate != nil {
				return certificate, nil
			}
		}
		if anyPin() {
			current := certificates.Current()
			if current == nil {
				if _, err := certificates.ForLease(now().Add(time.Hour)); err == nil {
					current = certificates.Current()
				}
			}
			if current != nil {
				return current, nil
			}
		}
		return nil, errors.New("worker certificate is not assigned")
	}
	secure, certificates := workertls.WrapListenerWithOptions(listener, now, options)
	return secure, certificates
}

func (s *sharedServer) serve() {
	err := s.http.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		err = errors.New("room media signaling server stopped")
	}
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	close(s.done)
}

func (s *sharedServer) add(sessionID string, handler http.Handler) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if _, exists := s.sessions[sessionID]; exists {
		return errors.New("room media session is already active")
	}
	s.sessions[sessionID] = handler
	return nil
}

func (s *sharedServer) remove(sessionID string) {
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

var mountInstances atomic.Uint64

type mountEntry struct {
	owner   mountOwner
	handler http.Handler
}

type mountOwner struct {
	nodeID   string
	instance string
	attempt  uint64
}

func (s *sharedServer) hasMounts() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.mounts) > 0
}

func (s *sharedServer) mount(prefix string, owner mountOwner, handler http.Handler) error {
	if !strings.HasPrefix(prefix, "/v1/") || !strings.HasSuffix(prefix, "/") || strings.HasPrefix(prefix, "/v1/rooms/") {
		return errors.New("room media mount prefix is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if existing, exists := s.mounts[prefix]; exists && (existing.owner.nodeID != owner.nodeID ||
		existing.owner.instance == owner.instance && existing.owner.attempt > owner.attempt) {
		return errors.New("room media node path is already served by another node on this Worker")
	}
	s.mounts[prefix] = mountEntry{owner: owner, handler: handler}
	return nil
}

func (s *sharedServer) unmount(prefix string, owner mountOwner) {
	s.mu.Lock()
	if s.mounts[prefix].owner == owner {
		delete(s.mounts, prefix)
	}
	s.mu.Unlock()
}

func (s *sharedServer) mounted(path string) http.Handler {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best string
	var handler http.Handler
	for prefix, candidate := range s.mounts {
		if strings.HasPrefix(path, prefix) && len(prefix) > len(best) {
			best, handler = prefix, candidate.handler
		}
	}
	return handler
}

func (s *sharedServer) failure() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.err != nil {
		return s.err
	}
	return errors.New("room media signaling server stopped")
}

func (s *sharedServer) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if !strings.HasPrefix(request.URL.Path, "/v1/rooms/") {
		if handler := s.mounted(request.URL.Path); handler != nil {
			handler.ServeHTTP(response, request)
			return
		}
		sfuv2.WriteError(response, &sfuv2.HTTPError{Status: http.StatusNotFound, Code: "not_found", Detail: "unknown session"})
		return
	}
	sessionID, ok := sessionIDFromPath(request.URL.Path)
	if !ok {
		http.NotFound(response, request)
		return
	}
	s.mu.RLock()
	handler := s.sessions[sessionID]
	s.mu.RUnlock()
	if handler == nil {
		http.NotFound(response, request)
		return
	}
	handler.ServeHTTP(response, request)
}

func sessionIDFromPath(path string) (string, bool) {
	const prefix = "/v1/rooms/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	value := strings.TrimPrefix(path, prefix)
	if separator := strings.IndexByte(value, '/'); separator >= 0 {
		value = value[:separator]
	}
	decoded, err := url.PathUnescape(value)
	return decoded, err == nil && decoded != ""
}

func decodeSpec(spec domain.Spec) (contracts.RoomWorkerSpec[contracts.MediaUnitDetails], error) {
	var value contracts.RoomWorkerSpec[contracts.MediaUnitDetails]
	decoder := json.NewDecoder(strings.NewReader(string(spec.Payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	if value.Schema != contracts.RoomWorkloadSchema || value.Identity.Kind != domain.KindRoomMedia {
		return value, errors.New("invalid room media worker spec")
	}
	return value, nil
}

func mediaResult(details contracts.MediaResultDetails) domain.Result {
	encoded, _ := json.Marshal(details)
	return domain.Result{BytesProcessed: details.Bytes, Outputs: map[string]string{"room_result_details": string(encoded)}}
}

func reportProgress(ctx context.Context, details contracts.MediaProgressDetails) {
	encoded, _ := json.Marshal(details)
	workloadprogress.Report(ctx, map[string]string{"room_progress_details": string(encoded)})
}

// ValidateAdvertiseURL requires the clean HTTPS origin that agents reach with
// the pinned worker certificate. {port} expands to the bound listener port.
func ValidateAdvertiseURL(configured string) error {
	parsed, err := url.Parse(strings.ReplaceAll(configured, "{port}", "1"))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("room media requires an HTTPS advertise URL")
	}
	return nil
}

func advertisedURL(configured string, address net.Addr) (string, error) {
	_, port, err := net.SplitHostPort(address.String())
	if err != nil {
		return "", err
	}
	value := strings.TrimRight(strings.ReplaceAll(configured, "{port}", port), "/")
	if err := ValidateAdvertiseURL(value); err != nil {
		return "", err
	}
	return value, nil
}

func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

type publisherAccessKey struct{}

func authorize(publisherToken, viewerToken string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodOptions {
			cors(response)
			response.WriteHeader(http.StatusNoContent)
			return
		}
		provided := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
		publisher := tokenMatches(provided, publisherToken)
		if !publisher && !tokenMatches(provided, viewerToken) {
			writeJSON(response, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		cors(response)
		next.ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), publisherAccessKey{}, publisher)))
	})
}

func tokenMatches(provided, token string) bool {
	return len(provided) == len(token) && subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func publisherAccess(request *http.Request) bool {
	publisher, _ := request.Context().Value(publisherAccessKey{}).(bool)
	return publisher
}

func cors(response http.ResponseWriter) {
	response.Header().Set("Access-Control-Allow-Origin", "*")
	response.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	response.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	response.Header().Set("Access-Control-Expose-Headers", "Location")
}

type MediaListener struct {
	handler *Handler
	server  *sharedServer
}

func (h *Handler) PrepareMediaListener() error {
	_, err := h.nodeListener()
	return err
}

func (h *Handler) MediaListener() (*MediaListener, error) {
	server, err := h.nodeListener()
	if err != nil {
		return nil, err
	}
	return &MediaListener{handler: h, server: server}, nil
}

func (l *MediaListener) MountRouter(prefix string, handler http.Handler) (func(), error) {
	parts := strings.Split(strings.Trim(prefix, "/"), "/")
	if len(parts) != 2 || parts[0] != "v1" || parts[1] == "" || parts[1] == "rooms" || !strings.HasSuffix(prefix, "/") {
		return nil, errors.New("a router mounts a top-level /v1/<family>/ prefix")
	}
	owner := mountOwner{nodeID: "router:" + prefix}
	if err := l.server.mount(prefix, owner, handler); err != nil {
		return nil, err
	}
	return func() { l.server.unmount(prefix, owner) }, nil
}

func (l *MediaListener) PinFor(host *string, expiresAt time.Time) (*string, error) {
	if tlsConfig := l.handler.config.NodeTLS; tlsConfig != nil && tlsConfig.Certificate != nil && host != nil &&
		tlsConfig.Certificate(*host) != nil {
		return nil, nil
	}
	if _, err := l.server.certificates.ForLease(expiresAt); err != nil {
		return nil, err
	}
	fingerprint := l.server.certificates.CurrentFingerprint()
	if fingerprint == "" {
		return nil, errors.New("no lease certificate")
	}
	return &fingerprint, nil
}

func (l *MediaListener) Mount(prefix, nodeID string, attempt uint64, handler http.Handler) (func(), error) {
	owner := mountOwner{nodeID: nodeID, attempt: attempt, instance: "mount-" + strconv.FormatUint(mountInstances.Add(1), 10)}
	if err := l.server.mount(prefix, owner, handler); err != nil {
		return nil, err
	}
	return func() { l.server.unmount(prefix, owner) }, nil
}

func (l *MediaListener) BaseURL() string { return l.server.baseURL }

func (l *MediaListener) LeasePin(expiresAt time.Time) (string, error) {
	return l.server.certificates.ForLease(expiresAt)
}
