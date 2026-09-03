package roomtransfer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"time"

	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

const (
	maxReceiptBytes   = 64 << 10
	maxTargets        = 10_000
	maxChunkBytes     = 64 << 20
	maxParallelFanout = 32
)

var (
	errSourceDisconnected = errors.New("source disconnected")
	errSourceCapacity     = errors.New("source capacity exhausted")
	errSourceLease        = errors.New("source lease rejected")
	errSourceFileMutated  = errors.New("source file mutated")
	errSourceIntegrity    = errors.New("source range integrity failed")
)

type sourceFailureError struct {
	kind    error
	receipt contracts.SourceFailureReceipt
}

func (failure *sourceFailureError) Error() string { return failure.kind.Error() }
func (failure *sourceFailureError) Unwrap() error { return failure.kind }

type checkpointValue struct {
	BatchID        string                                  `json:"batch_id"`
	TransferID     string                                  `json:"transfer_id"`
	LaneID         string                                  `json:"lane_id"`
	SourceReceipts map[int64]contracts.SourceRangeReceipt  `json:"source_receipts"`
	TargetReceipts map[string]contracts.TargetRangeReceipt `json:"target_receipts"`
	FinalReceipts  map[string]contracts.FinalTargetReceipt `json:"final_receipts"`
}

type deliveryResponse struct {
	RangeReceipt contracts.TargetRangeReceipt  `json:"range_receipt"`
	FinalReceipt *contracts.FinalTargetReceipt `json:"final_receipt,omitempty"`
}

type Handler struct {
	client *http.Client
	now    func() time.Time
}

func NewHandler(client *http.Client) *Handler {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	return &Handler{client: client, now: time.Now}
}

func (h *Handler) Kind() domain.Kind { return domain.KindRoomTransfer }

func (h *Handler) Validate(spec domain.Spec) error {
	var transfer contracts.RoomTransfer
	if err := json.Unmarshal(spec.Payload, &transfer); err != nil {
		return fmt.Errorf("decode room transfer: %w", err)
	}
	if transfer.SchemaVersion != contracts.RoomTransferSchemaVersion || transfer.BatchID == "" || transfer.RoomID == "" ||
		transfer.ChannelID == "" || transfer.PublicationID == "" || transfer.TransferID == "" || transfer.LaneID == "" ||
		transfer.SnapshotVersion == 0 || transfer.Attempt <= 0 {
		return errors.New("room transfer identity or schema is invalid")
	}
	if transfer.FileSizeBytes <= 0 || transfer.ChunkSizeBytes <= 0 || transfer.ChunkSizeBytes > maxChunkBytes ||
		transfer.ChunkCount != (transfer.FileSizeBytes+transfer.ChunkSizeBytes-1)/transfer.ChunkSizeBytes ||
		transfer.ChunkStart < 0 || transfer.ChunkEnd < transfer.ChunkStart || transfer.ChunkEnd >= transfer.ChunkCount {
		return errors.New("room transfer file or lane range is invalid")
	}
	if len(transfer.Targets) == 0 || len(transfer.Targets) > maxTargets {
		return errors.New("room transfer target count is invalid")
	}
	now := h.now().UTC()
	if err := transfer.SourceLease.Validate(contracts.TunnelLeaseRoleSourceRead, "", now); err != nil {
		return fmt.Errorf("source lease: %w", err)
	}
	seen := make(map[string]struct{}, len(transfer.Targets))
	for _, target := range transfer.Targets {
		if target.MemberID == "" {
			return errors.New("room transfer target member is required")
		}
		if _, duplicate := seen[target.MemberID]; duplicate {
			return fmt.Errorf("duplicate room target %s", target.MemberID)
		}
		seen[target.MemberID] = struct{}{}
		if err := target.Lease.Validate(contracts.TunnelLeaseRoleTargetWrite, target.MemberID, now); err != nil {
			return fmt.Errorf("target %s lease: %w", target.MemberID, err)
		}
	}
	return nil
}

