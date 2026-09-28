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
	if result.State != domain.StateCompleted && result.State != domain.StateReceiptCommitted {
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
		if completed {
			chunkHash = hash
			destinations = append(destinations, destinationSuccess(etag))
			continue
		}
		failure := result.Outputs[prefix+"error"]
		if failure == "" {
			failure = "fanout_worker_failed"
			if result.ErrorMessage == "room_source_changed" {
				failure = "room_source_changed"
			}
		}
		destinations = append(destinations, map[string]any{"error": failure})
	}
	return chunkHash, destinations, nil
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
