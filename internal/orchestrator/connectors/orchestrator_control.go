package connectors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/orchestrator/roomtransfer"
	"github.com/Beam-Network/beam/internal/orchestrator/roomworkloads"
	beamcoreadapter "github.com/Beam-Network/beam/internal/workload/adapters/beamcore"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/nats-io/nats.go"
	"github.com/vmihailenco/msgpack/v5"
)

const orchestratorControlSchema = "orchestrator-control/v1"

type orchestratorControlEnvelope struct {
	MessageID     string `msgpack:"message_id"`
	SchemaVersion string `msgpack:"schema_version"`
	Environment   string `msgpack:"environment"`
	Hotkey        string `msgpack:"hotkey"`
	MessageType   string `msgpack:"message_type"`
	RequestID     string `msgpack:"request_id,omitempty"`
	OccurredAt    string `msgpack:"occurred_at"`
	Producer      string `msgpack:"producer"`
	Payload       any    `msgpack:"payload"`
}

type roomControl struct {
	config                    NATSConfig
	conn                      roomControlNATS
	rooms                     *roomtransfer.Service
	workloads                 *roomworkloads.Manager
	tasks                     *dispatch.Service
	lastCapabilityFingerprint string
}

type roomControlSubscription interface {
	Unsubscribe() error
}

type roomControlNATS interface {
	chanSubscribe(string, chan *nats.Msg) (roomControlSubscription, error)
	flushWithContext(context.Context) error
	requestWithContext(context.Context, string, []byte) (*nats.Msg, error)
}

type natsRoomControlConnection struct {
	conn *nats.Conn
}

func (connection natsRoomControlConnection) chanSubscribe(subject string, messages chan *nats.Msg) (roomControlSubscription, error) {
	return connection.conn.ChanSubscribe(subject, messages)
}

func (connection natsRoomControlConnection) flushWithContext(ctx context.Context) error {
	return connection.conn.FlushWithContext(ctx)
}

func (connection natsRoomControlConnection) requestWithContext(ctx context.Context, subject string, payload []byte) (*nats.Msg, error) {
	return connection.conn.RequestWithContext(ctx, subject, payload)
}

type roomControlSession struct {
	control       *roomControl
	messages      chan *nats.Msg
	subscriptions []roomControlSubscription
}

func newRoomControl(config NATSConfig, conn *nats.Conn, rooms *roomtransfer.Service,
	workloads *roomworkloads.Manager, tasks *dispatch.Service) *roomControl {
	if config.Environment == "" {
		config.Environment = "prod"
	}
	if config.ControlPrefix == "" {
		config.ControlPrefix = "beam.orch.control"
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 10 * time.Second
	}
	config.Hotkey = strings.TrimSpace(config.Hotkey)
	var controlConnection roomControlNATS
	if conn != nil {
		controlConnection = natsRoomControlConnection{conn: conn}
	}
	return &roomControl{config: config, conn: controlConnection, rooms: rooms, workloads: workloads, tasks: tasks}
}

func (control *roomControl) enabled() bool {
	return control != nil && control.conn != nil && control.tasks != nil && control.config.Hotkey != "" && control.config.GatewayURL != ""
}

func (control *roomControl) bind(ctx context.Context) (_ *roomControlSession, err error) {
	if !control.enabled() {
		return &roomControlSession{control: control}, nil
	}
	if err := control.request(ctx, "register", map[string]any{"gateway_url": control.config.GatewayURL, "ready": true}); err != nil {
		return nil, fmt.Errorf("register Orchestrator: %w", err)
	}
	if err := control.publishCapability(ctx, true); err != nil {
		return nil, err
	}
	session := &roomControlSession{control: control, messages: make(chan *nats.Msg, 256)}
	defer func() {
		if err != nil {
			session.close()
		}
	}()
	subscribe := func(messageType string) error {
		subscription, subscribeErr := control.conn.chanSubscribe(
			control.subject("runtime", messageType), session.messages,
		)
		if subscribeErr != nil {
			return subscribeErr
		}
		session.subscriptions = append(session.subscriptions, subscription)
		return nil
	}
	if err = subscribe("worker_task_offer_batch"); err != nil {
		return nil, err
	}
	if control.rooms != nil {
		if err = subscribe("room_task_offer_batch"); err != nil {
			return nil, err
		}
		if err = subscribe("room_task_cancel"); err != nil {
			return nil, err
		}
	}
	if control.workloads != nil {
		if err = subscribe("room_workload_offer"); err != nil {
			return nil, err
		}
		if err = subscribe("room_workload_cancel"); err != nil {
			return nil, err
		}
	}
	if err = control.conn.flushWithContext(ctx); err != nil {
		return nil, err
	}
	return session, nil
}

