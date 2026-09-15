package roomtransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func (s *Service) resourcesFor(batch contracts.RoomTaskOfferBatch, lane contracts.RoomSourceLane) domain.Resources {
	resources := s.config.Resources
	resources.MemoryBytes = max(resources.MemoryBytes, contracts.RoomBufferMemoryBytes(batch.FileSizeBytes, batch.ChunkSizeBytes))
	connections := len(lane.TargetMemberIDs) + 1
	if batch.SchemaVersion == contracts.RoomStorageSchemaVersion {
		connections = min(8, len(lane.TargetMemberIDs)) + 1
	}
	resources.Connections = max(resources.Connections, int64(connections))
	return resources
}

func (s *Service) redeemRequest(batch contracts.RoomTaskOfferBatch, laneID string, lane LaneRecord,
	intent contracts.TunnelLeaseIntent) RedeemRequest {
	return RedeemRequest{SchemaVersion: batch.SchemaVersion, Type: "room_tunnel_lease_redeem",
		BatchID: batch.BatchID, RoomID: batch.RoomID, TransferID: batch.TransferID, LaneID: laneID,
		WorkerID: lane.WorkerID, NodeID: lane.NodeID, Intent: intent}
}

func (s *Service) workload(batch contracts.RoomTaskOfferBatch, offer contracts.RoomSourceLane, lane LaneRecord) (domain.Spec, error) {
	targets := make([]contracts.RoomTransferDestination, 0, len(lane.TargetLeases))
	expiresAt := lane.SourceLease.ExpiresAt
	for _, memberID := range offer.TargetMemberIDs {
		lease, ok := lane.TargetLeases[memberID]
		if !ok {
			continue
		}
		if lease.ExpiresAt.Before(expiresAt) {
			expiresAt = lease.ExpiresAt
		}
		targets = append(targets, contracts.RoomTransferDestination{MemberID: memberID, Lease: lease})
	}
	if len(targets) == 0 {
		return domain.Spec{}, errors.New("room lane has no admitted destinations")
	}
	if offer.TimingBudgetSeconds > 0 {
		expiresAt = minTime(expiresAt, s.config.Now().UTC().Add(time.Duration(offer.TimingBudgetSeconds)*time.Second))
	}
	payload, err := json.Marshal(contracts.RoomTransfer{SchemaVersion: batch.SchemaVersion,
		BatchID: batch.BatchID, RoomID: batch.RoomID, ChannelID: batch.ChannelID, PublicationID: batch.PublicationID,
		TransferID: batch.TransferID, SnapshotVersion: batch.SnapshotVersion, Protection: batch.Protection,
		LaneID: offer.LaneID, Attempt: offer.Attempt,
		FileSizeBytes: batch.FileSizeBytes, ChunkSizeBytes: batch.ChunkSizeBytes, ChunkCount: batch.ChunkCount,
		ChunkStart: offer.ChunkStart, ChunkEnd: offer.ChunkEnd, SourceLease: *lane.SourceLease, Targets: targets})
	if err != nil {
		return domain.Spec{}, err
	}
	workID, attemptID := workloadIdentity(batch.BatchID, offer.LaneID, batch.SnapshotVersion, offer.Attempt)
	return domain.Spec{WorkloadID: workID, AttemptID: attemptID,
		Identity: domain.Identity{WorkerID: lane.WorkerID, NodeID: lane.NodeID}, Kind: domain.KindRoomTransfer,
		Class: domain.ClassJob, Source: domain.Source{System: "beamcore.room", Reference: batch.TransferID},
		RequiredCapabilities: batchCapabilities(batch), Resources: s.resourcesFor(batch, offer),
		Lease:    domain.Lease{OfferExpiresAt: minTime(batch.OfferExpiresAt, expiresAt), AssignmentExpiresAt: expiresAt},
		Evidence: domain.EvidencePolicy{ReceiptRequired: true, Commitments: []string{"target_receipts"}}, Payload: payload}, nil
}

func (s *Service) provisioningResult(batch contracts.RoomTaskOfferBatch, offer contracts.RoomSourceLane, lane LaneRecord) contracts.RoomTaskResult {
	failures := make([]contracts.RoomFailure, 0, len(lane.ProvisionFailures))
	failedTargets := make([]string, 0, len(lane.ProvisionFailures))
	for memberID, failure := range lane.ProvisionFailures {
		failures = append(failures, failure)
		failedTargets = append(failedTargets, memberID)
	}
	sort.Strings(failedTargets)
	stage := "provisioning"
	if lane.SourceLease == nil || len(lane.TargetLeases) == 0 {
		stage = "terminal"
	}
	return contracts.RoomTaskResult{Type: "room_task_result", SchemaVersion: batch.SchemaVersion,
		ResultID: resultID(batch.TransferID, offer.LaneID, offer.Attempt, "provisioning:"+fmt.Sprint(failedTargets)),
		BatchID:  batch.BatchID, RoomID: batch.RoomID, TransferID: batch.TransferID, LaneID: offer.LaneID, Attempt: offer.Attempt,
		WorkerID: lane.WorkerID, ExecutionStage: stage, Missing: missingForTargets(offer, failedTargets),
		Failures: failures, ReportedAt: s.config.Now().UTC()}
}

