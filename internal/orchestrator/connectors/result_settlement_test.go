package connectors

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestResultEvidenceUsesTheWorkloadPartIndex(t *testing.T) {
	payload, err := json.Marshal(contracts.MultipartTransfer{Parts: []contracts.TransferPart{{Index: 7}}})
	if err != nil {
		t.Fatal(err)
	}
	record := dispatch.Record{Spec: domain.Spec{Payload: payload,
		Evidence: domain.EvidencePolicy{Commitments: []string{"sha256", "etag"}}}}
	result := domain.Result{State: domain.StateCompleted, Outputs: map[string]string{
		"sha256": "wrong-top-level-hash", "part.0.sha256": "wrong-part-hash",
		"part.7.sha256": "expected-hash", "part.7.etag": "expected-etag",
	}}

	evidence, err := resultEvidence(record, result)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.chunkHash != "expected-hash" || evidence.etag != "expected-etag" {
		t.Fatalf("unexpected evidence: %+v", evidence)
	}
}

func TestResultEvidenceRejectsMissingAndMultiPartEvidence(t *testing.T) {
	for _, parts := range [][]contracts.TransferPart{{{Index: 3}}, {{Index: 0}, {Index: 1}}} {
		payload, err := json.Marshal(contracts.MultipartTransfer{Parts: parts})
		if err != nil {
			t.Fatal(err)
		}
		_, err = resultEvidence(dispatch.Record{Spec: domain.Spec{Payload: payload,
			Evidence: domain.EvidencePolicy{Commitments: []string{"sha256", "etag"}}}},
			domain.Result{State: domain.StateCompleted, Outputs: map[string]string{"part.3.sha256": "hash"}})
		var terminal *dispatch.TerminalDeliveryError
		if !errors.As(err, &terminal) {
			t.Fatalf("expected terminal evidence error, got %v", err)
		}
	}
}

func TestTaskResultDispositionStatuses(t *testing.T) {
	for _, status := range []string{"completed", "owned_processing"} {
		if err := taskResultDisposition(taskResultAcknowledgement{Received: true, Status: status}); err != nil {
			t.Fatalf("accepted status %s returned %v", status, err)
		}
	}
	if err := taskResultDisposition(taskResultAcknowledgement{Status: "retry"}); err == nil {
		t.Fatal("retry status was accepted")
	}
	for _, status := range []string{"failed", "rejected", "late_expired", "late_superseded"} {
		err := taskResultDisposition(taskResultAcknowledgement{Received: status != "rejected", Status: status})
		var terminal *dispatch.TerminalDeliveryError
		if !errors.As(err, &terminal) {
			t.Fatalf("status %s was not terminal: %v", status, err)
		}
	}
}
