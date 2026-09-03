package evidence

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

const FormatVersion uint32 = 1

type ReceiptType string

const (
	ReceiptWorkload ReceiptType = "workload"
	ReceiptTransfer ReceiptType = "transfer"
	ReceiptTunnel   ReceiptType = "tunnel"
	ReceiptRoom     ReceiptType = "room"
	ReceiptCircuit  ReceiptType = "circuit"
)

type Commitment struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Receipt is the signed, portable evidence envelope emitted by a Worker. Maps
// are deliberately excluded from the signed form: commitments are sorted by
// name before hashing so the canonical bytes are stable across processes.
type Receipt struct {
	FormatVersion   uint32       `json:"format_version"`
	ReceiptID       string       `json:"receipt_id"`
	Type            ReceiptType  `json:"type"`
	Event           string       `json:"event"`
	OrchestratorID  string       `json:"orchestrator_id"`
	WorkerID        string       `json:"worker_id"`
	NodeID          string       `json:"node_id"`
	InstanceID      string       `json:"instance_id,omitempty"`
	WorkloadID      string       `json:"workload_id,omitempty"`
	AttemptID       string       `json:"attempt_id,omitempty"`
	CircuitID       string       `json:"circuit_id,omitempty"`
	Capability      string       `json:"capability"`
	State           domain.State `json:"state,omitempty"`
	PlanVersion     uint64       `json:"plan_version,omitempty"`
	BytesProcessed  int64        `json:"bytes_processed,omitempty"`
	Commitments     []Commitment `json:"commitments,omitempty"`
	StartedAt       time.Time    `json:"started_at"`
	CompletedAt     time.Time    `json:"completed_at"`
	IssuedAt        time.Time    `json:"issued_at"`
	SignerPublicKey string       `json:"signer_public_key"`
	Signature       string       `json:"signature"`
}

type canonicalReceipt struct {
	FormatVersion   uint32       `json:"format_version"`
	Type            ReceiptType  `json:"type"`
	Event           string       `json:"event"`
	OrchestratorID  string       `json:"orchestrator_id"`
	WorkerID        string       `json:"worker_id"`
	NodeID          string       `json:"node_id"`
	InstanceID      string       `json:"instance_id,omitempty"`
	WorkloadID      string       `json:"workload_id,omitempty"`
	AttemptID       string       `json:"attempt_id,omitempty"`
	CircuitID       string       `json:"circuit_id,omitempty"`
	Capability      string       `json:"capability"`
	State           domain.State `json:"state,omitempty"`
	PlanVersion     uint64       `json:"plan_version,omitempty"`
	BytesProcessed  int64        `json:"bytes_processed,omitempty"`
	Commitments     []Commitment `json:"commitments,omitempty"`
	StartedAt       string       `json:"started_at"`
	CompletedAt     string       `json:"completed_at"`
	IssuedAt        string       `json:"issued_at"`
	SignerPublicKey string       `json:"signer_public_key"`
}

func NewWorkloadReceipt(identity domain.Identity, privateKey ed25519.PrivateKey, spec domain.Spec, result domain.Result) (Receipt, error) {
	if (result.WorkloadID != "" && result.WorkloadID != spec.WorkloadID) ||
		(result.AttemptID != "" && result.AttemptID != spec.AttemptID) {
		return Receipt{}, errors.New("workload result identity does not match its specification")
	}
	receiptType := ReceiptWorkload
	switch spec.Kind {
	case domain.KindTransferMultipart, domain.KindTransferRange, domain.KindTransferDistribute:
		receiptType = ReceiptTransfer
	case domain.KindTunnelHTTP, domain.KindTunnelTCP:
		receiptType = ReceiptTunnel
	case domain.KindRoomMedia, domain.KindRoomTransfer, domain.KindRoomDatagram,
		domain.KindRoomMessage, domain.KindRoomCommand, domain.KindRoomStream:
		receiptType = ReceiptRoom
	}
	commitments, err := workloadCommitments(spec, result)
	if err != nil {
		return Receipt{}, err
	}
	receipt := Receipt{
		FormatVersion: FormatVersion, Type: receiptType, Event: string(result.State),
		OrchestratorID: identity.OrchestratorID, WorkerID: identity.WorkerID, NodeID: identity.NodeID, InstanceID: identity.InstanceID,
		WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID, Capability: string(spec.Kind), State: result.State,
		BytesProcessed: result.BytesProcessed, Commitments: commitments,
		StartedAt: result.StartedAt, CompletedAt: result.CompletedAt, IssuedAt: result.CompletedAt,
	}
	return sign(receipt, privateKey)
}

