package payment

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	workerevidence "github.com/Beam-Network/beam/internal/evidence"
	"github.com/Beam-Network/beam/internal/platform/bittensor"
)

const FormatVersion uint32 = 2

type LegacySigner interface {
	SignPaymentEvidence(context.Context, string, string, string, string) (bittensor.SignedMessage, error)
}

type Proof struct {
	FormatVersion           uint32                   `json:"format_version"`
	EvidenceID              string                   `json:"evidence_id"`
	OrchestratorID          string                   `json:"orchestrator_id"`
	WorkerID                string                   `json:"worker_id"`
	TaskID                  string                   `json:"task_id"`
	OfferID                 string                   `json:"offer_id"`
	Success                 bool                     `json:"success"`
	RequiredPayment         bool                     `json:"required_payment"`
	BytesTransferred        int64                    `json:"bytes_transferred"`
	ChunkHash               string                   `json:"chunk_hash,omitempty"`
	StartedAt               time.Time                `json:"started_at"`
	CompletedAt             time.Time                `json:"completed_at"`
	ReceiptIDs              []string                 `json:"receipt_ids"`
	ReceiptRoot             string                   `json:"receipt_root"`
	Receipts                []workerevidence.Receipt `json:"receipts"`
	LegacyHotkey            string                   `json:"legacy_hotkey,omitempty"`
	LegacyMessage           string                   `json:"legacy_message,omitempty"`
	WorkerSignature         string                   `json:"worker_signature,omitempty"`
	AttestationPublicKey    string                   `json:"attestation_public_key"`
	OrchestratorAttestation string                   `json:"orchestrator_attestation"`
	IssuedAt                time.Time                `json:"issued_at"`
}

type canonicalProof struct {
	FormatVersion        uint32   `json:"format_version"`
	OrchestratorID       string   `json:"orchestrator_id"`
	WorkerID             string   `json:"worker_id"`
	TaskID               string   `json:"task_id"`
	OfferID              string   `json:"offer_id"`
	Success              bool     `json:"success"`
	RequiredPayment      bool     `json:"required_payment"`
	BytesTransferred     int64    `json:"bytes_transferred"`
	ChunkHash            string   `json:"chunk_hash,omitempty"`
	StartedAt            string   `json:"started_at"`
	CompletedAt          string   `json:"completed_at"`
	ReceiptIDs           []string `json:"receipt_ids"`
	ReceiptRoot          string   `json:"receipt_root"`
	LegacyHotkey         string   `json:"legacy_hotkey,omitempty"`
	LegacyMessage        string   `json:"legacy_message,omitempty"`
	WorkerSignature      string   `json:"worker_signature,omitempty"`
	AttestationPublicKey string   `json:"attestation_public_key"`
	IssuedAt             string   `json:"issued_at"`
}

func Build(ctx context.Context, orchestratorID string, key ed25519.PrivateKey, receipts []workerevidence.Receipt, signer LegacySigner, issuedAt time.Time) (Proof, error) {
	if orchestratorID == "" || len(key) != ed25519.PrivateKeySize || issuedAt.IsZero() {
		return Proof{}, errors.New("orchestrator identity, attestation key, and issue time are required")
	}
	verified, terminal, err := normalizeReceipts(orchestratorID, receipts)
	if err != nil {
		return Proof{}, err
	}
	proof := Proof{
		FormatVersion: FormatVersion, OrchestratorID: orchestratorID,
		WorkerID: terminal.WorkerID, TaskID: terminal.WorkloadID, OfferID: terminal.AttemptID,
		Success: true, RequiredPayment: true, BytesTransferred: terminal.BytesProcessed,
		ChunkHash: receiptChunkHash(terminal), StartedAt: terminal.StartedAt.UTC(), CompletedAt: terminal.CompletedAt.UTC(),
		Receipts: verified, IssuedAt: issuedAt.UTC(),
		AttestationPublicKey: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
	}
	proof.ReceiptIDs = make([]string, len(verified))
	for index := range verified {
		proof.ReceiptIDs[index] = verified[index].ReceiptID
	}
	proof.ReceiptRoot = receiptMerkleRoot(proof.ReceiptIDs)
	if signer != nil {
		signed, signErr := signer.SignPaymentEvidence(ctx, proof.WorkerID, proof.TaskID, proof.OfferID, proof.ChunkHash)
		if signErr != nil {
			return Proof{}, fmt.Errorf("sign legacy BeamCore payment evidence: %w", signErr)
		}
		proof.LegacyHotkey, proof.LegacyMessage, proof.WorkerSignature = signed.Hotkey, signed.Message, signed.Signature
	}
	canonical, err := proof.canonicalBytes()
	if err != nil {
		return Proof{}, err
	}
	digest := sha256.Sum256(canonical)
	proof.EvidenceID = "payment_" + base64.RawURLEncoding.EncodeToString(digest[:])
	proof.OrchestratorAttestation = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, attestationMessage(canonical)))
	if err := proof.Verify(); err != nil {
		return Proof{}, err
	}
	return proof, nil
}

