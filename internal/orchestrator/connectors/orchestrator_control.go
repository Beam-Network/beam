package connectors

import (
	"context"
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

const DefaultControlPrefix = "beam.orch.control.v2"

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
		config.Environment = "dev"
	}
	if config.ControlPrefix == "" {
		config.ControlPrefix = DefaultControlPrefix
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
	if err = subscribe("task_offer_batch"); err != nil {
		return nil, err
	}
	if err = subscribe("task_offer_batch_cancel"); err != nil {
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
	flushCtx, cancelFlush := context.WithTimeout(ctx, control.config.RequestTimeout)
	defer cancelFlush()
	if err = control.conn.flushWithContext(flushCtx); err != nil {
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
	var roomQueue *roomWorkloadQueue
	if control.workloads != nil {
		jobsCtx, stopJobs := context.WithCancel(ctx)
		roomQueue = newRoomWorkloadQueue(control.processRoomWorkloadJob)
		stopped := make(chan struct{})
		go func() {
			roomQueue.run(jobsCtx)
			close(stopped)
		}()
		defer func() {
			stopJobs()
			<-stopped
		}()
	}
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
			switch messageType := message.Subject[strings.LastIndex(message.Subject, ".")+1:]; messageType {
			case "task_offer_batch":
				if err := control.handleTaskOfferBatch(ctx, message.Data); err != nil {
					log.Printf("ignore invalid BeamCore task_offer_batch: %v", err)
				}
			case "task_offer_batch_cancel":
				if err := control.handleTaskOfferBatchCancel(ctx, message.Data); err != nil {
					log.Printf("ignore invalid BeamCore task_offer_batch_cancel: %v", err)
				}
			case "room_workload_offer":
				if err := control.queueRoomWorkload(ctx, roomQueue, message.Data, false); err != nil {
					log.Printf("ignore invalid BeamCore room workload offer: %v", err)
				}
			case "room_workload_cancel":
				if err := control.queueRoomWorkload(ctx, roomQueue, message.Data, true); err != nil {
					log.Printf("ignore invalid BeamCore room workload cancellation: %v", err)
				}
			case "room_task_cancel":
				if err := control.handleCancel(ctx, message.Data); err != nil {
					log.Printf("ignore invalid BeamCore room cancellation: %v", err)
				}
			case "room_task_offer_batch":
				if err := control.handleOffer(ctx, message.Data); err != nil {
					log.Printf("ignore invalid BeamCore room transfer offer: %v", err)
				}
			default:
				log.Printf("ignore BeamCore control message of unknown type %q", messageType)
			}
		}
	}
}

func (control *roomControl) queueRoomWorkload(ctx context.Context, queue *roomWorkloadQueue, encoded []byte, cancel bool) error {
	if queue == nil {
		return errors.New("generic room workload service is missing")
	}
	if control.workloads == nil {
		return errors.New("generic room workload service is missing")
	}
	messageType := "room_workload_offer"
	if cancel {
		messageType = "room_workload_cancel"
	}
	payload, err := decodeControlPayload(encoded, messageType)
	if err != nil {
		return err
	}
	var identity struct {
		WorkloadID string `json:"workload_id"`
	}
	if err := json.Unmarshal(payload, &identity); err != nil {
		return err
	}
	if identity.WorkloadID == "" {
		return errors.New("room workload id is required")
	}
	return queue.enqueue(ctx, roomWorkloadJob{workloadID: identity.WorkloadID, payload: payload, cancel: cancel})
}

