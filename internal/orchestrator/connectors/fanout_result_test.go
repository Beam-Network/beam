package connectors

import (
	"strings"
	"testing"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestSourceGroupPreservesSiblingDeliveryEvidence(t *testing.T) {
	transfer := contracts.MultipartTransfer{SourceGroupID: "range", Parts: []contracts.TransferPart{{Index: 0, TaskID: "t0", OfferID: "a0", Length: 8, ETagRequired: true}, {Index: 1, TaskID: "t1", OfferID: "a1", Length: 8, ETagRequired: true}}}
	result := domain.Result{State: domain.StateFailed, Outputs: map[string]string{"part.0.state": "completed", "part.0.bytes": "8", "part.0.sha256": strings.Repeat("a", 64), "part.0.etag": "verified", "part.1.error": "destination_http_403", "source.read_count": "1", "source.payload_bytes": "8"}}
	payloads, err := sourceGroupResults(dispatch.Record{WorkerID: "worker"}, result, transfer)
	if err != nil || len(payloads) != 2 {
		t.Fatalf("result=%v err=%v", payloads, err)
	}
	if payloads[0]["success"] != true || payloads[1]["success"] != false || payloads[1]["offer_id"] != "a1" || payloads[1]["source_read"] != nil {
		t.Fatalf("payloads=%+v", payloads)
	}
	delete(result.Outputs, "part.0.etag")
	if _, err := sourceGroupResults(dispatch.Record{}, result, transfer); err == nil {
		t.Fatal("missing required provider evidence accepted")
	}
}