func (s *Service) dispatchFailureResult(batch contracts.RoomTaskOfferBatch, offer contracts.RoomSourceLane, lane LaneRecord,
	dispatchErr error) contracts.RoomTaskResult {
	return contracts.RoomTaskResult{Type: "room_task_result", SchemaVersion: batch.SchemaVersion,
		ResultID: resultID(batch.TransferID, offer.LaneID, offer.Attempt, "dispatch_failed"), BatchID: batch.BatchID,
		RoomID: batch.RoomID, TransferID: batch.TransferID, LaneID: offer.LaneID, Attempt: offer.Attempt,
		WorkerID: lane.WorkerID, WorkerAcknowledgedAt: lane.WorkerAcknowledgedAt,
		ExecutableLeaseIssuedAt: lane.ExecutableLeaseIssuedAt, ExecutionStage: "terminal",
		Missing: missingForTargets(offer, keys(lane.TargetLeases)), Failures: []contracts.RoomFailure{{Origin: "orchestrator",
			Code: "worker_dispatch_failed", Retryable: true, Detail: dispatchErr.Error()}}, ReportedAt: s.config.Now().UTC()}
}

func (s *Service) submitResult(ctx context.Context, lane *LaneRecord, result contracts.RoomTaskResult) error {
	if result.SourceReceipts == nil {
		result.SourceReceipts = []contracts.SourceRangeReceipt{}
	}
	if result.TargetReceipts == nil {
		result.TargetReceipts = []contracts.TargetRangeReceipt{}
	}
	if result.FinalTargetReceipts == nil {
		result.FinalTargetReceipts = []contracts.FinalTargetReceipt{}
	}
	if result.Missing == nil {
		result.Missing = []contracts.RoomMissingCells{}
	}
	if result.Failures == nil {
		result.Failures = []contracts.RoomFailure{}
	}
	if lane.ReportedResultIDs == nil {
		lane.ReportedResultIDs = make(map[string]bool)
	}
	if lane.ReportedResultIDs[result.ResultID] {
		return nil
	}
	s.mu.RLock()
	sink := s.sink
	s.mu.RUnlock()
	if sink == nil {
		return errors.New("BeamCore room result sink is not connected")
	}
	if err := sink.SubmitRoomTaskResult(ctx, result); err != nil {
		return err
	}
	lane.ReportedResultIDs[result.ResultID] = true
	return nil
}

