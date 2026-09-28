package evidence

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func testCircuitReceipts(t *testing.T, count int) []Receipt {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.Identity{OrchestratorID: "orch", WorkerID: "worker", NodeID: nodeID(publicKey)}
	now := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	receipts := make([]Receipt, 0, count)
	for i := range count {
		plan := circuit.Plan{CircuitID: fmt.Sprintf("circuit-%d", i), PlanVersion: 1, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
		receipt, err := NewCircuitReceipt(identity, privateKey, plan, "authorized", now, "")
		if err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts
}

func TestFileJournalAcknowledgeManyPersistsBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts.json")
	journal, err := OpenFileJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	receipts := testCircuitReceipts(t, 3)
	for _, receipt := range receipts {
		if _, err := journal.Put(receipt); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 9, 28, 8, 1, 0, 0, time.UTC)
	acknowledgements := map[string]time.Time{
		receipts[0].ReceiptID: at,
		receipts[1].ReceiptID: at,
		"receipt_unknown":     at,
	}
	if err := journal.AcknowledgeMany(acknowledgements); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	pending := reopened.Pending()
	if len(pending) != 1 || pending[0].ReceiptID != receipts[2].ReceiptID {
		t.Fatalf("pending after reopen = %+v, want only %s", pending, receipts[2].ReceiptID)
	}
	record, err := reopened.Get(receipts[0].ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.AcknowledgedAt.Equal(at) {
		t.Fatalf("acknowledged_at = %s, want %s", record.AcknowledgedAt, at)
	}
}

func TestMemoryJournalAcknowledgeManyKeepsFirstAcknowledgement(t *testing.T) {
	journal := NewMemoryJournal()
	receipt := testCircuitReceipts(t, 1)[0]
	if _, err := journal.Put(receipt); err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 9, 28, 8, 1, 0, 0, time.UTC)
	if err := journal.AcknowledgeMany(map[string]time.Time{receipt.ReceiptID: first, "receipt_unknown": first}); err != nil {
		t.Fatal(err)
	}
	if err := journal.AcknowledgeMany(map[string]time.Time{receipt.ReceiptID: first.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	record, err := journal.Get(receipt.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.AcknowledgedAt.Equal(first) || len(journal.Pending()) != 0 {
		t.Fatalf("record = %+v pending = %d", record, len(journal.Pending()))
	}
}
