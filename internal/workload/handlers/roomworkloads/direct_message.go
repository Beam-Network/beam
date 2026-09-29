package roomworkloads

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/handlers/workertls"
)

type DirectMessageConfig struct {
	ListenAddress string
	AdvertiseURL  string
}

type DirectMessageHandler struct {
	config DirectMessageConfig
	now    func() time.Time
	mu     sync.Mutex
	server *messageServer
}

type messageServer struct {
	listener     net.Listener
	http         *http.Server
	baseURL      string
	certificates *workertls.Certificates
	mu           sync.RWMutex
	sessions     map[string]*messageSession
}

type messageSession struct {
	token        string
	messageID    string
	sizeBytes    int64
	targets      map[string]struct{}
	expiresAt    time.Time
	mu           sync.Mutex
	payload      []byte
	payloadReady chan struct{}
	deliveries   map[string]contracts.MessageDelivery
	changed      chan struct{}
}

func NewDirectMessageHandler(config DirectMessageConfig) *DirectMessageHandler {
	return &DirectMessageHandler{config: config, now: time.Now}
}

func (*DirectMessageHandler) Kind() domain.Kind { return domain.KindRoomMessage }

func (handler *DirectMessageHandler) Validate(spec domain.Spec) error {
	if err := NewMessageHandler(nil).Validate(spec); err != nil {
		return err
	}
	if _, _, err := net.SplitHostPort(strings.TrimSpace(handler.config.ListenAddress)); err != nil {
		return errors.New("direct room message listen address must be host:port")
	}
	parsed, err := url.Parse(strings.ReplaceAll(handler.config.AdvertiseURL, "{port}", "1"))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("direct room messages require an HTTPS advertise URL")
	}
	return nil
}

func (handler *DirectMessageHandler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	if err := handler.Validate(spec); err != nil {
		return domain.Result{}, err
	}
	assignment, _ := decodeSpec[contracts.MessageUnitDetails](spec)
	server, err := handler.sharedServer()
	if err != nil {
		return domain.Result{}, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return domain.Result{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	sessionID := fmt.Sprintf("%s-%s-%d", assignment.Identity.WorkloadID, assignment.Identity.UnitID, assignment.Identity.Attempt)
	targets := make(map[string]struct{}, len(assignment.Targets))
	for _, target := range assignment.Targets {
		targets[target.MemberID] = struct{}{}
	}
	session := &messageSession{token: token, messageID: assignment.Details.MessageID,
		sizeBytes: assignment.Details.SizeBytes, targets: targets, expiresAt: assignment.Identity.ExpiresAt,
		payloadReady: make(chan struct{}), deliveries: make(map[string]contracts.MessageDelivery),
		changed: make(chan struct{}, 1)}
	server.mu.Lock()
	if _, exists := server.sessions[sessionID]; exists {
		server.mu.Unlock()
		return domain.Result{}, errors.New("direct room message session is already active")
	}
	server.sessions[sessionID] = session
	server.mu.Unlock()
	defer func() { server.mu.Lock(); delete(server.sessions, sessionID); server.mu.Unlock() }()
	fingerprint, err := server.certificates.ForLease(assignment.Identity.ExpiresAt)
	if err != nil {
		return domain.Result{}, err
	}
	runtime := contracts.RoomMessageRuntime{SchemaVersion: contracts.RoomMessageRuntimeSchema,
		WorkloadID: assignment.Identity.WorkloadID, UnitID: assignment.Identity.UnitID, Epoch: assignment.Identity.Epoch,
		Attempt: assignment.Identity.Attempt, WorkerID: spec.Identity.WorkerID,
		Capability: contracts.RoomMessageDirectCapability, Transport: "worker_https",
		BaseURL:     strings.TrimRight(server.baseURL, "/") + "/v1/room-messages/" + url.PathEscape(sessionID),
		AccessToken: token, TLSCertificateSHA256: fingerprint, ExpiresAt: assignment.Identity.ExpiresAt}
	identity := assignment.Identity
	identity.WorkerID = spec.Identity.WorkerID
	if err := runtime.Validate(identity, handler.now().UTC()); err != nil {
		return domain.Result{}, err
	}
	report(ctx, contracts.MessageProgressDetails{Runtime: &runtime})

	for {
		session.mu.Lock()
		complete := len(session.payload) > 0 && len(session.deliveries) == len(session.targets)
		deliveries := make([]contracts.MessageDelivery, 0, len(session.deliveries))
		if complete {
			for _, delivery := range session.deliveries {
				deliveries = append(deliveries, delivery)
			}
		}
		payloadBytes := len(session.payload)
		session.mu.Unlock()
		if complete {
			sort.Slice(deliveries, func(i, j int) bool { return deliveries[i].TargetMemberID < deliveries[j].TargetMemberID })
			details := contracts.MessageProgressDetails{Deliveries: deliveries}
			report(ctx, details)
			return result(details, int64(payloadBytes)), nil
		}
		select {
		case <-ctx.Done():
			return domain.Result{}, ctx.Err()
		case <-time.After(time.Until(assignment.Identity.ExpiresAt)):
			return domain.Result{}, errors.New("direct room message session expired")
		case <-session.changed:
		}
	}
}

func (handler *DirectMessageHandler) Close() error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.server == nil {
		return nil
	}
	return handler.server.http.Close()
}

func (handler *DirectMessageHandler) PrepareListener() error {
	_, err := handler.sharedServer()
	return err
}

func (handler *DirectMessageHandler) sharedServer() (*messageServer, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.server != nil {
		return handler.server, nil
	}
	listener, err := net.Listen("tcp", handler.config.ListenAddress)
	if err != nil {
		return nil, err
	}
	baseURL, err := messageAdvertisedURL(handler.config.AdvertiseURL, listener.Addr())
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	secure, certificates := workertls.WrapListener(listener, handler.now)
	server := &messageServer{listener: secure, baseURL: baseURL, certificates: certificates,
		sessions: make(map[string]*messageSession)}
	server.http = &http.Server{Handler: server, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: time.Minute, WriteTimeout: time.Minute, IdleTimeout: time.Minute}
	handler.server = server
	go func() { _ = server.http.Serve(server.listener) }()
	return server, nil
}

func (server *messageServer) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "room-messages" {
		http.NotFound(response, request)
		return
	}
	server.mu.RLock()
	session := server.sessions[parts[2]]
	server.mu.RUnlock()
	if session == nil || !session.authorize(request) {
		response.WriteHeader(http.StatusUnauthorized)
		return
	}
	if parts[3] == "source" && len(parts) == 4 {
		session.source(response, request)
		return
	}
	if parts[3] == "targets" && (len(parts) == 5 || len(parts) == 6) {
		memberID, err := url.PathUnescape(parts[4])
		if err != nil {
			http.NotFound(response, request)
			return
		}
		if len(parts) == 5 {
			session.target(response, request, memberID)
			return
		}
		if parts[5] == "ack" {
			session.ack(response, request, memberID)
			return
		}
	}
	http.NotFound(response, request)
}

