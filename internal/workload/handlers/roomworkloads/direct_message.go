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
	sourceToken  string
	targetTokens []contracts.RoomStreamTargetToken
	messageID    string
	sizeBytes    int64
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
	sourceToken, err := newAccessToken()
	if err != nil {
		return domain.Result{}, err
	}
	sessionID := directSessionID(assignment.Identity)
	session := &messageSession{sourceToken: sourceToken, messageID: assignment.Details.MessageID,
		sizeBytes: assignment.Details.SizeBytes, expiresAt: assignment.Identity.ExpiresAt,
		payloadReady: make(chan struct{}), deliveries: make(map[string]contracts.MessageDelivery),
		changed: make(chan struct{}, 1)}
	members := make([]string, 0, len(assignment.Targets))
	for _, target := range assignment.Targets {
		token, err := newAccessToken()
		if err != nil {
			return domain.Result{}, err
		}
		session.targetTokens = append(session.targetTokens,
			contracts.RoomStreamTargetToken{TargetMemberID: target.MemberID, AccessToken: token})
		members = append(members, target.MemberID)
	}
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
		BaseURL: directSessionURL(baseURL, roomMessagesPath, sessionID), TLSCertificateSHA256: fingerprint,
		ExpiresAt: assignment.Identity.ExpiresAt, SourceAccessToken: session.sourceToken,
		TargetAccessTokens: session.targetTokens}
	identity := assignment.Identity
	identity.WorkerID = spec.Identity.WorkerID
	if err := runtime.Validate(identity, members, handler.now().UTC()); err != nil {
		return domain.Result{}, err
	}
	report(ctx, contracts.MessageProgressDetails{Runtime: &runtime})

	for {
		session.mu.Lock()
		complete := len(session.payload) > 0 && len(session.deliveries) == len(session.targetTokens)
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
	var role streamRole
	ok := false
	if session != nil {
		role, ok = session.authenticate(request)
	}
	if !ok {
		response.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case len(parts) == 1 && parts[0] == "source":
		if !role.source {
			response.WriteHeader(http.StatusForbidden)
			return
		}
		session.source(response, request)
	case (len(parts) == 2 || len(parts) == 3 && parts[2] == "ack") && parts[0] == "targets":
		if role.source || role.member != parts[1] {
			response.WriteHeader(http.StatusForbidden)
			return
		}
		if len(parts) == 2 {
			session.target(response, request, parts[1])
		} else {
			session.ack(response, request, parts[1])
		}
	default:
		http.NotFound(response, request)
	}
}

func (session *messageSession) authenticate(request *http.Request) (streamRole, bool) {
	authorization := request.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") || !time.Now().Before(session.expiresAt) {
		return streamRole{}, false
	}
	provided := []byte(strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer ")))
	var role streamRole
	found := false
	if subtle.ConstantTimeCompare(provided, []byte(session.sourceToken)) == 1 {
		role, found = streamRole{source: true}, true
	}
	for _, target := range session.targetTokens {
		if subtle.ConstantTimeCompare(provided, []byte(target.AccessToken)) == 1 {
			role, found = streamRole{member: target.TargetMemberID}, true
		}
	}
	return role, found
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
