package evidence

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestTransferReceiptCommitsAuthenticatedPartEvidence(t *testing.T) {
	_, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	identity := domain.Identity{WorkerID: "worker-1", OrchestratorID: "orchestrator-1", NodeID: nodeID(publicKey)}
	now := time.Now().UTC()
	receipt, err := NewWorkloadReceipt(identity, privateKey, domain.Spec{
		WorkloadID: "offer-box", AttemptID: "offer-box", Identity: identity, Kind: domain.KindTransferMultipart,
		Evidence: domain.EvidencePolicy{ReceiptRequired: true,
			Commitments: []string{"sha256", "sha1", "request_count", "provider_part_id"}},
	}, domain.Result{State: domain.StateCompleted, StartedAt: now.Add(-time.Second), CompletedAt: now, Outputs: map[string]string{
		"part.0.sha256": "digest-256", "part.0.sha1": "digest-1", "part.0.request_count": "2",
		"part.0.provider_part_id": "PART8", "part.0.unlisted": "ignored",
	}})
	if err != nil {
		t.Fatal(err)
	}
	committed := map[string]string{}
	for _, commitment := range receipt.Commitments {
		committed[commitment.Name] = commitment.Value
	}
	if committed["part.0.sha1"] != "digest-1" || committed["part.0.request_count"] != "2" ||
		committed["part.0.provider_part_id"] != "PART8" || committed["part.0.sha256"] != "digest-256" ||
		committed["part.0.unlisted"] != "" || receipt.Verify() != nil {
		t.Fatalf("authenticated evidence is not committed: %+v", receipt.Commitments)
	}
}
