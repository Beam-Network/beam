package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	beamcoreadapter "github.com/Beam-Network/beam/internal/workload/adapters/beamcore"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func sourceDigestRecord(t *testing.T, size int64) dispatch.Record {
	t.Helper()
	payload, err := json.Marshal(contracts.SourceDigest{Size: size,
		Source: contracts.HTTPEndpoint{URL: "https://bucket.test/object?X-Amz-Signature=sig"}})
	if err != nil {
		t.Fatal(err)
	}
	return dispatch.Record{WorkerID: "worker-1", Spec: domain.Spec{AttemptID: "offer-digest",
		Kind: domain.KindTransferSourceDigest, Payload: payload,
		Evidence: domain.EvidencePolicy{Commitments: beamcoreadapter.SourceDigestEvidence}}}
}

func TestTaskOfferResultEncodesTheSourceDigestVariant(t *testing.T) {
	record := sourceDigestRecord(t, 1610612736)
	sha1, sha256 := strings.Repeat("b", 40), strings.Repeat("a", 64)
	success := domain.Result{State: domain.StateCompleted, BytesProcessed: 1610612736, Outputs: map[string]string{
		"sha1": sha1, "sha256": sha256, "bytes": "1610612736",
	}}
	want := `{"digest":{"bytes":1610612736,"sha1":"` + sha1 + `","sha256":"` + sha256 +
		`"},"offer_id":"offer-digest","worker_id":"worker-1"}`
	if got := encodedResult(t, record, success); got != want {
		t.Fatalf("unexpected digest result:\n got %s\nwant %s", got, want)
	}
	for _, test := range []struct {
		name   string
		result domain.Result
		want   string
	}{
		{"length mismatch", domain.Result{State: domain.StateFailed, ErrorMessage: "source_length_mismatch",
			Outputs: map[string]string{"error": "source_length_mismatch"}}, "source_length_mismatch"},
		{"read failure", domain.Result{State: domain.StateFailed, ErrorMessage: "source_read_failed",
			Outputs: map[string]string{"error": "source_read_failed"}}, "source_read_failed"},
		{"cancelled in the handler", domain.Result{State: domain.StateCancelled, ErrorMessage: "delivery_cancelled",
			Outputs: map[string]string{"error": "delivery_cancelled"}}, "delivery_cancelled"},
		{"stopped subsystem", domain.Result{State: domain.StateExpired, ErrorMessage: "context deadline exceeded"},
			"delivery_cancelled"},
		{"no handler class", domain.Result{State: domain.StateFailed}, "worker_failed"},
	} {
		want := `{"error":"` + test.want + `","offer_id":"offer-digest","worker_id":"worker-1"}`
		if got := encodedResult(t, record, test.result); got != want {
			t.Fatalf("%s:\n got %s\nwant %s", test.name, got, want)
		}
	}
}

func TestTaskOfferResultRejectsADigestWithoutEvidence(t *testing.T) {
	record := sourceDigestRecord(t, 4096)
	valid := map[string]string{"sha1": strings.Repeat("b", 40), "sha256": strings.Repeat("a", 64), "bytes": "4096"}
	for name, mutate := range map[string]func(map[string]string){
		"no sha1":         func(outputs map[string]string) { delete(outputs, "sha1") },
		"short sha1":      func(outputs map[string]string) { outputs["sha1"] = outputs["sha1"][:39] },
		"uppercase sha1":  func(outputs map[string]string) { outputs["sha1"] = strings.ToUpper(outputs["sha1"]) },
		"no sha256":       func(outputs map[string]string) { delete(outputs, "sha256") },
		"sha1 as sha256":  func(outputs map[string]string) { outputs["sha256"] = outputs["sha1"] },
		"no bytes":        func(outputs map[string]string) { delete(outputs, "bytes") },
		"short bytes":     func(outputs map[string]string) { outputs["bytes"] = "4095" },
		"long bytes":      func(outputs map[string]string) { outputs["bytes"] = "4097" },
		"malformed bytes": func(outputs map[string]string) { outputs["bytes"] = "4096 bytes" },
	} {
		outputs := map[string]string{}
		for key, value := range valid {
			outputs[key] = value
		}
		mutate(outputs)
		_, err := taskOfferResult(record, domain.Result{State: domain.StateCompleted, Outputs: outputs})
		var terminal *dispatch.TerminalDeliveryError
		if !errors.As(err, &terminal) {
			t.Fatalf("%s: expected terminal evidence error, got %v", name, err)
		}
	}
}

