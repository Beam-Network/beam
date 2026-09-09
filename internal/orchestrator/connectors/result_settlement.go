package connectors

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type beamCoreResultEvidence struct {
	chunkHash string
	etag      string
}

type taskResultAcknowledgement struct {
	Received bool   `json:"received"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
}

func resultEvidence(record dispatch.Record, result domain.Result) (beamCoreResultEvidence, error) {
	var transfer contracts.MultipartTransfer
	if err := json.Unmarshal(record.Spec.Payload, &transfer); err != nil {
		return beamCoreResultEvidence{}, dispatch.TerminalDelivery(fmt.Errorf("BeamCore task result has invalid transfer payload: %w", err))
	}
	if len(transfer.Parts) != 1 {
		return beamCoreResultEvidence{}, dispatch.TerminalDelivery(
			fmt.Errorf("BeamCore scalar task result requires exactly one transfer part; got %d", len(transfer.Parts)),
		)
	}
	partPrefix := fmt.Sprintf("part.%d.", transfer.Parts[0].Index)
	evidence := beamCoreResultEvidence{
		chunkHash: strings.TrimSpace(result.Outputs[partPrefix+"sha256"]),
		etag:      strings.TrimSpace(result.Outputs[partPrefix+"etag"]),
	}
	if result.State != domain.StateCompleted && result.State != domain.StateReceiptCommitted {
		return evidence, nil
	}
	for _, commitment := range record.Spec.Evidence.Commitments {
		switch strings.ToLower(strings.TrimSpace(commitment)) {
		case "sha256":
			if evidence.chunkHash == "" {
				return beamCoreResultEvidence{}, dispatch.TerminalDelivery(
					fmt.Errorf("BeamCore task result is missing part %d sha256 evidence", transfer.Parts[0].Index),
				)
			}
		case "etag":
			if evidence.etag == "" {
				return beamCoreResultEvidence{}, dispatch.TerminalDelivery(
					fmt.Errorf("BeamCore task result is missing part %d etag evidence", transfer.Parts[0].Index),
				)
			}
		}
	}
	return evidence, nil
}

func taskResultDisposition(ack taskResultAcknowledgement) error {
	status := strings.TrimSpace(ack.Status)
	reason := fallback(ack.Reason, "no reason supplied")
	switch status {
	case "completed", "owned_processing":
		if !ack.Received {
			return fmt.Errorf("BeamCore returned inconsistent task_result_ack status %s with received=false", status)
		}
		return nil
	case "retry":
		return fmt.Errorf("BeamCore requested task_result retry: %s", reason)
	case "failed", "rejected", "late_expired", "late_superseded":
		return dispatch.TerminalDelivery(fmt.Errorf("BeamCore returned terminal task_result status %s: %s", status, reason))
	case "":
		return errors.New("BeamCore task_result acknowledgement is missing status")
	default:
		return fmt.Errorf("BeamCore task_result acknowledgement has unknown status %q", status)
	}
}
