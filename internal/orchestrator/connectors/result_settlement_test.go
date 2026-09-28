package connectors

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestTaskOfferResultUsesTheWorkloadPartIndex(t *testing.T) {
	payload, err := json.Marshal(contracts.MultipartTransfer{Parts: []contracts.TransferPart{{Index: 7}}})
	if err != nil {
		t.Fatal(err)
	}
	record := dispatch.Record{WorkerID: "worker-1", Spec: domain.Spec{AttemptID: "offer-1", Payload: payload,
		Evidence: domain.EvidencePolicy{Commitments: []string{"sha256", "etag"}}}}
	result := domain.Result{State: domain.StateCompleted, Outputs: map[string]string{
		"sha256": "wrong-top-level-hash", "part.0.sha256": "wrong-part-hash",
		"part.7.sha256": "expected-hash", "part.7.etag": "expected-etag",
	}}
	built, err := taskOfferResult(record, result)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(built)
	if string(encoded) != `{"chunk_hash":"expected-hash","destinations":[{"etag":"expected-etag"}],"offer_id":"offer-1","worker_id":"worker-1"}` {
		t.Fatalf("unexpected task_offer_result: %s", encoded)
	}
}

func TestTaskOfferResultRejectsMissingAndMultiPartEvidence(t *testing.T) {
	for _, parts := range [][]contracts.TransferPart{{{Index: 3}}, {{Index: 0}, {Index: 1}}} {
		payload, err := json.Marshal(contracts.MultipartTransfer{Parts: parts})
		if err != nil {
			t.Fatal(err)
		}
		_, err = taskOfferResult(dispatch.Record{Spec: domain.Spec{Payload: payload,
			Evidence: domain.EvidencePolicy{Commitments: []string{"sha256", "etag"}}}},
			domain.Result{State: domain.StateCompleted, Outputs: map[string]string{"part.3.sha256": "hash"}})
		var terminal *dispatch.TerminalDeliveryError
		if !errors.As(err, &terminal) {
			t.Fatalf("expected terminal evidence error, got %v", err)
		}
	}
}

func TestTaskOfferResultAlignsFanoutDestinations(t *testing.T) {
	payload, err := json.Marshal(contracts.MultipartTransfer{Fanout: true, Parts: []contracts.TransferPart{
		{Index: 0, Length: 8, ETagRequired: true}, {Index: 1, Length: 8},
	}})
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	built, err := taskOfferResult(dispatch.Record{WorkerID: "worker-1", Spec: domain.Spec{AttemptID: "offer-2", Payload: payload}},
		domain.Result{State: domain.StateFailed, Outputs: map[string]string{
			"part.0.state": "completed", "part.0.bytes": "8", "part.0.sha256": hash, "part.0.etag": "verified",
			"part.1.state": "failed", "part.1.error": "destination_http_403",
		}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(built)
	want := `{"chunk_hash":"` + hash + `","destinations":[{"etag":"verified"},{"error":"destination_http_403"}],"offer_id":"offer-2","worker_id":"worker-1"}`
	if string(encoded) != want {
		t.Fatalf("unexpected fan-out result: %s", encoded)
	}
}

func TestTaskOfferResultDispositionStatuses(t *testing.T) {
	if err := taskOfferResultDisposition(taskOfferResultAcknowledgement{Status: "accepted"}); err != nil {
		t.Fatalf("accepted returned %v", err)
	}
	if err := taskOfferResultDisposition(taskOfferResultAcknowledgement{Status: "retry"}); err == nil {
		t.Fatal("retry status was accepted")
	}
	err := taskOfferResultDisposition(taskOfferResultAcknowledgement{Status: "rejected"})
	var terminal *dispatch.TerminalDeliveryError
	if !errors.As(err, &terminal) {
		t.Fatalf("rejected was not terminal: %v", err)
	}
	for _, status := range []string{"", "completed"} {
		if err := taskOfferResultDisposition(taskOfferResultAcknowledgement{Status: status}); err == nil {
			t.Fatalf("status %q was accepted", status)
		}
	}
}