func NewCircuitReceipt(identity domain.Identity, privateKey ed25519.PrivateKey, plan circuit.Plan, event string, observedAt time.Time, reason string) (Receipt, error) {
	if event != "authorized" && event != "revoked" && event != "expired" {
		return Receipt{}, fmt.Errorf("unsupported circuit receipt event %q", event)
	}
	if observedAt.IsZero() {
		return Receipt{}, errors.New("circuit receipt observation time is required")
	}
	digest, err := circuitPlanDigest(plan)
	if err != nil {
		return Receipt{}, err
	}
	commitments := []Commitment{
		{Name: "expires_at", Value: canonicalTime(plan.ExpiresAt)},
		{Name: "peer_count", Value: fmt.Sprintf("%d", len(plan.Peers))},
		{Name: "plan_sha256", Value: digest},
	}
	if reason != "" {
		commitments = append(commitments, Commitment{Name: "reason", Value: reason})
	}
	receipt := Receipt{
		FormatVersion: FormatVersion, Type: ReceiptCircuit, Event: event,
		OrchestratorID: identity.OrchestratorID, WorkerID: identity.WorkerID, NodeID: identity.NodeID, InstanceID: identity.InstanceID,
		WorkloadID: plan.WorkloadID, CircuitID: plan.CircuitID, Capability: "beamlink.circuit",
		PlanVersion: plan.PlanVersion, Commitments: commitments,
		StartedAt: plan.IssuedAt, CompletedAt: observedAt, IssuedAt: observedAt,
	}
	return sign(receipt, privateKey)
}

func sign(receipt Receipt, privateKey ed25519.PrivateKey) (Receipt, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return Receipt{}, errors.New("Ed25519 receipt signing key is required")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	receipt.SignerPublicKey = base64.RawURLEncoding.EncodeToString(publicKey)
	canonical, err := receipt.canonicalBytes()
	if err != nil {
		return Receipt{}, err
	}
	digest := sha256.Sum256(canonical)
	receipt.ReceiptID = "receipt_" + base64.RawURLEncoding.EncodeToString(digest[:])
	receipt.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, signatureMessage(canonical)))
	if err := receipt.Verify(); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func (r Receipt) Verify() error {
	if r.FormatVersion != FormatVersion {
		return fmt.Errorf("unsupported receipt format version %d", r.FormatVersion)
	}
	if r.ReceiptID == "" || r.OrchestratorID == "" || r.WorkerID == "" || r.NodeID == "" || r.Capability == "" || r.Event == "" {
		return errors.New("receipt identity, type, event, and capability are required")
	}
	if !slices.Contains([]ReceiptType{ReceiptWorkload, ReceiptTransfer, ReceiptTunnel, ReceiptRoom, ReceiptCircuit}, r.Type) {
		return fmt.Errorf("unsupported receipt type %q", r.Type)
	}
	if r.BytesProcessed < 0 || r.StartedAt.IsZero() || r.CompletedAt.IsZero() || r.IssuedAt.IsZero() {
		return errors.New("receipt timestamps and non-negative bytes are required")
	}
	if r.CompletedAt.Before(r.StartedAt) || r.IssuedAt.Before(r.CompletedAt) {
		return errors.New("receipt timestamps are not monotonic")
	}
	if r.Type == ReceiptCircuit {
		if r.CircuitID == "" || r.PlanVersion == 0 {
			return errors.New("circuit receipts require circuit_id and plan_version")
		}
	} else if r.WorkloadID == "" || r.AttemptID == "" ||
		!slices.Contains([]domain.State{domain.StateCompleted, domain.StateFailed, domain.StateCancelled, domain.StateExpired}, r.State) {
		return errors.New("workload receipts require workload identity and a terminal state")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(r.SignerPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("invalid receipt signer public key")
	}
	if nodeID(ed25519.PublicKey(publicKey)) != r.NodeID {
		return errors.New("receipt signer does not match node_id")
	}
	canonical, err := r.canonicalBytes()
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	if r.ReceiptID != "receipt_"+base64.RawURLEncoding.EncodeToString(digest[:]) {
		return errors.New("receipt_id does not match canonical receipt")
	}
	signature, err := base64.RawURLEncoding.DecodeString(r.Signature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), signatureMessage(canonical), signature) {
		return errors.New("invalid receipt signature")
	}
	return nil
}

