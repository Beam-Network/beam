package evidence

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

type Recorder struct {
	identity   domain.Identity
	privateKey ed25519.PrivateKey
	journal    Journal
	workloads  runtime.Store
}

func NewRecorder(identity domain.Identity, privateKey ed25519.PrivateKey, journal Journal, workloads runtime.Store) (*Recorder, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if identity.OrchestratorID == "" || identity.NodeID == "" {
		return nil, errors.New("orchestrator_id and node_id are required for receipt recording")
	}
	if len(privateKey) != ed25519.PrivateKeySize || journal == nil || workloads == nil {
		return nil, errors.New("receipt signing key, journal, and workload store are required")
	}
	return &Recorder{identity: identity, privateKey: privateKey, journal: journal, workloads: workloads}, nil
}

// Run watches terminal results and also reconciles the durable workload store.
// The periodic pass closes the crash window between persisting a result and
// persisting its receipt.
func (r *Recorder) Run(ctx context.Context, results <-chan domain.Result) error {
	if err := r.ReconcileWorkloads(); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case result := <-results:
			record, err := r.workloads.Get(result.WorkloadID + "/" + result.AttemptID)
			if err != nil {
				return err
			}
			if _, err := r.RecordWorkload(record.Spec, result); err != nil {
				return err
			}
		case <-ticker.C:
			if err := r.ReconcileWorkloads(); err != nil {
				return err
			}
		}
	}
}

func (r *Recorder) ReconcileWorkloads() error {
	for _, record := range r.workloads.List() {
		if record.Result == nil {
			continue
		}
		if _, err := r.RecordWorkload(record.Spec, *record.Result); err != nil {
			return err
		}
	}
	return nil
}

func (r *Recorder) RecordWorkload(spec domain.Spec, result domain.Result) (Receipt, error) {
	receipt, err := NewWorkloadReceipt(r.identity, r.privateKey, spec, result)
	if err != nil {
		return Receipt{}, err
	}
	_, err = r.journal.Put(receipt)
	return receipt, err
}

func (r *Recorder) RecordCircuit(plan circuit.Plan, event string, observedAt time.Time, reason string) (Receipt, error) {
	for _, record := range r.journal.Records() {
		if record.Receipt.Type == ReceiptCircuit && record.Receipt.CircuitID == plan.CircuitID &&
			record.Receipt.PlanVersion == plan.PlanVersion && record.Receipt.Event == event {
			return record.Receipt, nil
		}
	}
	receipt, err := NewCircuitReceipt(r.identity, r.privateKey, plan, event, observedAt, reason)
	if err != nil {
		return Receipt{}, err
	}
	_, err = r.journal.Put(receipt)
	return receipt, err
}

func (r *Recorder) Pending() []Receipt { return r.journal.Pending() }

func (r *Recorder) Acknowledge(receiptID string, at time.Time) error {
	if err := r.commitWorkload(receiptID, at); err != nil {
		return err
	}
	return r.journal.Acknowledge(receiptID, at)
}

// AcknowledgeMany commits each receipt's workload and then records every
// acknowledgement with a single journal write. Receipts the journal no longer
// holds are skipped. On error nothing in the journal is marked, so the
// Orchestrator's idempotent acknowledgement of a resent receipt retries it.
func (r *Recorder) AcknowledgeMany(acknowledgements map[string]time.Time) error {
	known := make(map[string]time.Time, len(acknowledgements))
	for receiptID, at := range acknowledgements {
		if err := r.commitWorkload(receiptID, at); err != nil {
			if errors.Is(err, ErrReceiptNotFound) {
				continue
			}
			return err
		}
		known[receiptID] = at
	}
	return r.journal.AcknowledgeMany(known)
}

func (r *Recorder) commitWorkload(receiptID string, at time.Time) error {
	record, err := r.journal.Get(receiptID)
	if err != nil {
		return err
	}
	if !record.AcknowledgedAt.IsZero() || record.Receipt.WorkloadID == "" || record.Receipt.AttemptID == "" {
		return nil
	}
	workload, err := r.workloads.Get(record.Receipt.WorkloadID + "/" + record.Receipt.AttemptID)
	if err != nil {
		if errors.Is(err, runtime.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if workload.State != domain.StateReceiptCommitted && domain.CanTransition(workload.State, domain.StateReceiptCommitted) {
		workload.State = domain.StateReceiptCommitted
		workload.UpdatedAt = at.UTC()
		return r.workloads.Save(workload)
	}
	return nil
}

func (r *Recorder) Journal() Journal { return r.journal }
