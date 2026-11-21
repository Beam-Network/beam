package roomtransfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type RedeemRequest struct {
	SchemaVersion string                      `json:"schema_version"`
	Type          string                      `json:"type"`
	BatchID       string                      `json:"batch_id"`
	RoomID        string                      `json:"room_id"`
	TransferID    string                      `json:"transfer_id"`
	LaneID        string                      `json:"lane_id"`
	WorkerID      string                      `json:"worker_id"`
	NodeID        string                      `json:"node_id"`
	Intent        contracts.TunnelLeaseIntent `json:"intent"`
}

type Provisioner interface {
	Redeem(context.Context, RedeemRequest) (contracts.TunnelLease, error)
}

type ResultSink interface {
	SubmitRoomTaskResult(context.Context, contracts.RoomTaskResult) error
}

type Config struct {
	Now       func() time.Time
	Resources domain.Resources
}

type Service struct {
	mu          sync.RWMutex
	batchLocks  sync.Map
	laneLocks   sync.Map
	config      Config
	dispatcher  *dispatch.Service
	store       Store
	provisioner Provisioner
	sink        ResultSink
}

func NewService(config Config, dispatcher *dispatch.Service, store Store) (*Service, error) {
	if dispatcher == nil || store == nil {
		return nil, errors.New("room transfer dispatcher and store are required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Resources.MemoryBytes == 0 {
		config.Resources.MemoryBytes = 96 << 20
	}
	if config.Resources.BandwidthMbps == 0 {
		config.Resources.BandwidthMbps = 10
	}
	if config.Resources.Connections == 0 {
		config.Resources.Connections = 16
	}
	service := &Service{config: config, dispatcher: dispatcher, store: store}
	dispatcher.RegisterSink(dispatch.SourceRoomTransfer, service)
	return service, nil
}

func (s *Service) RegisterProvisioner(value Provisioner) {
	s.mu.Lock()
	s.provisioner = value
	s.mu.Unlock()
}
func (s *Service) RegisterSink(value ResultSink) { s.mu.Lock(); s.sink = value; s.mu.Unlock() }

func (s *Service) CapabilityAvailable() bool {
	s.mu.RLock()
	ready := s.provisioner != nil && s.sink != nil
	s.mu.RUnlock()
	if !ready {
		return false
	}
	_, err := s.dispatcher.SelectWorker([]string{contracts.RoomTransferCapability, contracts.RoomTransferDirectCapability,
		contracts.RoomTransferE2EECapability}, s.config.Resources, nil)
	return err == nil
}

// Capability follows live worker availability, using the same placement rules.
func (s *Service) StorageCapabilityAvailable() bool {
	s.mu.RLock()
	ready := s.provisioner != nil && s.sink != nil
	s.mu.RUnlock()
	if !ready {
		return false
	}
	_, err := s.dispatcher.SelectWorker([]string{contracts.RoomTransferCapability, contracts.RoomStorageCapability}, s.config.Resources, nil)
	return err == nil
}

func (s *Service) LeaseRenewalAvailable() bool {
	s.mu.RLock()
	ready := s.provisioner != nil && s.sink != nil
	s.mu.RUnlock()
	if !ready {
		return false
	}
	_, err := s.dispatcher.SelectWorker([]string{contracts.RoomTransferCapability, contracts.RoomTransferLeaseRenewalCapability},
		s.config.Resources, nil)
	return err == nil
}

func (s *Service) StreamRelayAvailable() bool {
	s.mu.RLock()
	ready := s.provisioner != nil && s.sink != nil
	s.mu.RUnlock()
	if !ready {
		return false
	}
	_, err := s.dispatcher.SelectWorker([]string{contracts.RoomTransferCapability, contracts.RoomTransferStreamRelayCapability},
		s.config.Resources, nil)
	return err == nil
}

func (s *Service) ExtendLease(ctx context.Context, update contracts.RoomLaneLeaseUpdate) error {
	if err := update.Validate(s.config.Now().UTC()); err != nil {
		return err
	}
	lock := s.laneLock(update.BatchID, update.LaneID)
	lock.Lock()
	defer lock.Unlock()
	record, ok := s.store.Get(update.BatchID)
	if !ok || record.Batch.TransferID != update.TransferID || record.Batch.SchemaVersion != update.SchemaVersion {
		return errors.New("room lane lease update does not match a batch")
	}
	offer, ok := findOfferLane(record.Batch, update.LaneID)
	if !ok || offer.Attempt != update.Attempt {
		return errors.New("room lane lease update does not match a lane")
	}
	lane := record.Lanes[update.LaneID]
	if lane.State != LaneDispatched || lane.WorkloadKey == "" || lane.SourceLease == nil {
		return errors.New("room lane lease update names a lane that is not running")
	}
	expiries := make(map[string]time.Time, len(update.Leases))
	for _, lease := range update.Leases {
		expiries[lease.LeaseID] = lease.ExpiresAt.UTC()
	}
	named := 0
	extend := func(lease contracts.TunnelLease) contracts.TunnelLease {
		if expiresAt, ok := expiries[lease.LeaseID]; ok {
			named++
			if expiresAt.After(lease.ExpiresAt) {
				lease.ExpiresAt = expiresAt
			}
		}
		return lease
	}
	current := lane.SourceLease.ExpiresAt
	source := extend(*lane.SourceLease)
	targets := make(map[string]contracts.TunnelLease, len(lane.TargetLeases))
	expiresAt := source.ExpiresAt
	for memberID, lease := range lane.TargetLeases {
		current = minTime(current, lease.ExpiresAt)
		targets[memberID] = extend(lease)
		expiresAt = minTime(expiresAt, targets[memberID].ExpiresAt)
	}
	if named != len(expiries) {
		return errors.New("room lane lease update names a lease of another lane")
	}
	if !expiresAt.After(current) {
		return nil
	}
	if err := s.dispatcher.ExtendWorkload(ctx, lane.WorkloadKey, contracts.RoomTransferLeaseRenewalCapability, expiresAt, expiries); err != nil {
		return err
	}
	lane.SourceLease, lane.TargetLeases = &source, targets
	return s.saveLane(update.BatchID, lane)
}

func (s *Service) Cancel(ctx context.Context, request contracts.RoomTaskCancel) error {
	if err := request.Validate(); err != nil {
		return err
	}
	var failures []error
	for _, snapshot := range s.store.List() {
		if snapshot.Batch.TransferID != request.TransferID || snapshot.Batch.SchemaVersion != request.SchemaVersion {
			continue
		}
		failures = append(failures, s.cancelBatch(ctx, snapshot.Batch.BatchID, request)...)
	}
	return errors.Join(failures...)
}

func (s *Service) cancelBatch(ctx context.Context, batchID string, request contracts.RoomTaskCancel) []error {
	var failures []error
	cancelled, refused := make(map[string]bool), make(map[string]bool)
	for round := 0; round < 4; round++ {
		lock := s.batchLock(batchID)
		lock.Lock()
		record, ok := s.store.Get(batchID)
		if !ok {
			lock.Unlock()
			return failures
		}
		pending := make(map[string]string)
		changed := false
		for laneID, lane := range record.Lanes {
			if request.LaneID != "" {
				offer, found := findOfferLane(record.Batch, laneID)
				if !found || offer.LaneID != request.LaneID || offer.Attempt != request.Attempt {
					continue
				}
			}
			if lane.State == LaneCompleted || lane.State == LaneFailed || lane.State == LaneCancelled || refused[lane.WorkloadKey] {
				continue
			}
			if lane.WorkloadKey != "" && !cancelled[lane.WorkloadKey] {
				pending[laneID] = lane.WorkloadKey
				continue
			}
			lane.State, lane.Error = LaneCancelled, request.Reason
			record.Lanes[laneID] = lane
			changed = true
		}
		if changed {
			record.UpdatedAt = request.CancelledAt.UTC()
			if err := s.store.Put(record); err != nil {
				failures = append(failures, err)
			}
		}
		lock.Unlock()
		if len(pending) == 0 {
			return failures
		}
		for laneID, key := range pending {
			if err := s.dispatcher.CancelWorkload(ctx, key, request.Reason); err != nil {
				failures = append(failures, fmt.Errorf("cancel room lane %s: %w", laneID, err))
				refused[key] = true
				continue
			}
			cancelled[key] = true
		}
	}
	return failures
}

func (s *Service) Accept(batch contracts.RoomTaskOfferBatch) error {
	lock := s.batchLock(batch.BatchID)
	lock.Lock()
	defer lock.Unlock()
	now := s.config.Now().UTC()
	if err := batch.Validate(now); err != nil {
		return err
	}
	record, exists := s.store.Get(batch.BatchID)
	if exists {
		if !sameBatch(record.Batch, batch) {
			return errors.New("room transfer batch idempotency key belongs to another offer")
		}
		return nil
	}
	record = Record{Batch: batch, Lanes: make(map[string]LaneRecord), CreatedAt: now, UpdatedAt: now}
	for _, lane := range batch.Lanes {
		record.Lanes[lane.LaneID] = newLaneRecord(lane.LaneID)
	}
	return s.store.Put(record)
}

func (s *Service) Submit(ctx context.Context, batch contracts.RoomTaskOfferBatch) error {
	if err := s.Accept(batch); err != nil {
		return err
	}
	failures := make([]error, len(batch.Lanes))
	var group sync.WaitGroup
	for index, offerLane := range batch.Lanes {
		group.Add(1)
		go func() {
			defer group.Done()
			failures[index] = s.runLane(ctx, batch, offerLane)
		}()
	}
	group.Wait()
	return errors.Join(failures...)
}

func (s *Service) runLane(ctx context.Context, batch contracts.RoomTaskOfferBatch, offerLane contracts.RoomSourceLane) error {
	lock := s.laneLock(batch.BatchID, offerLane.LaneID)
	lock.Lock()
	defer lock.Unlock()
	err := s.startLane(ctx, batch, offerLane)
	if errors.Is(err, errLaneCancelled) {
		return nil
	}
	return err
}

func (s *Service) startLane(ctx context.Context, batch contracts.RoomTaskOfferBatch, offerLane contracts.RoomSourceLane) error {
	record, ok := s.store.Get(batch.BatchID)
	if !ok {
		return errors.New("room transfer batch disappeared")
	}
	lane := record.Lanes[offerLane.LaneID]
	if lane.State == LaneCompleted || lane.State == LaneDispatched || lane.State == LaneFailed || lane.State == LaneCancelled {
		return nil
	}
	if lane.WorkerID == "" {
		placement, err := s.dispatcher.SelectWorker(batchCapabilities(batch), s.resourcesFor(batch, offerLane), nil)
		if err != nil {
			return fmt.Errorf("select Worker for room lane %s: %w", offerLane.LaneID, err)
		}
		lane.WorkerID, lane.NodeID = placement.WorkerID, placement.NodeID
	}
	if lane.SourceLease == nil || len(lane.TargetLeases) == 0 && len(lane.ProvisionFailures) == 0 {
		provisionErr := s.provision(ctx, batch, offerLane, &lane)
		if err := s.saveLane(batch.BatchID, lane); err != nil {
			return err
		}
		if provisionErr != nil {
			return provisionErr
		}
	}
	if len(lane.ProvisionFailures) > 0 {
		result := s.provisioningResult(batch, offerLane, lane)
		if err := s.submitResult(ctx, &lane, result); err != nil {
			return err
		}
		if err := s.saveLane(batch.BatchID, lane); err != nil {
			return err
		}
	}
	if lane.SourceLease == nil || len(lane.TargetLeases) == 0 {
		lane.State = LaneCompleted
		return s.saveLane(batch.BatchID, lane)
	}
	lane.State = LaneProvisioned
	workload, err := s.workload(batch, offerLane, lane)
	if err != nil {
		return err
	}
	dispatched, err := s.dispatcher.Dispatch(ctx, dispatch.DispatchRequest{Source: dispatch.SourceRoomTransfer,
		ExternalID: batch.BatchID + "/" + lane.LaneID, Spec: workload, WorkerID: lane.WorkerID, NodeID: lane.NodeID,
		BeforeCommit: func(_ context.Context, task dispatch.Record) error {
			acknowledgedAt := s.config.Now().UTC()
			lane.WorkloadKey, lane.WorkerAcknowledgedAt = task.WorkloadKey, &acknowledgedAt
			lane.State = LaneProvisioned
			return s.saveLane(batch.BatchID, lane)
		}})
	if errors.Is(err, errLaneCancelled) {
		return err
	}
	if err != nil {
		lane.Error = err.Error()
		result := s.dispatchFailureResult(batch, offerLane, lane, err)
		if resultErr := s.submitResult(ctx, &lane, result); resultErr != nil {
			if saveErr := s.saveLane(batch.BatchID, lane); saveErr != nil {
				return errors.Join(err, resultErr, saveErr)
			}
			return errors.Join(err, resultErr)
		}
		lane.State = LaneCompleted
		return s.saveLane(batch.BatchID, lane)
	}
	lane.State, lane.WorkloadKey = LaneDispatched, dispatched.WorkloadKey
	return s.saveLane(batch.BatchID, lane)
}

func (s *Service) DeliverCheckpoint(ctx context.Context, checkpoint domain.Checkpoint) error {
	if (checkpoint.Schema != contracts.RoomTransferSchemaVersion && checkpoint.Schema != contracts.RoomStorageSchemaVersion) || checkpoint.Sequence == 0 {
		return nil
	}
	key := checkpoint.WorkloadID + "/" + checkpoint.AttemptID
	record, laneID, found := s.findByWorkload(key)
	if !found {
		return dispatch.TerminalDelivery(errors.New("room transfer checkpoint does not match a durable lane"))
	}
	lock := s.laneLock(record.Batch.BatchID, laneID)
	lock.Lock()
	defer lock.Unlock()
	record, ok := s.store.Get(record.Batch.BatchID)
	if !ok {
		return errors.New("room transfer batch disappeared")
	}
	lane := record.Lanes[laneID]
	if lane.State != LaneDispatched {
		return nil
	}
	offer, ok := findOfferLane(record.Batch, laneID)
	if !ok {
		return errors.New("room transfer lane disappeared")
	}
	var value struct {
		SourceReceipts map[int64]contracts.SourceRangeReceipt  `json:"source_receipts"`
		TargetReceipts map[string]contracts.TargetRangeReceipt `json:"target_receipts"`
		FinalReceipts  map[string]contracts.FinalTargetReceipt `json:"final_receipts"`
		StorageResults map[string]contracts.StorageRangeResult `json:"storage_results"`
		SourceReads    map[int64]contracts.SourceReadEvidence  `json:"source_reads"`
	}
	if err := json.Unmarshal(checkpoint.Payload, &value); err != nil {
		return fmt.Errorf("decode room transfer checkpoint: %w", err)
	}
	result := contracts.RoomTaskResult{Type: "room_task_result", SchemaVersion: record.Batch.SchemaVersion,
		ResultID: resultID(record.Batch.TransferID, laneID, offer.Attempt, fmt.Sprintf("checkpoint:%d", checkpoint.Sequence)),
		BatchID:  record.Batch.BatchID, RoomID: record.Batch.RoomID, TransferID: record.Batch.TransferID, LaneID: laneID,
		Attempt: offer.Attempt, WorkerID: lane.WorkerID, WorkerAcknowledgedAt: lane.WorkerAcknowledgedAt,
		ExecutableLeaseIssuedAt: lane.ExecutableLeaseIssuedAt, Runtime: lane.Runtime,
		ExecutionStage: "streaming", ReportedAt: s.config.Now().UTC()}
	for _, read := range value.SourceReads {
		result.SourceReads = append(result.SourceReads, read)
	}
	for _, evidence := range value.StorageResults {
		result.StorageResults = append(result.StorageResults, evidence)
	}
	for _, receipt := range value.SourceReceipts {
		result.SourceReceipts = append(result.SourceReceipts, receipt)
	}
	for _, receipt := range value.TargetReceipts {
		result.TargetReceipts = append(result.TargetReceipts, receipt)
	}
	for _, receipt := range value.FinalReceipts {
		result.FinalTargetReceipts = append(result.FinalTargetReceipts, receipt)
	}
	if len(result.FinalTargetReceipts) > 0 {
		result.ExecutionStage = "finalizing"
	}
	if err := s.verifyReceipts(record.Batch, offer, lane, result); err != nil {
		return err
	}
	if err := s.submitResult(ctx, &lane, result); err != nil {
		return err
	}
	lane.SourceReceipts, lane.TargetReceipts, lane.FinalTargetReceipts = result.SourceReceipts, result.TargetReceipts, result.FinalTargetReceipts
	lane.StorageResults = result.StorageResults
	lane.SourceReads = result.SourceReads
	return s.saveLane(record.Batch.BatchID, lane)
}

func (s *Service) provision(ctx context.Context, batch contracts.RoomTaskOfferBatch, offer contracts.RoomSourceLane, lane *LaneRecord) error {
	s.mu.RLock()
	provisioner := s.provisioner
	s.mu.RUnlock()
	if provisioner == nil {
		return errors.New("Tunnel lease provisioner is not connected")
	}
	type outcome struct {
		intent contracts.TunnelLeaseIntent
		lease  contracts.TunnelLease
		err    error
	}
	intents := append([]contracts.TunnelLeaseIntent{offer.SourceIntent}, offer.TargetIntents...)
	outcomes := make(chan outcome, len(intents))
	var group sync.WaitGroup
	for _, intent := range intents {
		intent := intent
		group.Add(1)
		go func() {
			defer group.Done()
			lease, err := provisioner.Redeem(ctx, s.redeemRequest(batch, offer.LaneID, *lane, intent))
			if err == nil {
				err = verifyRedeemedLease(intent, lease, s.config.Now().UTC())
			}
			outcomes <- outcome{intent: intent, lease: lease, err: err}
		}()
	}
	group.Wait()
	close(outcomes)
	lane.TargetLeases = make(map[string]contracts.TunnelLease)
	lane.ProvisionFailures = make(map[string]contracts.RoomFailure)
	for result := range outcomes {
		if result.err != nil {
			key := result.intent.TargetMemberID
			if result.intent.Role == contracts.TunnelLeaseRoleSourceRead {
				lane.Error = result.err.Error()
				for _, memberID := range offer.TargetMemberIDs {
					lane.ProvisionFailures[memberID] = contracts.RoomFailure{Origin: "room_coordinator", Code: "source_tunnel_unavailable",
						Retryable: true, TargetMemberID: memberID, Detail: result.err.Error()}
				}
				continue
			}
			lane.ProvisionFailures[key] = contracts.RoomFailure{Origin: "room_coordinator", Code: "target_tunnel_unavailable",
				Retryable: true, TargetMemberID: key, Detail: result.err.Error()}
			continue
		}
		if result.intent.Role == contracts.TunnelLeaseRoleSourceRead {
			lease := result.lease
			lane.SourceLease = &lease
		} else {
			lane.TargetLeases[result.intent.TargetMemberID] = result.lease
		}
	}
	if lane.SourceLease != nil && len(lane.TargetLeases) > 0 {
		issuedAt := s.config.Now().UTC()
		lane.ExecutableLeaseIssuedAt = &issuedAt
	}
	return nil
}

func (s *Service) DeliverResult(ctx context.Context, task dispatch.Record, result domain.Result) error {
	record, laneID, found := s.findByWorkload(task.WorkloadKey)
	if !found {
		return dispatch.TerminalDelivery(errors.New("room transfer result does not match a durable lane"))
	}
	lock := s.laneLock(record.Batch.BatchID, laneID)
	lock.Lock()
	defer lock.Unlock()
	record, ok := s.store.Get(record.Batch.BatchID)
	if !ok {
		return errors.New("room transfer batch disappeared")
	}
	lane := record.Lanes[laneID]
	offer, ok := findOfferLane(record.Batch, laneID)
	if !ok {
		return errors.New("room transfer lane disappeared")
	}
	roomResult := contracts.RoomTaskResult{Type: "room_task_result", SchemaVersion: record.Batch.SchemaVersion,
		ResultID: resultID(record.Batch.TransferID, laneID, offer.Attempt, "terminal"), BatchID: record.Batch.BatchID,
		RoomID: record.Batch.RoomID, TransferID: record.Batch.TransferID, LaneID: laneID, Attempt: offer.Attempt,
		WorkerID: lane.WorkerID, WorkerAcknowledgedAt: lane.WorkerAcknowledgedAt,
		ExecutableLeaseIssuedAt: lane.ExecutableLeaseIssuedAt, Runtime: lane.Runtime,
		ExecutionStage: "terminal", ReportedAt: s.config.Now().UTC()}
	if err := decodeOutput(result.Outputs, "source_reads", &roomResult.SourceReads); err != nil {
		return err
	}
	if err := decodeOutput(result.Outputs, "storage_results", &roomResult.StorageResults); err != nil {
		return err
	}
	if err := decodeOutput(result.Outputs, "source_receipts", &roomResult.SourceReceipts); err != nil {
		return err
	}
	if err := decodeOutput(result.Outputs, "target_receipts", &roomResult.TargetReceipts); err != nil {
		return err
	}
	if err := decodeOutput(result.Outputs, "final_target_receipts", &roomResult.FinalTargetReceipts); err != nil {
		return err
	}
	if err := decodeOutput(result.Outputs, "missing", &roomResult.Missing); err != nil {
		return err
	}
	if err := decodeOutput(result.Outputs, "room_failures", &roomResult.Failures); err != nil {
		return err
	}
	if result.State != domain.StateCompleted && result.State != domain.StateReceiptCommitted && len(roomResult.Failures) == 0 {
		roomResult.Failures = append(roomResult.Failures, contracts.RoomFailure{Origin: "worker", Code: "worker_execution_failed",
			Retryable: true, Detail: fallback(result.ErrorMessage, "room transfer Worker failed")})
		roomResult.Missing = missingForTargets(offer, keys(lane.TargetLeases))
	}
	if err := s.verifyReceipts(record.Batch, offer, lane, roomResult); err != nil {
		return err
	}
	if err := s.submitResult(ctx, &lane, roomResult); err != nil {
		return err
	}
	lane.SourceReceipts, lane.TargetReceipts, lane.FinalTargetReceipts = roomResult.SourceReceipts, roomResult.TargetReceipts, roomResult.FinalTargetReceipts
	lane.StorageResults = roomResult.StorageResults
	lane.SourceReads = roomResult.SourceReads
	lane.State = LaneCompleted
	return s.saveLane(record.Batch.BatchID, lane)
}

func (s *Service) DeliverProgress(ctx context.Context, task dispatch.Record, progress domain.Progress) error {
	encoded := strings.TrimSpace(progress.Outputs["room_transfer_runtime"])
	if encoded == "" {
		return nil
	}
	record, laneID, found := s.findByWorkload(task.WorkloadKey)
	if !found {
		return errors.New("room transfer progress does not match a durable lane")
	}
	lock := s.laneLock(record.Batch.BatchID, laneID)
	lock.Lock()
	defer lock.Unlock()
	record, ok := s.store.Get(record.Batch.BatchID)
	if !ok {
		return errors.New("room transfer batch disappeared")
	}
	lane := record.Lanes[laneID]
	offer, ok := findOfferLane(record.Batch, laneID)
	if !ok {
		return errors.New("room transfer lane disappeared")
	}
	var directRuntime contracts.DirectRoomTransferRuntime
	if err := json.Unmarshal([]byte(encoded), &directRuntime); err != nil {
		return fmt.Errorf("decode room transfer runtime: %w", err)
	}
	if err := directRuntime.Validate(lane.WorkerID, laneID, offer.Attempt, s.config.Now().UTC()); err != nil {
		return err
	}
	if (record.Batch.SchemaVersion == contracts.RoomStorageSchemaVersion) != (directRuntime.Capability == contracts.RoomStorageCapability) {
		return errors.New("worker transport protection differs from its assignment")
	}
	roomResult := contracts.RoomTaskResult{Type: "room_task_result", SchemaVersion: record.Batch.SchemaVersion,
		ResultID: resultID(record.Batch.TransferID, laneID, offer.Attempt,
			"runtime:"+directRuntime.AccessToken+":"+directRuntime.ExpiresAt.UTC().Format(time.RFC3339Nano)),
		BatchID: record.Batch.BatchID, RoomID: record.Batch.RoomID, TransferID: record.Batch.TransferID,
		LaneID: laneID, Attempt: offer.Attempt, WorkerID: lane.WorkerID,
		WorkerAcknowledgedAt: lane.WorkerAcknowledgedAt, ExecutableLeaseIssuedAt: lane.ExecutableLeaseIssuedAt,
		ExecutionStage: "streaming", Runtime: &directRuntime, ReportedAt: progress.ObservedAt.UTC()}
	if err := s.submitResult(ctx, &lane, roomResult); err != nil {
		return err
	}
	lane.Runtime = &directRuntime
	return s.saveLane(record.Batch.BatchID, lane)
}

func (s *Service) Replay(ctx context.Context) {
	for _, record := range s.store.List() {
		_ = s.Submit(ctx, record.Batch)
	}
}