func (p Proof) Verify() error {
	if p.FormatVersion != FormatVersion || p.EvidenceID == "" || p.OrchestratorID == "" || p.WorkerID == "" || p.TaskID == "" || p.OfferID == "" {
		return errors.New("payment evidence identity and supported format are required")
	}
	if !p.Success || p.BytesTransferred < 0 || p.StartedAt.IsZero() || p.CompletedAt.Before(p.StartedAt) || p.IssuedAt.Before(p.CompletedAt) {
		return errors.New("payment evidence requires a successful monotonic transfer")
	}
	if p.ChunkHash != "" {
		decoded, err := hex.DecodeString(p.ChunkHash)
		if err != nil || len(decoded) != sha256.Size {
			return errors.New("payment evidence chunk_hash must be hexadecimal SHA-256")
		}
	}
	if p.LegacyHotkey != "" || p.LegacyMessage != "" || p.WorkerSignature != "" {
		expected := strings.Join([]string{"beam-worker-payment-evidence", p.WorkerID, p.TaskID, p.OfferID, p.ChunkHash}, ":")
		if p.LegacyHotkey == "" || p.WorkerSignature == "" || p.LegacyMessage != expected {
			return errors.New("legacy payment evidence fields are incomplete or non-canonical")
		}
	}
	verified, terminal, err := normalizeReceipts(p.OrchestratorID, p.Receipts)
	if err != nil || terminal.WorkerID != p.WorkerID || terminal.WorkloadID != p.TaskID || terminal.AttemptID != p.OfferID {
		return errors.New("payment evidence receipts do not match its task identity")
	}
	if terminal.BytesProcessed != p.BytesTransferred || receiptChunkHash(terminal) != p.ChunkHash ||
		!terminal.StartedAt.Equal(p.StartedAt) || !terminal.CompletedAt.Equal(p.CompletedAt) {
		return errors.New("payment evidence transfer facts do not match its terminal receipt")
	}
	ids := make([]string, len(verified))
	for index := range verified {
		ids[index] = verified[index].ReceiptID
	}
	if !equalStrings(ids, p.ReceiptIDs) || receiptMerkleRoot(ids) != p.ReceiptRoot {
		return errors.New("payment evidence receipt aggregate does not match its Merkle root")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(p.AttestationPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("invalid payment attestation public key")
	}
	canonical, err := p.canonicalBytes()
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	if p.EvidenceID != "payment_"+base64.RawURLEncoding.EncodeToString(digest[:]) {
		return errors.New("payment evidence id does not match canonical payload")
	}
	signature, err := base64.RawURLEncoding.DecodeString(p.OrchestratorAttestation)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), attestationMessage(canonical), signature) {
		return errors.New("invalid Orchestrator payment attestation")
	}
	return nil
}

