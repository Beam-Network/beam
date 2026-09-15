package roomtransfer

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"strconv"
	"sync"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

type deliveryOutcome struct {
	target  contracts.RoomTransferDestination
	agent   targetResponse
	storage contracts.StorageRangeResult
	err     error
}

// Destination concurrency is independent of snapshot size. Every destination
// uses current.payload; the source reader is never called by delivery retries.
func (h *Handler) deliverChunk(ctx context.Context, workerID string, active *session, current *chunk,
	index int64, resume *checkpointValue) ([]contracts.RoomFailure, error) {
	outcomes := make(chan deliveryOutcome, 8)
	var workers sync.WaitGroup
	storageTargets := make([]contracts.RoomTransferDestination, 0)
	agentTargets := make(map[string]contracts.RoomTransferDestination)
	for _, target := range active.transfer.Targets {
		if targetDelivered(*resume, target, index) {
			continue
		}
		if target.Lease.Storage != nil {
			storageTargets = append(storageTargets, target)
		} else {
			agentTargets[target.MemberID] = target
		}
	}
	// Agent receipts arrive over the session HTTP endpoint. One watcher observes
	// them together; an unavailable agent cannot consume a provider upload slot.
	if len(agentTargets) > 0 {
		workers.Add(1)
		go func() { defer workers.Done(); active.collectAgentReceipts(ctx, index, agentTargets, outcomes) }()
	}
	if len(storageTargets) > 0 {
		digest := md5.Sum(current.payload) // Provider integrity header, not authentication.
		contentMD5 := base64.StdEncoding.EncodeToString(digest[:])
		jobs := make(chan contracts.RoomTransferDestination)
		for count := 0; count < min(8, len(storageTargets)); count++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for target := range jobs {
					result, err := writeStorageChunk(ctx, h.storageClient, active.transfer, workerID, target, index, current.payload, current.source.RangeSHA256, contentMD5, h.now)
					outcomes <- deliveryOutcome{target: target, storage: result, err: err}
				}
			}()
		}
		go func() {
			defer close(jobs)
			for _, target := range storageTargets {
				jobs <- target
			}
		}()
	}
	go func() { workers.Wait(); close(outcomes) }()
	var failures []contracts.RoomFailure
	var checkpointErr error
	for outcome := range outcomes {
		target := outcome.target
		if outcome.err != nil {
			origin, code := "target_agent", "target_write_failed"
			if target.Lease.Storage != nil {
				origin, code = "storage_provider", storageFailureCode(outcome.err)
			}
			failures = append(failures, contracts.RoomFailure{Origin: origin, Code: code, Retryable: !errors.Is(outcome.err, context.Canceled),
				TargetMemberID: target.MemberID, ChunkIndices: []int64{index}})
			continue
		}
		key := receiptKey(target.MemberID, index)
		if target.Lease.Storage != nil {
			resume.StorageResults[storageResultKey(outcome.storage)] = outcome.storage
		} else {
			resume.TargetReceipts[key] = outcome.agent.RangeReceipt
			if outcome.agent.FinalReceipt != nil {
				resume.FinalReceipts[target.MemberID] = *outcome.agent.FinalReceipt
			}
		}
		if err := saveCheckpoint(ctx, active.transfer.SchemaVersion, *resume, key); err != nil {
			checkpointErr = err
		}
		workloadprogress.Report(ctx, map[string]string{"phase": "delivered", "lane_id": active.transfer.LaneID,
			"target_member_id": target.MemberID, "chunk_index": strconv.FormatInt(index, 10)})
	}
	return failures, checkpointErr
}

func targetDelivered(resume checkpointValue, target contracts.RoomTransferDestination, index int64) bool {
	if target.Lease.Storage != nil {
		_, ok := resume.StorageResults[contracts.TunnelLeaseRoleTargetWrite+":"+receiptKey(target.MemberID, index)]
		return ok
	}
	_, ok := resume.TargetReceipts[receiptKey(target.MemberID, index)]
	return ok
}

func allTargetsDelivered(resume checkpointValue, targets []contracts.RoomTransferDestination, index int64) bool {
	for _, target := range targets {
		if !targetDelivered(resume, target, index) {
			return false
		}
	}
	return len(targets) > 0
}

func (s *session) restoreCompleted(resume checkpointValue) {
	for index := s.transfer.ChunkStart; index <= s.transfer.ChunkEnd; index++ {
		if !allTargetsDelivered(resume, s.transfer.Targets, index) {
			continue
		}
		done := &completedChunk{source: resume.SourceReceipts[index], targets: map[string]contracts.TargetRangeReceipt{}, finals: map[string]contracts.FinalTargetReceipt{}}
		for _, target := range s.transfer.Targets {
			if target.Lease.Storage == nil {
				done.targets[target.MemberID] = resume.TargetReceipts[receiptKey(target.MemberID, index)]
				if final, ok := resume.FinalReceipts[target.MemberID]; ok {
					done.finals[target.MemberID] = final
				}
			}
		}
		s.completed[index] = done
	}
	for s.completed[s.expected] != nil {
		s.expected++
	}
}

func storageResultKey(result contracts.StorageRangeResult) string {
	return result.Role + ":" + receiptKey(result.MemberID, result.ChunkIndex)
}

func storageFailureCode(err error) string {
	if err.Error() == "room_storage_source_mutated" {
		return "room_storage_source_mutated"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "storage_endpoint_failed"
}

func targetLeases(targets []contracts.RoomTransferDestination) []contracts.TunnelLease {
	leases := make([]contracts.TunnelLease, 0, len(targets))
	for _, target := range targets {
		leases = append(leases, target.Lease)
	}
	return leases
}

// A single waiter avoids losing coalesced session notifications to unrelated
// target waiters. It holds no payload copy and never starts a goroutine per member.
func (s *session) collectAgentReceipts(ctx context.Context, index int64, pending map[string]contracts.RoomTransferDestination, outcomes chan<- deliveryOutcome) {
	for len(pending) > 0 {
		s.mu.Lock()
		ready := make([]deliveryOutcome, 0)
		if current := s.chunks[index]; current != nil {
			for memberID, target := range pending {
				if receipt, ok := current.targets[memberID]; ok {
					value := targetResponse{RangeReceipt: receipt}
					if final, ok := current.finals[memberID]; ok {
						copy := final
						value.FinalReceipt = &copy
					}
					ready = append(ready, deliveryOutcome{target: target, agent: value})
					delete(pending, memberID)
				}
			}
		}
		s.mu.Unlock()
		for _, result := range ready {
			outcomes <- result
		}
		if len(pending) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			for _, target := range pending {
				outcomes <- deliveryOutcome{target: target, err: context.Cause(ctx)}
			}
			return
		case <-s.changed:
		}
	}
}
