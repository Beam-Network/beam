package roomworkloads

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

// DirectMessageHandler serves room.message.direct.v1 sessions on the shared
// direct room listener.
type DirectMessageHandler struct {
	server *DirectServer
	now    func() time.Time
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

func NewDirectMessageHandler(server *DirectServer) *DirectMessageHandler {
	return &DirectMessageHandler{server: server, now: time.Now}
}

func (*DirectMessageHandler) Kind() domain.Kind { return domain.KindRoomMessage }

func (handler *DirectMessageHandler) Validate(spec domain.Spec) error {
	if err := NewMessageHandler(nil).Validate(spec); err != nil {
		return err
	}
	return ValidateDirectServerConfig(handler.server.config)
}

func (handler *DirectMessageHandler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	if err := handler.Validate(spec); err != nil {
		return domain.Result{}, err
	}
	assignment, _ := decodeSpec[contracts.MessageUnitDetails](spec)
	baseURL, certificates, err := handler.server.ready()
	if err != nil {
		return domain.Result{}, err
	}
	token, err := newAccessToken()
	if err != nil {
		return domain.Result{}, err
	}
	sessionID := directSessionID(assignment.Identity)
	targets := make(map[string]struct{}, len(assignment.Targets))
	for _, target := range assignment.Targets {
		targets[target.MemberID] = struct{}{}
	}
	session := &messageSession{token: token, messageID: assignment.Details.MessageID,
		sizeBytes: assignment.Details.SizeBytes, targets: targets, expiresAt: assignment.Identity.ExpiresAt,
		payloadReady: make(chan struct{}), deliveries: make(map[string]contracts.MessageDelivery),
		changed: make(chan struct{}, 1)}
	if err := handler.server.registerMessage(sessionID, session); err != nil {
		return domain.Result{}, err
	}
	defer handler.server.unregisterMessage(sessionID)
	fingerprint, err := certificates.ForLease(assignment.Identity.ExpiresAt)
	if err != nil {
		return domain.Result{}, err
	}
	runtime := contracts.RoomMessageRuntime{SchemaVersion: contracts.RoomMessageRuntimeSchema,
		WorkloadID: assignment.Identity.WorkloadID, UnitID: assignment.Identity.UnitID, Epoch: assignment.Identity.Epoch,
		Attempt: assignment.Identity.Attempt, WorkerID: spec.Identity.WorkerID,
		Capability: contracts.RoomMessageDirectCapability, Transport: contracts.RoomWorkerHTTPSTransport,
		BaseURL:     directSessionURL(baseURL, roomMessagesPath, sessionID),
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

// directSessionID names one attempt on the shared listener.
func directSessionID(identity contracts.RoomWorkloadIdentity) string {
	return fmt.Sprintf("%s-%s-%d", identity.WorkloadID, identity.UnitID, identity.Attempt)
}

func serveMessage(session *messageSession, response http.ResponseWriter, request *http.Request, parts []string) {
	if session == nil || !session.authorize(request) {
		response.WriteHeader(http.StatusUnauthorized)
		return
	}
	if len(parts) == 1 && parts[0] == "source" {
		session.source(response, request)
		return
	}
	if len(parts) == 2 && parts[0] == "targets" {
		session.target(response, request, parts[1])
		return
	}
	if len(parts) == 3 && parts[0] == "targets" && parts[2] == "ack" {
		session.ack(response, request, parts[1])
		return
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
