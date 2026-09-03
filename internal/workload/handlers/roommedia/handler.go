package roommedia

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
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
}

type Handler struct {
	config Config

	serverMu sync.Mutex
	server   *sharedServer
}

type sharedServer struct {
	listener net.Listener
	http     *http.Server
	baseURL  string

	mu       sync.RWMutex
	sessions map[string]http.Handler
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
	if config.PublisherReconnectGrace <= 0 {
		config.PublisherReconnectGrace = 30 * time.Second
	}
	return &Handler{config: config}
}

func (*Handler) Kind() domain.Kind { return domain.KindRoomMedia }

func (h *Handler) Validate(spec domain.Spec) error {
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
	if _, _, err := net.SplitHostPort(h.config.ListenAddress); err != nil {
		return errors.New("room media listen address must be host:port")
	}
	if h.config.AdvertiseURL != "" {
		parsed, err := url.Parse(strings.ReplaceAll(h.config.AdvertiseURL, "{port}", "1"))
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return errors.New("room media advertise URL must be absolute")
		}
	}
	if (h.config.UDPPortMin == 0) != (h.config.UDPPortMax == 0) ||
		(h.config.UDPPortMin != 0 && h.config.UDPPortMin > h.config.UDPPortMax) {
		return errors.New("room media UDP port range is invalid")
	}
	return nil
}

func (h *Handler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
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
	token, err := randomToken(32)
	if err != nil {
		return domain.Result{}, err
	}
	baseURL := strings.TrimRight(shared.baseURL, "/") + "/v1/rooms/" + url.PathEscape(worker.Details.SessionID)
	runtime := &contracts.MediaRuntime{Capability: contracts.RoomMediaWebRTCCapability,
		Transport: "worker_sfu", BaseURL: baseURL,
		AccessToken: token, ExpiresAt: worker.Identity.ExpiresAt}
	if err := shared.add(worker.Details.SessionID, authorize(token, media.Handler(runtime.BaseURL))); err != nil {
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
	shared := &sharedServer{listener: listener, baseURL: baseURL, sessions: make(map[string]http.Handler), done: make(chan struct{})}
	shared.http = &http.Server{Handler: shared, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	h.server = shared
	go shared.serve()
	return shared, nil
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

func (s *sharedServer) failure() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.err != nil {
		return s.err
	}
	return errors.New("room media signaling server stopped")
}

func (s *sharedServer) ServeHTTP(response http.ResponseWriter, request *http.Request) {
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

func advertisedURL(configured string, address net.Addr) (string, error) {
	bound, ok := address.(*net.TCPAddr)
	if !ok {
		return "", errors.New("room media listener is not TCP")
	}
	if strings.TrimSpace(configured) == "" {
		host := bound.IP.String()
		if host == "" || bound.IP.IsUnspecified() {
			host = "127.0.0.1"
		}
		return "http://" + net.JoinHostPort(host, strconv.Itoa(bound.Port)), nil
	}
	parsed, err := url.Parse(strings.TrimRight(configured, "/"))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(strings.ReplaceAll(parsed.String(), "{port}", strconv.Itoa(bound.Port)), "/"), nil
}

func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func authorize(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodOptions {
			cors(response)
			response.WriteHeader(http.StatusNoContent)
			return
		}
		provided := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
		if len(provided) != len(token) || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			writeJSON(response, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		cors(response)
		next.ServeHTTP(response, request)
	})
}

func cors(response http.ResponseWriter) {
	response.Header().Set("Access-Control-Allow-Origin", "*")
	response.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	response.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	response.Header().Set("Access-Control-Expose-Headers", "Location")
}