func (session *roomControlSession) close() {
	for index := len(session.subscriptions) - 1; index >= 0; index-- {
		_ = session.subscriptions[index].Unsubscribe()
	}
	session.subscriptions = nil
}

func (session *roomControlSession) run(ctx context.Context) error {
	control := session.control
	if control == nil || !control.enabled() {
		return nil
	}
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	heartbeatErrors := make(chan error, 1)
	go control.runHeartbeat(heartbeatCtx, heartbeatErrors)
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-heartbeatErrors:
			return err
		case message := <-session.messages:
			if message == nil {
				return nil
			}
			if strings.HasSuffix(message.Subject, ".worker_task_offer_batch") {
				if err := control.handleTaskOfferBatch(ctx, message.Data); err != nil {
					log.Printf("ignore invalid BeamCore task offer: %v", err)
				}
			} else if strings.HasSuffix(message.Subject, ".room_workload_offer") {
				if err := control.handleRoomWorkloadOffer(ctx, message.Data); err != nil {
					log.Printf("ignore invalid BeamCore room workload offer: %v", err)
				}
			} else if strings.HasSuffix(message.Subject, ".room_workload_cancel") {
				if err := control.handleRoomWorkloadCancel(ctx, message.Data); err != nil {
					log.Printf("ignore invalid BeamCore room workload cancellation: %v", err)
				}
			} else if strings.HasSuffix(message.Subject, ".room_task_cancel") {
				if err := control.handleCancel(ctx, message.Data); err != nil {
					log.Printf("ignore invalid BeamCore room cancellation: %v", err)
				}
			} else if err := control.handleOffer(ctx, message.Data); err != nil {
				log.Printf("ignore invalid BeamCore room transfer offer: %v", err)
			}
		}
	}
}

// runHeartbeat sends liveness and capability changes.
func (control *roomControl) runHeartbeat(ctx context.Context, failures chan<- error) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := control.request(ctx, "heartbeat", map[string]any{}); err != nil {
				reportHeartbeatFailure(ctx, failures, err)
				return
			}
			if err := control.publishCapability(ctx, false); err != nil {
				reportHeartbeatFailure(ctx, failures, err)
				return
			}
		}
	}
}

func reportHeartbeatFailure(ctx context.Context, failures chan<- error, err error) {
	select {
	case failures <- err:
	case <-ctx.Done():
	}
}

func (control *roomControl) handleRoomWorkloadOffer(ctx context.Context, encoded []byte) error {
	payload, err := control.roomWorkloadPayload(encoded, "room_workload_offer")
	if err != nil {
		return err
	}
	return control.workloads.Submit(ctx, payload)
}

func (control *roomControl) handleRoomWorkloadCancel(ctx context.Context, encoded []byte) error {
	payload, err := control.roomWorkloadPayload(encoded, "room_workload_cancel")
	if err != nil {
		return err
	}
	var cancel contracts.RoomWorkloadCancelWire
	if err := decodeStrictRoomWire(payload, &cancel); err != nil {
		return err
	}
	return control.workloads.Cancel(ctx, cancel)
}

func (control *roomControl) roomWorkloadPayload(encoded []byte, messageType string) ([]byte, error) {
	if control.workloads == nil {
		return nil, errors.New("generic room workload service is missing")
	}
	var envelope orchestratorControlEnvelope
	if err := msgpack.Unmarshal(encoded, &envelope); err != nil {
		return nil, err
	}
	if envelope.SchemaVersion != orchestratorControlSchema || envelope.Environment != control.config.Environment ||
		strings.TrimSpace(envelope.Hotkey) != control.config.Hotkey || envelope.MessageType != messageType ||
		envelope.Producer != "transfer-runtime" {
		return nil, errors.New("BeamCore room workload envelope is invalid")
	}
	return json.Marshal(envelope.Payload)
}