func (h *Handler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	var transfer contracts.RoomTransfer
	if err := json.Unmarshal(spec.Payload, &transfer); err != nil {
		return domain.Result{}, err
	}
	resume := checkpointValue{BatchID: transfer.BatchID, TransferID: transfer.TransferID, LaneID: transfer.LaneID,
		SourceReceipts: make(map[int64]contracts.SourceRangeReceipt), TargetReceipts: make(map[string]contracts.TargetRangeReceipt),
		FinalReceipts: make(map[string]contracts.FinalTargetReceipt)}
	if _, ok, err := workloadcheckpoint.Current(ctx, contracts.RoomTransferSchemaVersion, &resume); err != nil {
		return domain.Result{}, err
	} else if ok && (resume.BatchID != transfer.BatchID || resume.TransferID != transfer.TransferID || resume.LaneID != transfer.LaneID) {
		return domain.Result{}, errors.New("room transfer checkpoint belongs to another assignment")
	}
	initializeCheckpoint(&resume)
	if err := h.verifyCheckpoint(transfer, resume); err != nil {
		return domain.Result{}, err
	}

	var bytesProcessed int64
	failures := make(map[string]contracts.RoomFailure)
	for chunkIndex := transfer.ChunkStart; chunkIndex <= transfer.ChunkEnd; chunkIndex++ {
		pending := pendingTargets(transfer.Targets, resume.TargetReceipts, chunkIndex)
		if len(pending) == 0 {
			continue
		}
		offset, length := contracts.ChunkRange(transfer.FileSizeBytes, transfer.ChunkSizeBytes, chunkIndex)
		workloadprogress.Report(ctx, map[string]string{"phase": "reading", "lane_id": transfer.LaneID,
			"chunk_index": fmt.Sprint(chunkIndex)})
		payload, sourceReceipt, err := h.readRange(ctx, transfer, chunkIndex, offset, length)
		if err != nil {
			origin, code, retryable := "source_agent", "source_disconnected", true
			var signedFailure *sourceFailureError
			if errors.As(err, &signedFailure) {
				code, retryable = signedFailure.receipt.Code, false
			} else if errors.Is(err, errSourceFileMutated) || errors.Is(err, errSourceIntegrity) {
				code = "source_integrity_evidence_invalid"
			} else if errors.Is(err, errSourceCapacity) {
				code = "source_capacity_exhausted"
			} else if errors.Is(err, errSourceLease) {
				origin, code = "room_coordinator", "source_lease_rejected"
			}
			failure := contracts.RoomFailure{Origin: origin, Code: code, Retryable: retryable,
				ChunkIndices: []int64{chunkIndex}, Detail: err.Error()}
			if signedFailure != nil {
				failure.SourceFailureReceipt = &signedFailure.receipt
			}
			encoded, _ := json.Marshal([]contracts.RoomFailure{failure})
			return domain.Result{BytesProcessed: bytesProcessed, Outputs: map[string]string{"room_failures": string(encoded)}}, err
		}
		resume.SourceReceipts[chunkIndex] = sourceReceipt
		bytesProcessed += int64(len(payload))
		outcomes := h.fanOut(ctx, transfer, pending, chunkIndex, offset, payload, sourceReceipt.RangeSHA256)
		for _, outcome := range outcomes {
			key := receiptKey(outcome.memberID, chunkIndex)
			if outcome.err != nil {
				failures[key] = contracts.RoomFailure{Origin: "target_agent", Code: "target_write_failed", Retryable: true,
					TargetMemberID: outcome.memberID, ChunkIndices: []int64{chunkIndex}, Detail: outcome.err.Error()}
				continue
			}
			resume.TargetReceipts[key] = outcome.response.RangeReceipt
			if outcome.response.FinalReceipt != nil {
				resume.FinalReceipts[outcome.memberID] = *outcome.response.FinalReceipt
			}
			if err := saveCheckpoint(ctx, resume, key); err != nil {
				return domain.Result{BytesProcessed: bytesProcessed}, err
			}
			workloadprogress.Report(ctx, map[string]string{"phase": "delivered", "lane_id": transfer.LaneID,
				"target_member_id": outcome.memberID, "chunk_index": fmt.Sprint(chunkIndex)})
		}
	}
	return h.result(transfer, resume, failures, bytesProcessed)
}

func initializeCheckpoint(value *checkpointValue) {
	if value.SourceReceipts == nil {
		value.SourceReceipts = make(map[int64]contracts.SourceRangeReceipt)
	}
	if value.TargetReceipts == nil {
		value.TargetReceipts = make(map[string]contracts.TargetRangeReceipt)
	}
	if value.FinalReceipts == nil {
		value.FinalReceipts = make(map[string]contracts.FinalTargetReceipt)
	}
}