func TestCapabilityManifestAdvertisesSourceDigestWithProtocolVersion1(t *testing.T) {
	digestOnly := []string{contracts.TransferSourceDigestCapability}
	streaming := beamcoreadapter.SourceDigestResources().MemoryBytes
	for _, test := range []struct {
		name         string
		capabilities []string
		memory       int64
		want         bool
	}{
		{"capable worker", digestOnly, streaming, true},
		{"worker below the streaming reservation", digestOnly, streaming - 1, false},
		{"worker without the capability", []string{contracts.TransferMultipartCapability}, streaming, false},
	} {
		control, _ := authenticatedControl(t, test.capabilities, test.memory)
		manifest := control.capabilityManifest(time.Now())
		if got := contracts.SupportsCapability(manifest, contracts.TransferSourceDigestCapability); got != test.want {
			t.Fatalf("%s: advertised=%v manifest=%+v", test.name, got, manifest.Capabilities)
		}
	}
	control, _ := authenticatedControl(t, []string{contracts.TransferMultipartCapability,
		contracts.TransferAuthenticatedPartCapability, contracts.TransferSourceDigestCapability}, 64<<20)
	manifest := control.capabilityManifest(time.Now())
	advertised := map[string]contracts.ProtocolRange{}
	for _, protocol := range manifest.Protocols {
		advertised[protocol.Name] = protocol
	}
	if advertised[contracts.TransferSourceDigestCapability] != (contracts.ProtocolRange{
		Name: contracts.TransferSourceDigestCapability, Min: 1, Max: 1}) ||
		!contracts.SupportsCapability(manifest, contracts.TransferAuthenticatedPartCapability) {
		t.Fatalf("source digest is not advertised beside authenticated parts: %+v", manifest.Protocols)
	}
}

func TestTaskOfferBatchDispatchesSourceDigestOffers(t *testing.T) {
	control, tasks := authenticatedControl(t, []string{contracts.TransferSourceDigestCapability},
		beamcoreadapter.SourceDigestResources().MemoryBytes)
	digestOffer := map[string]any{
		"offer_id": "offer-digest", "kind": "source_digest",
		"source": map[string]any{"url": "https://source.invalid/object", "size": 1610612736,
			"headers": map[string]string{"x-amz-security-token": "source-token"}},
		"digest": map[string]any{"algorithms": []string{"sha1", "sha256"}},
	}
	batch := beamCorePayload(t, map[string]any{
		"type": "task_offer_batch", "batch_id": "batch-digest", "assignment_timeout_ms": 2_400_000,
		"offers": []map[string]any{digestOffer},
	})
	if err := control.handleTaskOfferBatch(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	record, ok := tasks.Record("offer-digest/offer-digest")
	if !ok || record.Spec.Kind != domain.KindTransferSourceDigest {
		t.Fatalf("digest offer was not dispatched as a digest workload: %+v", record)
	}
	var digest contracts.SourceDigest
	if err := json.Unmarshal(record.Spec.Payload, &digest); err != nil {
		t.Fatal(err)
	}
	if digest.Size != 1610612736 || digest.Source.Headers["x-amz-security-token"] != "source-token" {
		t.Fatalf("msgpack offer lost digest fields: %+v", digest)
	}
	unknown := map[string]any{"offer_id": "offer-unknown", "kind": "source_hash",
		"source": digestOffer["source"], "digest": digestOffer["digest"]}
	batch = beamCorePayload(t, map[string]any{
		"type": "task_offer_batch", "batch_id": "batch-unknown", "assignment_timeout_ms": 30_000,
		"offers": []map[string]any{unknown},
	})
	if err := control.handleTaskOfferBatch(context.Background(), batch); err == nil ||
		!strings.Contains(err.Error(), "unsupported BeamCore offer kind") {
		t.Fatalf("unknown offer kind was not rejected: %v", err)
	}
	if _, ok := tasks.Record("offer-unknown/offer-unknown"); ok {
		t.Fatal("an offer of an unknown kind was dispatched")
	}
}
