package payment

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"time"

	workerevidence "github.com/Beam-Network/beam/internal/evidence"
	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
)

type Sink interface {
	SubmitPaymentEvidence(context.Context, Proof) error
}

type TaskResolver interface {
	Record(string) (dispatch.Record, bool)
}

type Service struct {
	orchestratorID string
	key            ed25519.PrivateKey
	signer         LegacySigner
	store          Store
	tasks          TaskResolver
	now            func() time.Time

	mu       sync.Mutex
	proofMu  sync.Mutex
	receipts map[string]map[string]workerevidence.Receipt
	sink     Sink
}

func NewService(orchestratorID string, key ed25519.PrivateKey, signer LegacySigner, store Store, tasks TaskResolver) (*Service, error) {
	if orchestratorID == "" || len(key) != ed25519.PrivateKeySize || store == nil || tasks == nil {
		return nil, errors.New("payment service requires Orchestrator identity, key, store, and task resolver")
	}
	return &Service{orchestratorID: orchestratorID, key: key, signer: signer, store: store, tasks: tasks,
		now: time.Now, receipts: make(map[string]map[string]workerevidence.Receipt)}, nil
}

func (s *Service) RegisterSink(sink Sink) {
	s.mu.Lock()
	s.sink = sink
	s.mu.Unlock()
}

func (s *Service) Observe(ctx context.Context, receipt workerevidence.Receipt) error {
	if err := receipt.Verify(); err != nil {
		return err
	}
	if receipt.OrchestratorID != s.orchestratorID {
		return errors.New("receipt belongs to another Orchestrator")
	}
	s.mu.Lock()
	group := s.receipts[receipt.WorkloadID]
	if group == nil {
		group = make(map[string]workerevidence.Receipt)
		s.receipts[receipt.WorkloadID] = group
	}
	group[receipt.ReceiptID] = receipt
	if receipt.Type != workerevidence.ReceiptTransfer || receipt.State != "completed" {
		s.mu.Unlock()
		return nil
	}
	record, ok := s.tasks.Record(receipt.WorkloadID + "/" + receipt.AttemptID)
	if !ok || record.Source != dispatch.SourceBeamCore || record.WorkerID != receipt.WorkerID {
		s.mu.Unlock()
		return errors.New("transfer receipt does not match a BeamCore dispatch record")
	}
	receipts := make([]workerevidence.Receipt, 0, len(group))
	for _, candidate := range group {
		if candidate.ReceiptID == receipt.ReceiptID || candidate.Type == workerevidence.ReceiptCircuit {
			receipts = append(receipts, candidate)
		}
	}
	s.mu.Unlock()
	s.proofMu.Lock()
	defer s.proofMu.Unlock()
	if existing, exists := s.store.RecordForTask(receipt.WorkerID, receipt.WorkloadID, receipt.AttemptID); exists {
		if !existing.AcknowledgedAt.IsZero() {
			return nil
		}
		return s.submit(ctx, existing.Proof)
	}
	issuedAt := s.now().UTC()
	if issuedAt.Before(receipt.CompletedAt) {
		issuedAt = receipt.CompletedAt
	}
	proof, err := Build(ctx, s.orchestratorID, s.key, receipts, s.signer, issuedAt)
	if err != nil {
		return err
	}
	_, err = s.store.Put(proof)
	if err != nil {
		return err
	}
	return s.submit(ctx, proof)
}

func (s *Service) Replay(ctx context.Context) {
	for _, proof := range s.store.Pending() {
		_ = s.submit(ctx, proof)
	}
}

func (s *Service) Pending() []Proof { return s.store.Pending() }

func (s *Service) submit(ctx context.Context, proof Proof) error {
	s.mu.Lock()
	sink := s.sink
	s.mu.Unlock()
	if sink == nil {
		return nil
	}
	if err := sink.SubmitPaymentEvidence(ctx, proof); err != nil {
		return err
	}
	return s.store.Acknowledge(proof.EvidenceID, s.now().UTC())
}