func (p Proof) canonicalBytes() ([]byte, error) {
	return json.Marshal(canonicalProof{
		FormatVersion: p.FormatVersion, OrchestratorID: p.OrchestratorID, WorkerID: p.WorkerID,
		TaskID: p.TaskID, OfferID: p.OfferID, Success: p.Success, RequiredPayment: p.RequiredPayment,
		BytesTransferred: p.BytesTransferred, ChunkHash: p.ChunkHash,
		StartedAt: p.StartedAt.UTC().Format(time.RFC3339Nano), CompletedAt: p.CompletedAt.UTC().Format(time.RFC3339Nano),
		ReceiptIDs: append([]string(nil), p.ReceiptIDs...), ReceiptRoot: p.ReceiptRoot,
		LegacyHotkey: p.LegacyHotkey, LegacyMessage: p.LegacyMessage, WorkerSignature: p.WorkerSignature,
		AttestationPublicKey: p.AttestationPublicKey, IssuedAt: p.IssuedAt.UTC().Format(time.RFC3339Nano),
	})
}

func normalizeReceipts(orchestratorID string, receipts []workerevidence.Receipt) ([]workerevidence.Receipt, workerevidence.Receipt, error) {
	if len(receipts) == 0 || len(receipts) > 256 {
		return nil, workerevidence.Receipt{}, errors.New("payment evidence requires a bounded receipt aggregate")
	}
	result := append([]workerevidence.Receipt(nil), receipts...)
	sort.Slice(result, func(i, j int) bool { return result[i].ReceiptID < result[j].ReceiptID })
	var terminal workerevidence.Receipt
	for index, receipt := range result {
		if err := receipt.Verify(); err != nil {
			return nil, terminal, err
		}
		if receipt.OrchestratorID != orchestratorID {
			return nil, terminal, errors.New("receipt belongs to another Orchestrator")
		}
		if index > 0 && result[index-1].ReceiptID == receipt.ReceiptID {
			return nil, terminal, errors.New("duplicate receipt in payment aggregate")
		}
		if receipt.Type == workerevidence.ReceiptTransfer && receipt.State == "completed" {
			if terminal.ReceiptID != "" {
				return nil, terminal, errors.New("payment aggregate contains multiple terminal transfer receipts")
			}
			terminal = receipt
		}
	}
	if terminal.ReceiptID == "" {
		return nil, terminal, errors.New("payment aggregate requires a completed transfer receipt")
	}
	for _, receipt := range result {
		if receipt.WorkloadID != terminal.WorkloadID || (receipt.Type != workerevidence.ReceiptCircuit && receipt.ReceiptID != terminal.ReceiptID) {
			return nil, terminal, errors.New("payment aggregate contains an unrelated receipt")
		}
	}
	return result, terminal, nil
}

func receiptChunkHash(receipt workerevidence.Receipt) string {
	var candidates []string
	for _, commitment := range receipt.Commitments {
		if commitment.Name == "sha256" || commitment.Name == "chunk_hash" {
			if isHexSHA256(commitment.Value) {
				return strings.ToLower(commitment.Value)
			}
		}
		if strings.HasSuffix(commitment.Name, ".sha256") && isHexSHA256(commitment.Value) {
			candidates = append(candidates, strings.ToLower(commitment.Value))
		}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

func isHexSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func receiptMerkleRoot(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	level := make([][]byte, len(ids))
	for index, id := range ids {
		digest := sha256.Sum256(append([]byte("beam:payment-receipt:v1\x00"), []byte(id)...))
		level[index] = digest[:]
	}
	for len(level) > 1 {
		if len(level)%2 == 1 {
			level = append(level, level[len(level)-1])
		}
		next := make([][]byte, 0, len(level)/2)
		for index := 0; index < len(level); index += 2 {
			digest := sha256.Sum256(append(append([]byte("beam:payment-node:v1\x00"), level[index]...), level[index+1]...))
			next = append(next, digest[:])
		}
		level = next
	}
	return base64.RawURLEncoding.EncodeToString(level[0])
}

func attestationMessage(canonical []byte) []byte {
	return append([]byte("beam:payment-evidence:v2\x00"), canonical...)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("payment attestation key path is required")
	}
	encoded, err := os.ReadFile(path)
	if err == nil {
		key, decodeErr := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(encoded)))
		if decodeErr != nil || len(key) != ed25519.PrivateKeySize {
			return nil, errors.New("invalid payment attestation key")
		}
		return ed25519.PrivateKey(key), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".payment-key-*.tmp")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return nil, err
	}
	if _, err := temporary.WriteString(base64.RawURLEncoding.EncodeToString(key)); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return nil, err
	}
	return key, nil
}