func (s *Service) verifyReceipts(batch contracts.RoomTaskOfferBatch, offer contracts.RoomSourceLane, lane LaneRecord,
	result contracts.RoomTaskResult) error {
	now := s.config.Now().UTC()
	if result.SchemaVersion != batch.SchemaVersion {
		return errors.New("room result schema differs from its assignment")
	}
	seenReads := make(map[int64]bool)
	for _, read := range result.SourceReads {
		if seenReads[read.ChunkIndex] || read.Validate(batch.FileSizeBytes, batch.ChunkSizeBytes, offer.ChunkStart, offer.ChunkEnd, batch.Protection) != nil {
			return errors.New("invalid source read evidence")
		}
		seenReads[read.ChunkIndex] = true
	}
	if err := verifyStorageEvidence(batch, offer, lane, result, now); err != nil {
		return err
	}
	for _, failure := range result.Failures {
		receipt := failure.SourceFailureReceipt
		terminalSourceClaim := failure.Origin == "source_agent" &&
			(failure.Code == "source_file_mutated" || failure.Code == "source_integrity_failed" || !failure.Retryable)
		if receipt == nil {
			if terminalSourceClaim {
				return errors.New("terminal source failure is missing signed agent evidence")
			}
			continue
		}
		if lane.SourceLease == nil || !terminalSourceClaim || receipt.Code != failure.Code || receipt.TransferID != batch.TransferID ||
			receipt.LaneID != offer.LaneID || receipt.ChunkIndex < offer.ChunkStart || receipt.ChunkIndex > offer.ChunkEnd ||
			!slices.Contains(failure.ChunkIndices, receipt.ChunkIndex) || receipt.Verify(lane.SourceLease.AgentPublicKey, now) != nil {
			return errors.New("source failure receipt does not match the room lane")
		}
	}
	sourceSeen := make(map[int64]bool)
	for _, receipt := range result.SourceReceipts {
		offset, length := contracts.ChunkRange(batch.FileSizeBytes, batch.ChunkSizeBytes, receipt.ChunkIndex)
		if lane.SourceLease == nil || sourceSeen[receipt.ChunkIndex] || receipt.TransferID != batch.TransferID || receipt.LaneID != offer.LaneID ||
			receipt.ChunkIndex < offer.ChunkStart || receipt.ChunkIndex > offer.ChunkEnd || receipt.Offset != offset ||
			receipt.Length != length || receipt.LeaseID != lane.SourceLease.LeaseID ||
			receipt.Verify(lane.SourceLease.AgentPublicKey, now) != nil {
			return errors.New("source receipt does not match the room lane")
		}
		sourceSeen[receipt.ChunkIndex] = true
	}
	seen := make(map[string]bool)
	for _, receipt := range result.TargetReceipts {
		lease, ok := lane.TargetLeases[receipt.TargetMemberID]
		offset, length := contracts.ChunkRange(batch.FileSizeBytes, batch.ChunkSizeBytes, receipt.ChunkIndex)
		key := fmt.Sprintf("%s:%d", receipt.TargetMemberID, receipt.ChunkIndex)
		if !ok || seen[key] || receipt.TransferID != batch.TransferID || receipt.LaneID != offer.LaneID ||
			receipt.ChunkIndex < offer.ChunkStart || receipt.ChunkIndex > offer.ChunkEnd || receipt.Offset != offset ||
			receipt.Length != length || receipt.LeaseID != lease.LeaseID || receipt.Verify(lease.AgentPublicKey, now) != nil {
			return errors.New("target receipt does not match the room lane")
		}
		seen[key] = true
	}
	finalSeen := make(map[string]bool)
	for _, receipt := range result.FinalTargetReceipts {
		lease, ok := lane.TargetLeases[receipt.TargetMemberID]
		if !ok || finalSeen[receipt.TargetMemberID] || receipt.TransferID != batch.TransferID ||
			receipt.FileSizeBytes != batch.FileSizeBytes || receipt.Verify(lease.AgentPublicKey, now) != nil {
			return errors.New("final target receipt does not match the room lane")
		}
		finalSeen[receipt.TargetMemberID] = true
	}
	return nil
}

func batchCapabilities(batch contracts.RoomTaskOfferBatch) []string {
	if batch.SchemaVersion == contracts.RoomStorageSchemaVersion {
		return []string{contracts.RoomTransferCapability, contracts.RoomStorageCapability}
	}
	return []string{contracts.RoomTransferCapability, contracts.RoomTransferDirectCapability, contracts.RoomTransferE2EECapability}
}

func verifyStorageEvidence(batch contracts.RoomTaskOfferBatch, offer contracts.RoomSourceLane, lane LaneRecord,
	result contracts.RoomTaskResult, now time.Time) error {
	if len(result.StorageResults) == 0 {
		return nil
	}
	if batch.SchemaVersion != contracts.RoomStorageSchemaVersion {
		return errors.New("agent-only result contains storage evidence")
	}
	seen := make(map[string]bool)
	hashes := make(map[int64]string)
	for _, receipt := range result.SourceReceipts {
		hashes[receipt.ChunkIndex] = receipt.RangeSHA256
	}
	for _, evidence := range result.StorageResults {
		lease := lane.SourceLease
		if evidence.Role == contracts.TunnelLeaseRoleTargetWrite {
			target, ok := lane.TargetLeases[evidence.MemberID]
			if !ok {
				return errors.New("storage result is outside the destination snapshot")
			}
			lease = &target
		}
		offset, length := contracts.ChunkRange(batch.FileSizeBytes, batch.ChunkSizeBytes, evidence.ChunkIndex)
		key := fmt.Sprintf("%s:%s:%d", evidence.Role, evidence.MemberID, evidence.ChunkIndex)
		digest, hashErr := hex.DecodeString(evidence.RangeSHA256)
		if lease == nil || lease.Storage == nil || lease.LeaseID != evidence.LeaseID || lease.Role != evidence.Role ||
			lease.Storage.MemberID != evidence.MemberID || evidence.ChunkIndex < offer.ChunkStart || evidence.ChunkIndex > offer.ChunkEnd ||
			evidence.Offset != offset || evidence.Length != length || seen[key] || hashErr != nil || len(digest) != sha256.Size ||
			evidence.CompletedAt.IsZero() || evidence.CompletedAt.After(now.Add(time.Minute)) || evidence.CompletedAt.After(lease.ExpiresAt) {
			return errors.New("storage result does not match its worker assignment")
		}
		if evidence.Role == contracts.TunnelLeaseRoleTargetWrite && (evidence.ETag == "" || evidence.UploadID == "" || evidence.PartNumber != contracts.MultipartAttemptPartNumber(evidence.ChunkIndex, offer.Attempt)) {
			return errors.New("storage result lacks multipart evidence")
		}
		if evidence.Role == contracts.TunnelLeaseRoleSourceRead {
			hashes[evidence.ChunkIndex] = evidence.RangeSHA256
		}
		seen[key] = true
	}
	for _, evidence := range result.StorageResults {
		if evidence.Role == contracts.TunnelLeaseRoleTargetWrite && hashes[evidence.ChunkIndex] != evidence.RangeSHA256 {
			return errors.New("storage delivery differs from its source range")
		}
	}
	for _, receipt := range result.TargetReceipts {
		if hashes[receipt.ChunkIndex] != receipt.RangeSHA256 {
			return errors.New("agent delivery differs from its hybrid source range")
		}
	}
	return nil
}