func (h *Handler) verifyCheckpoint(transfer contracts.RoomTransfer, resume checkpointValue) error {
	now := h.now().UTC()
	targets := make(map[string]contracts.RoomTransferDestination, len(transfer.Targets))
	for _, target := range transfer.Targets {
		targets[target.MemberID] = target
	}
	for key, receipt := range resume.TargetReceipts {
		target, ok := targets[receipt.TargetMemberID]
		if !ok || key != receiptKey(receipt.TargetMemberID, receipt.ChunkIndex) {
			return errors.New("room transfer checkpoint contains an unknown target range")
		}
		if err := verifyTargetBinding(transfer, target, receipt, now); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) readRange(ctx context.Context, transfer contracts.RoomTransfer, chunkIndex, offset, length int64) ([]byte, contracts.SourceRangeReceipt, error) {
	var failures []error
	for _, endpoint := range transfer.SourceLease.Endpoints {
		method := endpoint.Method
		if method == "" {
			method = http.MethodGet
		}
		request, err := http.NewRequestWithContext(ctx, method, endpoint.URL, nil)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		copyHeaders(request.Header, endpoint.Headers)
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
		request.Header.Set("X-Beam-Transfer-ID", transfer.TransferID)
		request.Header.Set("X-Beam-Lane-ID", transfer.LaneID)
		request.Header.Set("X-Beam-Chunk-Index", fmt.Sprint(chunkIndex))
		request.Header.Set("X-Beam-Lease-ID", transfer.SourceLease.LeaseID)
		response, err := h.client.Do(request)
		if err != nil {
			failures = append(failures, fmt.Errorf("%w: %v", errSourceDisconnected, err))
			continue
		}
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, length+1))
		response.Body.Close()
		failureReceipt, failureReceiptErr := h.sourceFailureReceipt(transfer, chunkIndex, response)
		if response.StatusCode == http.StatusConflict {
			if failureReceiptErr == nil && failureReceipt.Code == "source_file_mutated" {
				failures = append(failures, &sourceFailureError{kind: errSourceFileMutated, receipt: failureReceipt})
			} else {
				failures = append(failures, fmt.Errorf("%w: unsigned mutation response", errSourceIntegrity))
			}
			continue
		}
		if response.StatusCode == http.StatusTooManyRequests {
			failures = append(failures, errSourceCapacity)
			continue
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			failures = append(failures, fmt.Errorf("%w: status=%d", errSourceLease, response.StatusCode))
			continue
		}
		if readErr != nil || (response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent) || int64(len(payload)) != length {
			if failureReceiptErr == nil {
				kind := errSourceIntegrity
				if failureReceipt.Code == "source_file_mutated" {
					kind = errSourceFileMutated
				}
				failures = append(failures, &sourceFailureError{kind: kind, receipt: failureReceipt})
				continue
			}
			kind := errSourceIntegrity
			if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusRequestTimeout ||
				response.StatusCode == http.StatusTooEarly || response.StatusCode >= 500 {
				kind = errSourceDisconnected
			}
			failures = append(failures, fmt.Errorf("%w: status=%d bytes=%d", kind, response.StatusCode, len(payload)))
			continue
		}
		receiptValue := response.Header.Get("X-Beam-Source-Receipt")
		if receiptValue == "" {
			receiptValue = response.Trailer.Get("X-Beam-Source-Receipt")
		}
		receipt, err := decodeSourceReceipt(receiptValue)
		if err != nil {
			failures = append(failures, fmt.Errorf("%w: %v", errSourceIntegrity, err))
			continue
		}
		digest := sha256.Sum256(payload)
		if receipt.TransferID != transfer.TransferID || receipt.LaneID != transfer.LaneID || receipt.ChunkIndex != chunkIndex ||
			receipt.Offset != offset || receipt.Length != length || receipt.LeaseID != transfer.SourceLease.LeaseID ||
			receipt.RangeSHA256 != hex.EncodeToString(digest[:]) || receipt.CompletedAt.After(transfer.SourceLease.ExpiresAt) ||
			receipt.Verify(transfer.SourceLease.AgentPublicKey, h.now().UTC()) != nil {
			failures = append(failures, fmt.Errorf("%w: receipt does not match streamed bytes", errSourceIntegrity))
			continue
		}
		return payload, receipt, nil
	}
	return nil, contracts.SourceRangeReceipt{}, fmt.Errorf("all source lease endpoints failed: %w", errors.Join(failures...))
}

