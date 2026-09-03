package roomtransfer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

type deliveryOutcome struct {
	memberID string
	response deliveryResponse
	err      error
}

func (h *Handler) fanOut(ctx context.Context, transfer contracts.RoomTransfer, targets []contracts.RoomTransferDestination,
	chunkIndex, offset int64, payload []byte, rangeHash string) []deliveryOutcome {
	outcomes := make(chan deliveryOutcome, len(targets))
	semaphore := make(chan struct{}, min(maxParallelFanout, len(targets)))
	var group sync.WaitGroup
	for _, target := range targets {
		target := target
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				outcomes <- deliveryOutcome{memberID: target.MemberID, err: context.Cause(ctx)}
				return
			}
			response, err := h.deliver(ctx, transfer, target, chunkIndex, offset, payload, rangeHash)
			outcomes <- deliveryOutcome{memberID: target.MemberID, response: response, err: err}
		}()
	}
	group.Wait()
	close(outcomes)
	result := make([]deliveryOutcome, 0, len(targets))
	for outcome := range outcomes {
		result = append(result, outcome)
	}
	return result
}

func (h *Handler) deliver(ctx context.Context, transfer contracts.RoomTransfer, target contracts.RoomTransferDestination,
	chunkIndex, offset int64, payload []byte, rangeHash string) (deliveryResponse, error) {
	var failures []error
	for _, endpoint := range target.Lease.Endpoints {
		method := endpoint.Method
		if method == "" {
			method = http.MethodPut
		}
		request, err := http.NewRequestWithContext(ctx, method, endpoint.URL, bytes.NewReader(payload))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		copyHeaders(request.Header, endpoint.Headers)
		request.Header.Set("Content-Type", "application/octet-stream")
		request.Header.Set("Content-Length", fmt.Sprint(len(payload)))
		request.Header.Set("X-Beam-Room-ID", transfer.RoomID)
		request.Header.Set("X-Beam-Transfer-ID", transfer.TransferID)
		request.Header.Set("X-Beam-Lane-ID", transfer.LaneID)
		request.Header.Set("X-Beam-Chunk-Index", fmt.Sprint(chunkIndex))
		request.Header.Set("X-Beam-Range-Offset", fmt.Sprint(offset))
		request.Header.Set("X-Beam-Range-SHA256", rangeHash)
		request.Header.Set("X-Beam-Lease-ID", target.Lease.LeaseID)
		response, err := h.client.Do(request)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		encoded, readErr := io.ReadAll(io.LimitReader(response.Body, maxReceiptBytes+1))
		response.Body.Close()
		if readErr != nil || response.StatusCode < 200 || response.StatusCode >= 300 || len(encoded) > maxReceiptBytes {
			failures = append(failures, fmt.Errorf("target range response is invalid: status=%d", response.StatusCode))
			continue
		}
		var result deliveryResponse
		if err := json.Unmarshal(encoded, &result); err != nil {
			failures = append(failures, err)
			continue
		}
		if err := verifyTargetBinding(transfer, target, result.RangeReceipt, h.now().UTC()); err != nil {
			failures = append(failures, err)
			continue
		}
		if result.RangeReceipt.ChunkIndex != chunkIndex || result.RangeReceipt.Offset != offset ||
			result.RangeReceipt.Length != int64(len(payload)) || result.RangeReceipt.RangeSHA256 != rangeHash {
			failures = append(failures, errors.New("target receipt does not match the delivered range"))
			continue
		}
		if result.FinalReceipt != nil {
			if result.FinalReceipt.TransferID != transfer.TransferID || result.FinalReceipt.TargetMemberID != target.MemberID ||
				result.FinalReceipt.FileSizeBytes != transfer.FileSizeBytes || result.FinalReceipt.CompletedAt.After(target.Lease.ExpiresAt) ||
				result.FinalReceipt.Verify(target.Lease.AgentPublicKey, h.now().UTC()) != nil {
				failures = append(failures, errors.New("final target receipt does not match the transfer"))
				continue
			}
		}
		return result, nil
	}
	return deliveryResponse{}, fmt.Errorf("all target lease endpoints failed: %w", errors.Join(failures...))
}

func verifyTargetBinding(transfer contracts.RoomTransfer, target contracts.RoomTransferDestination,
	receipt contracts.TargetRangeReceipt, now time.Time) error {
	if receipt.TransferID != transfer.TransferID || receipt.LaneID != transfer.LaneID || receipt.TargetMemberID != target.MemberID ||
		receipt.LeaseID != target.Lease.LeaseID || receipt.CompletedAt.After(target.Lease.ExpiresAt) {
		return errors.New("target receipt does not match the assigned range or lease")
	}
	return receipt.Verify(target.Lease.AgentPublicKey, now)
}

func decodeSourceReceipt(value string) (contracts.SourceRangeReceipt, error) {
	encoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxReceiptBytes {
		return contracts.SourceRangeReceipt{}, errors.New("source endpoint omitted a valid signed range receipt")
	}
	var receipt contracts.SourceRangeReceipt
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		return contracts.SourceRangeReceipt{}, err
	}
	return receipt, nil
}

func pendingTargets(targets []contracts.RoomTransferDestination, completed map[string]contracts.TargetRangeReceipt,
	chunkIndex int64) []contracts.RoomTransferDestination {
	result := make([]contracts.RoomTransferDestination, 0, len(targets))
	for _, target := range targets {
		if _, ok := completed[receiptKey(target.MemberID, chunkIndex)]; !ok {
			result = append(result, target)
		}
	}
	return result
}