func (s *Service) findByWorkload(key string) (Record, string, bool) {
	for _, record := range s.store.List() {
		for laneID, lane := range record.Lanes {
			if lane.WorkloadKey == key {
				return record, laneID, true
			}
		}
	}
	return Record{}, "", false
}

func (s *Service) batchLock(batchID string) *sync.Mutex {
	value, _ := s.batchLocks.LoadOrStore(batchID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func newLaneRecord(laneID string) LaneRecord {
	return LaneRecord{LaneID: laneID, State: LanePending, TargetLeases: make(map[string]contracts.TunnelLease),
		ProvisionFailures: make(map[string]contracts.RoomFailure), ReportedResultIDs: make(map[string]bool)}
}

func verifyRedeemedLease(intent contracts.TunnelLeaseIntent, lease contracts.TunnelLease, now time.Time) error {
	expectedProtocol := contracts.RoomTransferDirectCapability
	if intent.RequiredWorkerCapability == contracts.RoomStorageCapability {
		expectedProtocol = contracts.RoomStorageCapability
	}
	if lease.IntentID != intent.IntentID || lease.ExpiresAt.After(intent.ExpiresAt) || lease.Protocol != expectedProtocol {
		return errors.New("Tunnel coordinator redeemed another intent")
	}
	return lease.Validate(intent.Role, intent.TargetMemberID, now)
}

func findOfferLane(batch contracts.RoomTaskOfferBatch, laneID string) (contracts.RoomSourceLane, bool) {
	for _, lane := range batch.Lanes {
		if lane.LaneID == laneID {
			return lane, true
		}
	}
	return contracts.RoomSourceLane{}, false
}

func missingForTargets(lane contracts.RoomSourceLane, targets []string) []contracts.RoomMissingCells {
	result := make([]contracts.RoomMissingCells, 0, len(targets))
	for _, target := range targets {
		entry := contracts.RoomMissingCells{TargetMemberID: target}
		for chunk := lane.ChunkStart; chunk <= lane.ChunkEnd; chunk++ {
			entry.ChunkIndices = append(entry.ChunkIndices, chunk)
		}
		result = append(result, entry)
	}
	return result
}

func keys(values map[string]contracts.TunnelLease) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	return result
}

func decodeOutput(outputs map[string]string, key string, destination any) error {
	value := outputs[key]
	if value == "" {
		value = "[]"
	}
	if err := json.Unmarshal([]byte(value), destination); err != nil {
		return fmt.Errorf("decode Worker %s: %w", key, err)
	}
	return nil
}

func workloadIdentity(batchID, laneID string, snapshot uint64, attempt int64) (string, string) {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", batchID, laneID, attempt)))
	token := hex.EncodeToString(digest[:12])
	return "room-transfer-" + token, fmt.Sprintf("snapshot-%d-attempt-%d", snapshot, attempt)
}

func resultID(transferID, laneID string, attempt int64, stage string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", transferID, laneID, attempt, stage)))
	return "room-result-" + hex.EncodeToString(digest[:16])
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func sameBatch(a, b contracts.RoomTaskOfferBatch) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}
func fallback(value, other string) string {
	if value == "" {
		return other
	}
	return value
}

var _ dispatch.ResultSink = (*Service)(nil)
var _ dispatch.ProgressSink = (*Service)(nil)
