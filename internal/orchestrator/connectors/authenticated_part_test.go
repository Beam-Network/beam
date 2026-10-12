package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func authenticatedPart(index int) contracts.TransferPart {
	return contracts.TransferPart{Index: index, Length: 8, Protocol: contracts.UploadProtocolAuthenticatedPart,
		Profile: contracts.UploadProfileBox, MaxAttempts: 3}
}

func authenticatedRecord(t *testing.T, transfer contracts.MultipartTransfer) dispatch.Record {
	t.Helper()
	payload, err := json.Marshal(transfer)
	if err != nil {
		t.Fatal(err)
	}
	return dispatch.Record{WorkerID: "worker-1", Spec: domain.Spec{AttemptID: "offer-box", Payload: payload,
		Evidence: domain.EvidencePolicy{Commitments: []string{"sha256", "sha1", "request_count", "provider_part_id"}}}}
}

func encodedResult(t *testing.T, record dispatch.Record, result domain.Result) string {
	t.Helper()
	payload, err := taskOfferResult(record, result)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

func TestTaskOfferResultForwardsAuthenticatedEvidence(t *testing.T) {
	record := authenticatedRecord(t, contracts.MultipartTransfer{Parts: []contracts.TransferPart{authenticatedPart(0)}})
	sha256, sha1 := strings.Repeat("a", 64), strings.Repeat("b", 40)
	success := domain.Result{State: domain.StateCompleted, Outputs: map[string]string{
		"part.0.sha256": sha256, "part.0.sha1": sha1, "part.0.request_count": "2", "part.0.provider_part_id": "PART8",
	}}
	want := `{"chunk_hash":"` + sha256 + `","destinations":[{"provider_part_id":"PART8","request_count":2,"sha1":"` + sha1 +
		`"}],"offer_id":"offer-box","worker_id":"worker-1"}`
	if got := encodedResult(t, record, success); got != want {
		t.Fatalf("unexpected authenticated result:\n got %s\nwant %s", got, want)
	}
	failure := domain.Result{State: domain.StateFailed, ErrorMessage: "part 0: provider_throttled", Outputs: map[string]string{
		"part.0.sha1": sha1, "part.0.request_count": "3", "part.0.error": "provider_throttled",
		"part.0.retry_after_ms": "7000",
	}}
	want = `{"destinations":[{"error":"provider_throttled","request_count":3,"retry_after_ms":7000,"sha1":"` + sha1 +
		`"}],"offer_id":"offer-box","worker_id":"worker-1"}`
	if got := encodedResult(t, record, failure); got != want {
		t.Fatalf("unexpected authenticated failure:\n got %s\nwant %s", got, want)
	}
	for name, mutate := range map[string]func(map[string]string){
		"short sha1":         func(outputs map[string]string) { outputs["part.0.sha1"] = sha1[:39] },
		"uppercase sha1":     func(outputs map[string]string) { outputs["part.0.sha1"] = strings.ToUpper(sha1) },
		"no request count":   func(outputs map[string]string) { delete(outputs, "part.0.request_count") },
		"zero request count": func(outputs map[string]string) { outputs["part.0.request_count"] = "0" },
		"no part id":         func(outputs map[string]string) { delete(outputs, "part.0.provider_part_id") },
		"no sha256":          func(outputs map[string]string) { delete(outputs, "part.0.sha256") },
	} {
		outputs := map[string]string{}
		for key, value := range success.Outputs {
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

func TestTaskOfferResultReportsAuthenticatedFanoutDestinations(t *testing.T) {
	record := authenticatedRecord(t, contracts.MultipartTransfer{Fanout: true, Parts: []contracts.TransferPart{
		authenticatedPart(0), {Index: 1, Length: 8, ETagRequired: true}, authenticatedPart(2),
	}})
	sha256, sha1 := strings.Repeat("a", 64), strings.Repeat("c", 40)
	result := domain.Result{State: domain.StateFailed, ErrorMessage: "fanout_delivery_failed", Outputs: map[string]string{
		"part.0.state": "completed", "part.0.bytes": "8", "part.0.sha256": sha256, "part.0.sha1": sha1,
		"part.0.request_count": "1", "part.0.provider_part_id": "PART0",
		"part.1.state": "completed", "part.1.bytes": "8", "part.1.sha256": sha256, "part.1.etag": "verified",
		"part.2.state": "failed", "part.2.sha1": sha1, "part.2.request_count": "1", "part.2.error": "authorization_lost",
	}}
	want := `{"chunk_hash":"` + sha256 + `","destinations":[{"provider_part_id":"PART0","request_count":1,"sha1":"` + sha1 +
		`"},{"etag":"verified"},{"error":"authorization_lost","request_count":1,"sha1":"` + sha1 +
		`"}],"offer_id":"offer-box","worker_id":"worker-1"}`
	if got := encodedResult(t, record, result); got != want {
		t.Fatalf("unexpected fan-out result:\n got %s\nwant %s", got, want)
	}
	delete(result.Outputs, "part.0.provider_part_id")
	if _, err := taskOfferResult(record, result); err == nil {
		t.Fatal("authenticated fan-out success without a provider part id was accepted")
	}
}

func authenticatedControl(t *testing.T, capabilities []string, memory int64) (*roomControl, *dispatch.Service) {
	t.Helper()
	now := time.Now().UTC()
	orchestratorRegistry, err := registry.New(orchestratordomain.Orchestrator{OrchestratorID: "orch-1", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := orchestratorRegistry.Join(orchestratordomain.Membership{OrchestratorID: "orch-1",
		WorkerID: "worker-1", NodeID: "node-1", Status: "active", JoinedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	resources := domain.Resources{MemoryBytes: memory, BandwidthMbps: 8, Connections: 8, Streams: 8}
	if err := orchestratorRegistry.UpdateObservation(orchestratordomain.WorkerObservation{
		WorkerID: "worker-1", NodeID: "node-1", Status: "active", Capabilities: capabilities,
		Total: resources, Available: resources, ObservedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	tasks, err := dispatch.NewService(dispatch.Config{OrchestratorID: "orch-1"}, orchestratorRegistry,
		fakeControlWCP{}, dispatch.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	return newRoomControl(NATSConfig{Environment: "dev", Hotkey: "hotkey-1"}, nil, nil, nil, tasks), tasks
}

func TestCapabilityManifestAdvertisesAuthenticatedPartsWithTheirBuffer(t *testing.T) {
	capable := []string{contracts.TransferMultipartCapability, contracts.TransferAuthenticatedPartCapability}
	probe := int64(contracts.AuthenticatedPartProbeBytes + contracts.TransferBufferOverheadBytes)
	for _, test := range []struct {
		name         string
		capabilities []string
		memory       int64
		want         bool
	}{
		{"capable worker", capable, probe, true},
		{"worker below the part buffer", capable, probe - 1, false},
		{"worker without the capability", []string{contracts.TransferMultipartCapability}, probe, false},
	} {
		control, _ := authenticatedControl(t, test.capabilities, test.memory)
		manifest := control.capabilityManifest(time.Now())
		if got := contracts.SupportsCapability(manifest, contracts.TransferAuthenticatedPartCapability); got != test.want {
			t.Fatalf("%s: advertised=%v manifest=%+v", test.name, got, manifest.Capabilities)
		}
	}
}

func TestTaskOfferBatchCarriesAuthenticatedDestinationFields(t *testing.T) {
	control, tasks := authenticatedControl(t,
		[]string{contracts.TransferMultipartCapability, contracts.TransferAuthenticatedPartCapability}, 64<<20)
	batch := beamCorePayload(t, map[string]any{
		"type": "task_offer_batch", "batch_id": "batch-box", "assignment_timeout_ms": 30_000,
		"offers": []map[string]any{{
			"offer_id": "offer-box",
			"source":   map[string]any{"url": "https://source.invalid/object", "chunk_size": 8 << 20, "headers": map[string]string{"Range": "bytes=0-8388607"}},
			"destinations": []map[string]any{{
				"url":      "https://upload.box.com/api/2.0/files/upload_sessions/session-1",
				"headers":  map[string]string{"Authorization": "Bearer worker-token", "Content-Range": "bytes 0-8388607/20971520"},
				"protocol": "authenticated_part_v1", "profile": "box", "method": "PUT",
				"digest":       map[string]any{"algorithm": "sha1", "header": "Digest", "format": "sha=base64"},
				"receipt":      map[string]any{"path": "part", "fields": []string{"part_id", "offset", "size", "sha1"}},
				"max_attempts": 3,
			}},
		}},
	})
	if err := control.handleTaskOfferBatch(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	record, ok := tasks.Record("offer-box/offer-box")
	if !ok {
		t.Fatal("authenticated offer was not dispatched")
	}
	var transfer contracts.MultipartTransfer
	if err := json.Unmarshal(record.Spec.Payload, &transfer); err != nil {
		t.Fatal(err)
	}
	part := transfer.Parts[0]
	if part.Destination.Method != "PUT" || part.Protocol != contracts.UploadProtocolAuthenticatedPart || part.Profile != "box" ||
		part.MaxAttempts != 3 || part.Digest == nil || part.Digest.Header != "Digest" || part.Receipt == nil ||
		part.Receipt.Path != "part" || len(part.Receipt.Fields) != 4 {
		t.Fatalf("msgpack offer lost authenticated fields: %+v", part)
	}
}
