package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func sourceGroupResults(record dispatch.Record, result domain.Result, transfer contracts.MultipartTransfer) ([]map[string]any, error) {
	if len(transfer.Parts) == 0 {
		return nil, dispatch.TerminalDelivery(errors.New("empty source group result"))
	}
	payloads := make([]map[string]any, 0, len(transfer.Parts))
	var acknowledged contracts.SourceGroupCheckpoint
	if result.State == domain.StateRunning && record.UpstreamCheckpoint != nil {
		if err := json.Unmarshal(record.UpstreamCheckpoint.Payload, &acknowledged); err != nil {
			return nil, err
		}
	}
	for _, part := range transfer.Parts {
		prefix := fmt.Sprintf("part.%d.", part.Index)
		if result.State == domain.StateRunning && result.Outputs[prefix+"state"] == "" {
			continue
		}
		if result.State == domain.StateRunning && acknowledged.Outputs[prefix+"state"] != "" {
			for _, field := range []string{"state", "bytes", "sha256", "etag", "error"} {
				if result.Outputs[prefix+field] != acknowledged.Outputs[prefix+field] {
					return nil, dispatch.TerminalDelivery(errors.New("checkpoint changed acknowledged destination evidence"))
				}
			}
			continue
		}
		completed := result.Outputs[prefix+"state"] == "completed"
		length, _ := strconv.ParseInt(result.Outputs[prefix+"bytes"], 10, 64)
		hash, etag := result.Outputs[prefix+"sha256"], result.Outputs[prefix+"etag"]
		if part.TaskID == "" || part.OfferID == "" || (completed && (length != part.Length || len(hash) != 64 || (part.ETagRequired && etag == ""))) {
			return nil, dispatch.TerminalDelivery(errors.New("source group result lacks required delivery evidence"))
		}
		failure := result.Outputs[prefix+"error"]
		if !completed && failure == "" {
			failure = "source_group_worker_failed"
			if result.ErrorMessage == "room_source_changed" {
				failure = "room_source_changed"
			}
		}
		payload := map[string]any{"worker_id": record.WorkerID, "task_id": part.TaskID, "offer_id": part.OfferID,
			"success": completed, "bytes_transferred": length, "chunk_hash": hash, "etag": etag, "error": failure}
		if part.Index == 0 {
			reads, _ := strconv.ParseInt(result.Outputs["source.read_count"], 10, 64)
			bytes, _ := strconv.ParseInt(result.Outputs["source.payload_bytes"], 10, 64)
			payload["source_read"] = map[string]any{"group_id": transfer.SourceGroupID, "read_count": reads, "payload_bytes": bytes}
		}
		payloads = append(payloads, payload)
	}
	return payloads, nil
}

func (control *roomControl) submitSourceGroupResult(ctx context.Context, record dispatch.Record, result domain.Result, transfer contracts.MultipartTransfer) error {
	payloads, err := sourceGroupResults(record, result, transfer)
	if err != nil {
		return err
	}
	queue := make(chan map[string]any)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var retryError, terminalError error
	for slot := 0; slot < 8; slot++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for payload := range queue {
				if err := control.request(ctx, "task_result", payload); err != nil {
					mu.Lock()
					var terminal *dispatch.TerminalDeliveryError
					if errors.As(err, &terminal) {
						terminalError = err
					} else {
						retryError = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for _, payload := range payloads {
		queue <- payload
	}
	close(queue)
	wg.Wait()
	// Accepted scalar results are idempotent. A retryable sibling must still be
	// replayed even when another destination received a terminal disposition.
	if retryError != nil {
		return retryError
	}
	return terminalError
}