func (r Receipt) canonicalBytes() ([]byte, error) {
	commitments := append([]Commitment(nil), r.Commitments...)
	sort.Slice(commitments, func(i, j int) bool {
		if commitments[i].Name == commitments[j].Name {
			return commitments[i].Value < commitments[j].Value
		}
		return commitments[i].Name < commitments[j].Name
	})
	for index, commitment := range commitments {
		if commitment.Name == "" {
			return nil, errors.New("receipt commitment name is required")
		}
		if index > 0 && commitments[index-1].Name == commitment.Name {
			return nil, fmt.Errorf("duplicate receipt commitment %q", commitment.Name)
		}
	}
	return json.Marshal(canonicalReceipt{
		FormatVersion: r.FormatVersion, Type: r.Type, Event: r.Event,
		OrchestratorID: r.OrchestratorID, WorkerID: r.WorkerID, NodeID: r.NodeID, InstanceID: r.InstanceID,
		WorkloadID: r.WorkloadID, AttemptID: r.AttemptID, CircuitID: r.CircuitID,
		Capability: r.Capability, State: r.State, PlanVersion: r.PlanVersion,
		BytesProcessed: r.BytesProcessed, Commitments: commitments,
		StartedAt: canonicalTime(r.StartedAt), CompletedAt: canonicalTime(r.CompletedAt),
		IssuedAt: canonicalTime(r.IssuedAt), SignerPublicKey: r.SignerPublicKey,
	})
}

func workloadCommitments(spec domain.Spec, result domain.Result) ([]Commitment, error) {
	type output struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	outputs := make([]output, 0, len(result.Outputs))
	for name, value := range result.Outputs {
		outputs = append(outputs, output{Name: name, Value: value})
	}
	sort.Slice(outputs, func(i, j int) bool { return outputs[i].Name < outputs[j].Name })
	encoded, err := json.Marshal(struct {
		State        domain.State `json:"state"`
		Bytes        int64        `json:"bytes_processed"`
		Outputs      []output     `json:"outputs,omitempty"`
		ErrorCode    string       `json:"error_code,omitempty"`
		ErrorMessage string       `json:"error_message,omitempty"`
	}{result.State, result.BytesProcessed, outputs, result.ErrorCode, result.ErrorMessage})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	selected := map[string]string{"result_sha256": base64.RawURLEncoding.EncodeToString(digest[:])}
	for name, value := range result.Outputs {
		if standardCommitment(spec.Kind, name) || requestedCommitment(spec.Evidence.Commitments, name) {
			selected[name] = value
		}
	}
	if result.ErrorCode != "" {
		selected["error_code"] = result.ErrorCode
	}
	commitments := make([]Commitment, 0, len(selected))
	for name, value := range selected {
		commitments = append(commitments, Commitment{Name: name, Value: value})
	}
	sort.Slice(commitments, func(i, j int) bool { return commitments[i].Name < commitments[j].Name })
	return commitments, nil
}

func standardCommitment(kind domain.Kind, name string) bool {
	switch kind {
	case domain.KindTransferMultipart, domain.KindTransferRange, domain.KindTransferDistribute:
		return strings.HasSuffix(name, ".sha256") || strings.HasSuffix(name, ".etag") || name == "sha256" || name == "etag"
	case domain.KindTunnelHTTP, domain.KindTunnelTCP:
		return name == "listen_address" || name == "requests" || name == "connections"
	case domain.KindRoomMedia:
		return name == "listen_address" || name == "packets" || name == "dropped"
	case domain.KindRoomDatagram, domain.KindRoomMessage, domain.KindRoomCommand, domain.KindRoomStream:
		return name == "room_result_details"
	case domain.KindRoomTransfer:
		return name == "target_receipts_sha256" || name == "delivered_cells"
	default:
		return false
	}
}

func requestedCommitment(requested []string, name string) bool {
	for _, candidate := range requested {
		if candidate == name || strings.HasSuffix(name, "."+candidate) {
			return true
		}
	}
	return false
}

func circuitPlanDigest(plan circuit.Plan) (string, error) {
	peers := append([]circuit.Peer(nil), plan.Peers...)
	for index := range peers {
		peers[index].Roles = append([]string(nil), peers[index].Roles...)
		peers[index].Capabilities = append([]string(nil), peers[index].Capabilities...)
		sort.Strings(peers[index].Roles)
		sort.Strings(peers[index].Capabilities)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].NodeID < peers[j].NodeID })
	encoded, err := json.Marshal(struct {
		CircuitID   string         `json:"circuit_id"`
		WorkloadID  string         `json:"workload_id"`
		PlanVersion uint64         `json:"plan_version"`
		Peers       []circuit.Peer `json:"peers"`
		ExpiresAt   string         `json:"expires_at"`
		IssuedAt    string         `json:"issued_at"`
	}{plan.CircuitID, plan.WorkloadID, plan.PlanVersion, peers, canonicalTime(plan.ExpiresAt), canonicalTime(plan.IssuedAt)})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func canonicalTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func signatureMessage(canonical []byte) []byte {
	message := make([]byte, 0, len(canonical)+24)
	message = append(message, []byte("beam:receipt:v1\x00")...)
	return append(message, canonical...)
}

func nodeID(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return "node_" + base64.RawURLEncoding.EncodeToString(digest[:20])
}
