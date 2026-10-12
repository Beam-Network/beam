package connectors

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type taskOfferResultAcknowledgement struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func taskOfferResult(record dispatch.Record, result domain.Result) (map[string]any, error) {
	var transfer contracts.MultipartTransfer
	if err := json.Unmarshal(record.Spec.Payload, &transfer); err != nil {
		return nil, dispatch.TerminalDelivery(fmt.Errorf("BeamCore task result has invalid transfer payload: %w", err))
	}
	var chunkHash string
	var destinations []map[string]any
	var err error
	if transfer.Fanout {
		chunkHash, destinations, err = fanoutDestinationOutcomes(result, transfer)
	} else {
		chunkHash, destinations, err = scalarDestinationOutcome(record, result, transfer)
	}
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"offer_id": record.Spec.AttemptID, "worker_id": record.WorkerID, "destinations": destinations}
	if chunkHash != "" {
		payload["chunk_hash"] = chunkHash
	}
	return payload, nil
}

func scalarDestinationOutcome(record dispatch.Record, result domain.Result, transfer contracts.MultipartTransfer) (string, []map[string]any, error) {
	if len(transfer.Parts) != 1 {
		return "", nil, dispatch.TerminalDelivery(
			fmt.Errorf("BeamCore single-destination result requires exactly one transfer part; got %d", len(transfer.Parts)),
		)
	}
	part := transfer.Parts[0]
	prefix := fmt.Sprintf("part.%d.", part.Index)
	hash := strings.TrimSpace(result.Outputs[prefix+"sha256"])
	etag := strings.TrimSpace(result.Outputs[prefix+"etag"])
	completed := result.State == domain.StateCompleted || result.State == domain.StateReceiptCommitted
	if part.Authenticated() {
		failure := fallback(result.Outputs[prefix+"error"], fallback(result.ErrorMessage, "worker_failed"))
		outcome, err := authenticatedDestinationOutcome(result.Outputs, prefix, completed, failure)
		if err != nil {
			return "", nil, err
		}
		if completed && len(hash) != 64 {
			return "", nil, dispatch.TerminalDelivery(fmt.Errorf("BeamCore task result is missing part %d sha256 evidence", part.Index))
		}
		return hash, []map[string]any{outcome}, nil
	}
	if !completed {
		failure := map[string]any{"error": fallback(result.ErrorMessage, "worker_failed")}
		if etag != "" {
			failure["etag"] = etag
		}
		return hash, []map[string]any{failure}, nil
	}
	for _, commitment := range record.Spec.Evidence.Commitments {
		switch strings.ToLower(strings.TrimSpace(commitment)) {
		case "sha256":
			if hash == "" {
				return "", nil, dispatch.TerminalDelivery(fmt.Errorf("BeamCore task result is missing part %d sha256 evidence", part.Index))
			}
		case "etag":
			if etag == "" {
				return "", nil, dispatch.TerminalDelivery(fmt.Errorf("BeamCore task result is missing part %d etag evidence", part.Index))
			}
		}
	}
	return hash, []map[string]any{destinationSuccess(etag)}, nil
}

func fanoutDestinationOutcomes(result domain.Result, transfer contracts.MultipartTransfer) (string, []map[string]any, error) {
	if len(transfer.Parts) == 0 {
		return "", nil, dispatch.TerminalDelivery(errors.New("empty fan-out result"))
	}
	var chunkHash string
	destinations := make([]map[string]any, 0, len(transfer.Parts))
	for _, part := range transfer.Parts {
		prefix := fmt.Sprintf("part.%d.", part.Index)
		completed := result.Outputs[prefix+"state"] == "completed"
		length, _ := strconv.ParseInt(result.Outputs[prefix+"bytes"], 10, 64)
		hash, etag := result.Outputs[prefix+"sha256"], result.Outputs[prefix+"etag"]
		if completed && (length != part.Length || len(hash) != 64 || (part.ETagRequired && etag == "")) {
			return "", nil, dispatch.TerminalDelivery(errors.New("fan-out result lacks required delivery evidence"))
		}
		failure := result.Outputs[prefix+"error"]
		if failure == "" {
			failure = "fanout_worker_failed"
			if result.ErrorMessage == "room_source_changed" {
				failure = "room_source_changed"
			}
		}
		if completed {
			chunkHash = hash
		}
		if part.Authenticated() {
			outcome, err := authenticatedDestinationOutcome(result.Outputs, prefix, completed, failure)
			if err != nil {
				return "", nil, err
			}
			destinations = append(destinations, outcome)
			continue
		}
		if completed {
			destinations = append(destinations, destinationSuccess(etag))
			continue
		}
		destinations = append(destinations, map[string]any{"error": failure})
	}
	return chunkHash, destinations, nil
}

// authenticatedDestinationOutcome reports the sha1 of the bytes sent, the provider
// request count and, on success, the provider part id. A success needs all three.
// A throttled failure also reports the provider's Retry-After in milliseconds.
func authenticatedDestinationOutcome(outputs map[string]string, prefix string, completed bool, failure string) (map[string]any, error) {
	sha1 := outputs[prefix+"sha1"]
	requests, countErr := strconv.Atoi(outputs[prefix+"request_count"])
	if completed {
		partID := outputs[prefix+"provider_part_id"]
		if !lowerHex(sha1, 40) || countErr != nil || requests < 1 || partID == "" {
			return nil, dispatch.TerminalDelivery(errors.New("authenticated destination result lacks sha1, request_count or provider_part_id evidence"))
		}
		return map[string]any{"sha1": sha1, "request_count": requests, "provider_part_id": partID}, nil
	}
	outcome := map[string]any{"error": failure}
	if lowerHex(sha1, 40) {
		outcome["sha1"] = sha1
	}
	if countErr == nil && requests >= 0 {
		outcome["request_count"] = requests
	}
	if retryAfter, err := strconv.ParseInt(outputs[prefix+"retry_after_ms"], 10, 64); err == nil &&
		retryAfter >= 0 && retryAfter <= maxRetryAfterMillis {
		outcome["retry_after_ms"] = retryAfter
	}
	return outcome, nil
}

// maxRetryAfterMillis is the longest provider Retry-After BeamCore accepts in a result.
const maxRetryAfterMillis = 3_600_000

func lowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func destinationSuccess(etag string) map[string]any {
	if etag == "" {
		return map[string]any{}
	}
	return map[string]any{"etag": etag}
}

func taskOfferResultDisposition(ack taskOfferResultAcknowledgement) error {
	status := strings.TrimSpace(ack.Status)
	reason := fallback(ack.Reason, "no reason supplied")
	switch status {
	case "accepted":
		return nil
	case "retry":
		return fmt.Errorf("BeamCore requested task_offer_result retry: %s", reason)
	case "rejected":
		return dispatch.TerminalDelivery(fmt.Errorf("BeamCore rejected task_offer_result: %s", reason))
	case "":
		return errors.New("BeamCore task_offer_result acknowledgement is missing status")
	default:
		return fmt.Errorf("BeamCore task_offer_result acknowledgement has unknown status %q", status)
	}
}