func (h *Handler) sourceFailureReceipt(transfer contracts.RoomTransfer, chunkIndex int64,
	response *http.Response) (contracts.SourceFailureReceipt, error) {
	value := response.Header.Get("X-Beam-Source-Failure")
	if value == "" {
		value = response.Trailer.Get("X-Beam-Source-Failure")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxReceiptBytes {
		return contracts.SourceFailureReceipt{}, errors.New("source endpoint omitted signed failure evidence")
	}
	var receipt contracts.SourceFailureReceipt
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		return contracts.SourceFailureReceipt{}, err
	}
	if receipt.TransferID != transfer.TransferID || receipt.LaneID != transfer.LaneID || receipt.ChunkIndex != chunkIndex ||
		receipt.LeaseID != transfer.SourceLease.LeaseID || receipt.Verify(transfer.SourceLease.AgentPublicKey, h.now().UTC()) != nil {
		return contracts.SourceFailureReceipt{}, errors.New("source failure evidence does not match the assigned range")
	}
	return receipt, nil
}

func (h *Handler) result(transfer contracts.RoomTransfer, resume checkpointValue, failures map[string]contracts.RoomFailure,
	bytesProcessed int64) (domain.Result, error) {
	sourceReceipts := make([]contracts.SourceRangeReceipt, 0, len(resume.SourceReceipts))
	for _, receipt := range resume.SourceReceipts {
		sourceReceipts = append(sourceReceipts, receipt)
	}
	sort.Slice(sourceReceipts, func(i, j int) bool { return sourceReceipts[i].ChunkIndex < sourceReceipts[j].ChunkIndex })
	targetReceipts := make([]contracts.TargetRangeReceipt, 0, len(resume.TargetReceipts))
	for _, receipt := range resume.TargetReceipts {
		targetReceipts = append(targetReceipts, receipt)
	}
	sort.Slice(targetReceipts, func(i, j int) bool {
		if targetReceipts[i].TargetMemberID != targetReceipts[j].TargetMemberID {
			return targetReceipts[i].TargetMemberID < targetReceipts[j].TargetMemberID
		}
		return targetReceipts[i].ChunkIndex < targetReceipts[j].ChunkIndex
	})
	finalReceipts := make([]contracts.FinalTargetReceipt, 0, len(resume.FinalReceipts))
	missing := make([]contracts.RoomMissingCells, 0)
	for _, target := range transfer.Targets {
		if receipt, ok := resume.FinalReceipts[target.MemberID]; ok {
			finalReceipts = append(finalReceipts, receipt)
		}
		entry := contracts.RoomMissingCells{TargetMemberID: target.MemberID}
		for chunk := transfer.ChunkStart; chunk <= transfer.ChunkEnd; chunk++ {
			if _, ok := resume.TargetReceipts[receiptKey(target.MemberID, chunk)]; !ok {
				entry.ChunkIndices = append(entry.ChunkIndices, chunk)
			}
		}
		if len(entry.ChunkIndices) > 0 {
			missing = append(missing, entry)
		}
	}
	failureList := make([]contracts.RoomFailure, 0, len(failures))
	for _, failure := range failures {
		failureList = append(failureList, failure)
	}
	outputs := map[string]string{}
	for key, value := range map[string]any{"source_receipts": sourceReceipts, "target_receipts": targetReceipts,
		"final_target_receipts": finalReceipts, "missing": missing, "room_failures": failureList} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return domain.Result{}, err
		}
		outputs[key] = string(encoded)
	}
	return domain.Result{BytesProcessed: bytesProcessed, Outputs: outputs}, nil
}

func saveCheckpoint(ctx context.Context, value checkpointValue, lastRange string) error {
	err := workloadcheckpoint.Save(ctx, contracts.RoomTransferSchemaVersion, map[string]string{
		"batch_id": value.BatchID, "transfer_id": value.TransferID, "lane_id": value.LaneID,
		"last_range": lastRange, "delivered_ranges": fmt.Sprint(len(value.TargetReceipts)),
	}, value)
	if errors.Is(err, workloadcheckpoint.ErrUnavailable) {
		return nil
	}
	return err
}

func receiptKey(memberID string, chunkIndex int64) string {
	return fmt.Sprintf("%s:%d", memberID, chunkIndex)
}

func copyHeaders(destination http.Header, source map[string]string) {
	for key, value := range source {
		if slices.Contains([]string{"Host", "Content-Length", "Transfer-Encoding"}, http.CanonicalHeaderKey(key)) {
			continue
		}
		destination.Set(key, value)
	}
}