func (control *roomControl) handleTaskOfferBatch(ctx context.Context, encoded []byte) error {
	var envelope orchestratorControlEnvelope
	if err := msgpack.Unmarshal(encoded, &envelope); err != nil {
		return err
	}
	if envelope.SchemaVersion != orchestratorControlSchema || envelope.Environment != control.config.Environment ||
		strings.TrimSpace(envelope.Hotkey) != control.config.Hotkey || envelope.MessageType != "worker_task_offer_batch" ||
		envelope.Producer != "transfer-runtime" {
		return errors.New("BeamCore task offer envelope is invalid")
	}
	payload, err := json.Marshal(envelope.Payload)
	if err != nil {
		return err
	}
	var batch struct {
		BatchID string                      `json:"batch_id"`
		Offers  []beamcoreadapter.TaskOffer `json:"offers"`
	}
	if err := json.Unmarshal(payload, &batch); err != nil {
		return err
	}
	if batch.BatchID == "" || len(batch.Offers) == 0 {
		return errors.New("BeamCore task offer batch is empty")
	}
	for _, offer := range batch.Offers {
		spec, err := beamcoreadapter.ToWorkload(offer, domain.Identity{}, time.Now())
		if err != nil {
			return err
		}
		if _, err := control.tasks.Dispatch(ctx, dispatch.DispatchRequest{
			Source: dispatch.SourceBeamCore, ExternalID: offer.OfferID, Spec: spec,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (control *roomControl) handleCancel(ctx context.Context, encoded []byte) error {
	var envelope orchestratorControlEnvelope
	if err := msgpack.Unmarshal(encoded, &envelope); err != nil {
		return err
	}
	if envelope.SchemaVersion != orchestratorControlSchema || envelope.Environment != control.config.Environment ||
		strings.TrimSpace(envelope.Hotkey) != control.config.Hotkey || envelope.MessageType != "room_task_cancel" ||
		envelope.Producer != "transfer-runtime" {
		return errors.New("BeamCore room cancellation envelope is invalid")
	}
	payload, err := json.Marshal(envelope.Payload)
	if err != nil {
		return err
	}
	var request contracts.RoomTaskCancel
	if err := json.Unmarshal(payload, &request); err != nil {
		return err
	}
	return control.rooms.Cancel(ctx, request)
}

func (control *roomControl) handleOffer(ctx context.Context, encoded []byte) error {
	var envelope orchestratorControlEnvelope
	if err := msgpack.Unmarshal(encoded, &envelope); err != nil {
		return err
	}
	if envelope.SchemaVersion != orchestratorControlSchema || envelope.Environment != control.config.Environment ||
		strings.TrimSpace(envelope.Hotkey) != control.config.Hotkey || envelope.MessageType != "room_task_offer_batch" ||
		envelope.Producer != "transfer-runtime" {
		return errors.New("BeamCore room offer envelope is invalid")
	}
	payload, err := json.Marshal(envelope.Payload)
	if err != nil {
		return err
	}
	var batch contracts.RoomTaskOfferBatch
	if err := json.Unmarshal(payload, &batch); err != nil {
		return err
	}
	return control.rooms.Submit(ctx, batch)
}

func (control *roomControl) submitResult(ctx context.Context, result contracts.RoomTaskResult) error {
	return control.request(ctx, "room_task_result", result)
}

func (control *roomControl) submitTaskResult(ctx context.Context, record dispatch.Record, result domain.Result) error {
	evidence, err := resultEvidence(record, result)
	if err != nil {
		return err
	}
	return control.request(ctx, "task_result", map[string]any{
		"worker_id":  record.WorkerID,
		"task_id":    record.Spec.WorkloadID,
		"offer_id":   record.Spec.AttemptID,
		"success":    result.State == domain.StateCompleted || result.State == domain.StateReceiptCommitted,
		"chunk_hash": evidence.chunkHash,
		"etag":       evidence.etag,
		"error":      result.ErrorMessage,
	})
}

func (control *roomControl) submitRoomWorkload(ctx context.Context, messageType string, encoded []byte) error {
	var payload any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return err
	}
	return control.request(ctx, messageType, payload)
}

func (control *roomControl) SubmitRoomWorkloadProgress(ctx context.Context, value contracts.RoomGenericProgress) error {
	encoded, err := encodeRoomWorkloadProgress(value)
	if err != nil {
		return err
	}
	return control.submitRoomWorkload(ctx, "room_workload_progress", encoded)
}

func (control *roomControl) SubmitRoomWorkloadResult(ctx context.Context, value contracts.RoomGenericResult) error {
	encoded, err := encodeRoomWorkloadResult(value, time.Now().UTC())
	if err != nil {
		return err
	}
	return control.submitRoomWorkload(ctx, "room_workload_result", encoded)
}

func (control *roomControl) SubmitRoomWorkloadStatus(context.Context, json.RawMessage) error {
	return nil
}

func (control *roomControl) SubmitRoomWorkloadProvisioningResult(ctx context.Context,
	value contracts.RoomWorkloadProvisioningResultWire) error {
	return control.request(ctx, "room_workload_provisioning_result", value)
}

func (control *roomControl) publishCapability(ctx context.Context, force bool) error {
	now := time.Now().UTC()
	manifest := control.capabilityManifest(now)
	fingerprint := capabilityManifestFingerprint(manifest)
	if !force && fingerprint == control.lastCapabilityFingerprint {
		return nil
	}
	if err := control.request(ctx, "capability_update", manifest); err != nil {
		return err
	}
	control.lastCapabilityFingerprint = fingerprint
	return nil
}

func (control *roomControl) capabilityManifest(now time.Time) contracts.CapabilityManifest {
	version := control.config.SoftwareVersion
	if version == "" {
		version = "0.2.0"
	}
	capabilities := make([]string, 0, 8)
	if control.tasks.CapabilityAvailable(contracts.TransferMultipartCapability, beamcoreadapter.MultipartTransferResources()) {
		capabilities = append(capabilities, contracts.TransferMultipartCapability)
	}
	if control.rooms != nil && control.rooms.CapabilityAvailable() {
		capabilities = append(capabilities, contracts.RoomTransferCapability, contracts.RoomTransferDirectCapability,
			contracts.RoomTransferE2EECapability)
	}
	for _, kind := range []domain.Kind{domain.KindRoomDatagram, domain.KindRoomMessage, domain.KindRoomCommand,
		domain.KindRoomStream, domain.KindRoomMedia} {
		if control.workloads == nil || !control.workloads.CapabilityAvailable(kind) {
			continue
		}
		capabilities = append(capabilities, string(kind))
	}
	if control.workloads != nil && control.workloads.CapacityCapabilityAvailable(
		domain.KindRoomMedia, contracts.RoomMediaWebRTCCapability) {
		capabilities = append(capabilities, contracts.RoomMediaWebRTCCapability)
	}
	availableConnections := int64(0)
	if len(capabilities) > 0 {
		availableConnections = 1
	}
	manifest := contracts.NewOrchestratorCapabilityManifest(
		control.config.Hotkey,
		version,
		capabilities,
		availableConnections,
		availableConnections,
		now,
	)
	return manifest
}

func capabilityManifestFingerprint(manifest contracts.CapabilityManifest) string {
	payload := struct {
		SoftwareVersion string                    `json:"software_version"`
		Protocols       []contracts.ProtocolRange `json:"protocols"`
		Capabilities    []string                  `json:"capabilities"`
		Capacity        struct {
			MaxConnections       int64 `json:"max_connections"`
			AvailableConnections int64 `json:"available_connections"`
		} `json:"capacity"`
	}{
		SoftwareVersion: manifest.SoftwareVersion,
		Protocols:       manifest.Protocols,
		Capabilities:    manifest.Capabilities,
	}
	payload.Capacity.MaxConnections = manifest.Capacity.MaxConnections
	payload.Capacity.AvailableConnections = manifest.Capacity.AvailableConnections
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func (control *roomControl) request(ctx context.Context, messageType string, payload any) error {
	requestID := controlID()
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var normalizedPayload any
	if err := json.Unmarshal(jsonPayload, &normalizedPayload); err != nil {
		return err
	}
	envelope := orchestratorControlEnvelope{MessageID: controlID(), SchemaVersion: orchestratorControlSchema,
		Environment: control.config.Environment, Hotkey: control.config.Hotkey, MessageType: messageType,
		RequestID: requestID, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano), Producer: "orchestrator", Payload: normalizedPayload}
	encoded, err := msgpack.Marshal(envelope)
	if err != nil {
		return err
	}
	requestContext, cancel := context.WithTimeout(ctx, control.config.RequestTimeout)
	defer cancel()
	message, err := control.conn.requestWithContext(requestContext, control.subject("orch", messageType), encoded)
	if err != nil {
		return err
	}
	var response orchestratorControlEnvelope
	if err := msgpack.Unmarshal(message.Data, &response); err != nil {
		return err
	}
	if response.RequestID != requestID || response.Producer != "transfer-runtime" || response.MessageType == "error" ||
		response.MessageType == "register_error" {
		return fmt.Errorf("BeamCore rejected %s", messageType)
	}
	if messageType == "capability_update" {
		encodedPayload, err := json.Marshal(response.Payload)
		if err != nil {
			return err
		}
		var acknowledgement struct {
			Accepted bool   `json:"accepted"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(encodedPayload, &acknowledgement); err != nil {
			return err
		}
		if !acknowledgement.Accepted {
			return fmt.Errorf("BeamCore rejected capability_update: %s", fallback(acknowledgement.Reason, "not accepted"))
		}
	}
	if messageType == "room_task_result" || messageType == "task_result" {
		encodedPayload, err := json.Marshal(response.Payload)
		if err != nil {
			return err
		}
		var acknowledgement struct {
			Received bool   `json:"received"`
			Status   string `json:"status"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(encodedPayload, &acknowledgement); err != nil {
			return err
		}
		if messageType == "task_result" {
			return taskResultDisposition(taskResultAcknowledgement(acknowledgement))
		}
		if !acknowledgement.Received {
			return fmt.Errorf("BeamCore rejected %s: %s", messageType, fallback(acknowledgement.Reason, "not received"))
		}
	}
	return nil
}

func (control *roomControl) subject(direction, messageType string) string {
	return fmt.Sprintf("%s.%s.%s.%s.%s", strings.Trim(control.config.ControlPrefix, "."), control.config.Environment,
		direction, strings.ToLower(control.config.Hotkey), messageType)
}

func controlID() string {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	return hex.EncodeToString(value)
}