func (session *messageSession) authorize(request *http.Request) bool {
	authorization := request.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	return subtle.ConstantTimeCompare([]byte(provided), []byte(session.token)) == 1 && time.Now().Before(session.expiresAt)
}

func (session *messageSession) source(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(response, request.Body, maximumJSONBytes+1))
	if err != nil || len(payload) > maximumJSONBytes {
		response.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	var record messageRecord
	if json.Unmarshal(payload, &record) != nil || record.MessageID != session.messageID ||
		int64(len(record.Ciphertext)) != session.sizeBytes {
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	session.mu.Lock()
	if len(session.payload) != 0 && !bytes.Equal(session.payload, payload) {
		session.mu.Unlock()
		response.WriteHeader(http.StatusConflict)
		return
	}
	firstUpload := len(session.payload) == 0
	session.payload = append([]byte(nil), payload...)
	if firstUpload {
		close(session.payloadReady)
	}
	session.signalLocked()
	session.mu.Unlock()
	response.WriteHeader(http.StatusNoContent)
}

func (session *messageSession) target(response http.ResponseWriter, request *http.Request, memberID string) {
	if request.Method != http.MethodGet {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, ok := session.targets[memberID]; !ok {
		http.NotFound(response, request)
		return
	}
	for {
		session.mu.Lock()
		payload := append([]byte(nil), session.payload...)
		payloadReady := session.payloadReady
		session.mu.Unlock()
		if len(payload) != 0 {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write(payload)
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-time.After(time.Until(session.expiresAt)):
			response.WriteHeader(http.StatusGone)
			return
		case <-payloadReady:
		}
	}
}

func (session *messageSession) ack(response http.ResponseWriter, request *http.Request, memberID string) {
	if request.Method != http.MethodPost {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, ok := session.targets[memberID]; !ok {
		http.NotFound(response, request)
		return
	}
	var delivery contracts.MessageDelivery
	decoder := json.NewDecoder(io.LimitReader(request.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&delivery) != nil || delivery.TargetMemberID != memberID ||
		(delivery.State != "delivered" && delivery.State != "failed") {
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	session.mu.Lock()
	if existing, ok := session.deliveries[memberID]; ok && existing.State != delivery.State {
		session.mu.Unlock()
		response.WriteHeader(http.StatusConflict)
		return
	}
	session.deliveries[memberID] = delivery
	session.signalLocked()
	session.mu.Unlock()
	response.WriteHeader(http.StatusNoContent)
}

func (session *messageSession) signalLocked() {
	select {
	case session.changed <- struct{}{}:
	default:
	}
}

func messageAdvertisedURL(configured string, address net.Addr) (string, error) {
	_, port, err := net.SplitHostPort(address.String())
	if err != nil {
		return "", err
	}
	value := strings.ReplaceAll(configured, "{port}", port)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("direct room message advertise URL is invalid")
	}
	return strings.TrimRight(value, "/"), nil
}