func (control *roomControl) processRoomWorkloadJob(ctx context.Context, job roomWorkloadJob) error {
	if !job.cancel {
		return control.workloads.Submit(ctx, job.payload)
	}
	var cancel contracts.RoomWorkloadCancelWire
	if err := decodeStrictRoomWire(job.payload, &cancel); err != nil {
		return err
	}
	return control.workloads.Cancel(ctx, cancel)
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

func decodeControlPayload(encoded []byte, messageType string) ([]byte, error) {
	var payload map[string]any
	if err := msgpack.Unmarshal(encoded, &payload); err != nil {
		return nil, err
	}
	if payload["type"] != messageType {
		return nil, fmt.Errorf("BeamCore %s payload has type %v", messageType, payload["type"])
	}
	return json.Marshal(payload)
}

func (control *roomControl) handleTaskOfferBatch(ctx context.Context, encoded []byte) error {
	payload, err := decodeControlPayload(encoded, "task_offer_batch")
	if err != nil {
		return err
	}
	var batch beamcoreadapter.TaskOfferBatch
	if err := json.Unmarshal(payload, &batch); err != nil {
		return err
	}
	if batch.BatchID == "" || len(batch.Offers) == 0 {
		return errors.New("BeamCore task offer batch is empty")
	}
	if control.tasks.BatchCancelled(batch.BatchID) {
		return errors.New("batch_cancelled")
	}
	receivedAt := time.Now()
	timeout := time.Duration(batch.AssignmentTimeoutMS) * time.Millisecond
	specs := make([]domain.Spec, 0, len(batch.Offers))
	offers := make(map[string]struct{}, len(batch.Offers))
	for _, offer := range batch.Offers {
		if _, duplicate := offers[offer.OfferID]; duplicate {
			return errors.New("duplicate task offer identity")
		}
		offers[offer.OfferID] = struct{}{}
		spec, err := beamcoreadapter.ToWorkload(offer, timeout, receivedAt)
		if err != nil {
			return err
		}
		specs = append(specs, spec)
	}
	for _, spec := range specs {
		if _, err := control.tasks.Dispatch(ctx, dispatch.DispatchRequest{
			Source: dispatch.SourceBeamCore, ExternalID: spec.AttemptID, BatchID: batch.BatchID, Spec: spec,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (control *roomControl) handleCancel(ctx context.Context, encoded []byte) error {
	payload, err := decodeControlPayload(encoded, "room_task_cancel")
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
	payload, err := decodeControlPayload(encoded, "room_task_offer_batch")
	if err != nil {
		return err
	}
	var batch contracts.RoomTaskOfferBatch
	if err := json.Unmarshal(payload, &batch); err != nil {
		return err
	}
	return control.rooms.Submit(ctx, batch)
}

func (control *roomControl) handleTaskOfferBatchCancel(ctx context.Context, encoded []byte) error {
	encoded, err := decodeControlPayload(encoded, "task_offer_batch_cancel")
	if err != nil {
		return err
	}
	var payload struct {
		BatchIDs []string `json:"batch_ids"`
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return err
	}
	if len(payload.BatchIDs) == 0 || control.tasks == nil {
		return errors.New("invalid batch cancellation")
	}
	for _, batchID := range payload.BatchIDs {
		if strings.TrimSpace(batchID) == "" {
			return errors.New("invalid batch cancellation")
		}
	}
	return control.tasks.CancelBatches(ctx, payload.BatchIDs)
}

func (control *roomControl) submitResult(ctx context.Context, result contracts.RoomTaskResult) error {
	return control.request(ctx, "room_task_result", result)
}

func (control *roomControl) submitTaskResult(ctx context.Context, record dispatch.Record, result domain.Result) error {
	payload, err := taskOfferResult(record, result)
	if err != nil {
		return err
	}
	return control.request(ctx, "task_offer_result", payload)
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

func (control *roomControl) SubmitRoomMessageRuntime(ctx context.Context, value contracts.RoomMessageRuntimeWire) error {
	return control.request(ctx, contracts.RoomWorkloadRuntimeType, value)
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
	if control.tasks.CapabilityAvailable(contracts.TransferMultipartFanoutCapability, domain.Resources{MemoryBytes: 96 << 20, Connections: 9, Streams: 9}) {
		capabilities = append(capabilities, contracts.TransferMultipartFanoutCapability)
	}
	if control.rooms != nil {
		e2ee, storage := control.rooms.CapabilityAvailable(), control.rooms.StorageCapabilityAvailable()
		if e2ee || storage {
			capabilities = append(capabilities, contracts.RoomTransferCapability)
		}
		if e2ee {
			capabilities = append(capabilities, contracts.RoomTransferDirectCapability, contracts.RoomTransferE2EECapability)
		}
		if storage {
			capabilities = append(capabilities, contracts.RoomStorageCapability)
		}
	}
	for _, kind := range []domain.Kind{domain.KindRoomDatagram, domain.KindRoomMessage, domain.KindRoomCommand,
		domain.KindRoomStream, domain.KindRoomMedia} {
		if control.workloads == nil || !control.workloads.CapabilityAvailable(kind) {
			continue
		}
		capabilities = append(capabilities, string(kind))
	}
	if control.workloads != nil && control.workloads.CapacityCapabilityAvailable(
		domain.KindRoomMessage, contracts.RoomMessageDirectCapability) {
		capabilities = append(capabilities, contracts.RoomMessageDirectCapability)
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
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var normalizedPayload map[string]any
	if err := json.Unmarshal(jsonPayload, &normalizedPayload); err != nil {
		return err
	}
	if normalizedPayload == nil {
		normalizedPayload = map[string]any{}
	}
	normalizedPayload["type"] = messageType
	encoded, err := msgpack.Marshal(normalizedPayload)
	if err != nil {
		return err
	}
	requestContext, cancel := context.WithTimeout(ctx, control.config.RequestTimeout)
	defer cancel()
	message, err := control.conn.requestWithContext(requestContext, control.subject("orch", messageType), encoded)
	if err != nil {
		return err
	}
	var response map[string]any
	if err := msgpack.Unmarshal(message.Data, &response); err != nil {
		return err
	}
	if replyType, _ := response["type"].(string); replyType != controlReplyType(messageType) {
		reason, _ := response["reason"].(string)
		if reason == "" {
			reason, _ = response["message"].(string)
		}
		return fmt.Errorf("BeamCore rejected %s with %v: %s", messageType, response["type"], fallback(reason, "no reason supplied"))
	}
	encodedPayload, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if messageType == "capability_update" {
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
	if messageType == "task_offer_result" {
		var acknowledgement taskOfferResultAcknowledgement
		if err := json.Unmarshal(encodedPayload, &acknowledgement); err != nil {
			return err
		}
		return taskOfferResultDisposition(acknowledgement)
	}
	if messageType == "room_task_result" {
		var acknowledgement struct {
			Received bool   `json:"received"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(encodedPayload, &acknowledgement); err != nil {
			return err
		}
		if !acknowledgement.Received {
			return fmt.Errorf("BeamCore rejected %s: %s", messageType, fallback(acknowledgement.Reason, "not received"))
		}
	}
	return nil
}

func controlReplyType(messageType string) string {
	if messageType == "set_ready" {
		return "ready_state"
	}
	return messageType + "_ack"
}

func (control *roomControl) subject(direction, messageType string) string {
	return fmt.Sprintf("%s.%s.%s.%s.%s", strings.Trim(control.config.ControlPrefix, "."), control.config.Environment,
		direction, strings.ToLower(control.config.Hotkey), messageType)
}
