package contracts

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	RoomTransferSchemaVersion    = "room-transfer/v1"
	TransferMultipartCapability  = "transfer.multipart"
	RoomTransferCapability       = "room.transfer"
	RoomTransferDirectCapability = "room.transfer.direct.v1"
	RoomTransferE2EECapability   = "room.transfer.e2ee.v2"
	RoomTransferProtectionScheme = "btr.object.chunk.aead.v1"
	RoomStorageSchemaVersion     = "room-storage-transfer/v2"
	RoomStorageCapability        = "room.transfer.storage.v2"
	RoomStorageProtectionScheme  = "btr.object.transport.tls.v1"
	TunnelLeaseRoleSourceRead    = "source_read"
	TunnelLeaseRoleTargetWrite   = "target_write"
)

type RoomTransferProtection struct {
	Scheme   string `json:"scheme"`
	KeyEpoch uint64 `json:"key_epoch"`
}

func (protection RoomTransferProtection) Valid() bool {
	return protection.Scheme == RoomTransferProtectionScheme && protection.KeyEpoch > 0
}

func (protection RoomTransferProtection) Storage() bool {
	return protection.Scheme == RoomStorageProtectionScheme && protection.KeyEpoch == 0
}

type ProtocolRange struct {
	Name string `json:"name"`
	Min  int    `json:"min"`
	Max  int    `json:"max"`
}

type CapabilityManifest struct {
	SchemaVersion   string          `json:"schema_version"`
	ActorType       string          `json:"actor_type"`
	ActorID         string          `json:"actor_id"`
	SoftwareVersion string          `json:"software_version"`
	Protocols       []ProtocolRange `json:"protocols"`
	Capabilities    []string        `json:"capabilities"`
	Capacity        struct {
		MaxConnections       int64 `json:"max_connections"`
		AvailableConnections int64 `json:"available_connections"`
	} `json:"capacity"`
	ObservedAt time.Time `json:"observed_at"`
}

type RoomTaskOfferBatch struct {
	Type            string                 `json:"type"`
	SchemaVersion   string                 `json:"schema_version"`
	BatchID         string                 `json:"batch_id"`
	RoomID          string                 `json:"room_id"`
	ChannelID       string                 `json:"channel_id"`
	PublicationID   string                 `json:"publication_id"`
	TransferID      string                 `json:"transfer_id"`
	SnapshotVersion uint64                 `json:"snapshot_version"`
	Protection      RoomTransferProtection `json:"protection"`
	FileSizeBytes   int64                  `json:"file_size_bytes"`
	ChunkSizeBytes  int64                  `json:"chunk_size_bytes"`
	ChunkCount      int64                  `json:"chunk_count"`
	Targets         []RoomTransferTarget   `json:"targets"`
	Lanes           []RoomSourceLane       `json:"lanes"`
	OfferExpiresAt  time.Time              `json:"offer_expires_at"`
}

type RoomTaskCancel struct {
	LaneID        string    `json:"lane_id,omitempty"`
	Attempt       int64     `json:"attempt,omitempty"`
	Type          string    `json:"type"`
	SchemaVersion string    `json:"schema_version"`
	TransferID    string    `json:"transfer_id"`
	Reason        string    `json:"reason"`
	CancelledAt   time.Time `json:"cancelled_at"`
}

func (cancel RoomTaskCancel) Validate() error {
	if (cancel.LaneID == "") != (cancel.Attempt == 0) || cancel.Attempt < 0 ||
		(cancel.LaneID != "" && cancel.SchemaVersion != RoomStorageSchemaVersion) {
		return errors.New("invalid scoped room cancellation")
	}
	if cancel.Type != "room_task_cancel" || (cancel.SchemaVersion != RoomTransferSchemaVersion && cancel.SchemaVersion != RoomStorageSchemaVersion) ||
		strings.TrimSpace(cancel.TransferID) == "" || strings.TrimSpace(cancel.Reason) == "" || cancel.CancelledAt.IsZero() {
		return errors.New("invalid room transfer cancellation")
	}
	return nil
}

type RoomTransferTarget struct {
	Kind             string `json:"kind,omitempty"`
	MemberID         string `json:"member_id"`
	ReceiptPublicKey string `json:"receipt_public_key"`
}

type RoomSourceLane struct {
	LaneID              string              `json:"lane_id"`
	Attempt             int64               `json:"attempt"`
	ChunkStart          int64               `json:"chunk_start"`
	ChunkEnd            int64               `json:"chunk_end"`
	TargetMemberIDs     []string            `json:"target_member_ids"`
	SourceIntent        TunnelLeaseIntent   `json:"source_intent"`
	TargetIntents       []TunnelLeaseIntent `json:"target_intents"`
	TimingBudgetSeconds int64               `json:"timing_budget_seconds,omitempty"`
}

type TunnelLeaseIntent struct {
	IntentID                 string    `json:"intent_id"`
	TransferID               string    `json:"transfer_id"`
	LaneID                   string    `json:"lane_id"`
	Attempt                  int64     `json:"attempt"`
	Role                     string    `json:"role"`
	TargetMemberID           string    `json:"target_member_id,omitempty"`
	ChunkStart               int64     `json:"chunk_start"`
	ChunkEnd                 int64     `json:"chunk_end"`
	OrchestratorID           string    `json:"orchestrator_id"`
	RequiredWorkerCapability string    `json:"required_worker_capability"`
	ExpiresAt                time.Time `json:"expires_at"`
	Signature                string    `json:"signature"`
}

type TunnelLeaseEndpoint struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type TunnelLease struct {
	LeaseID        string                `json:"lease_id"`
	IntentID       string                `json:"intent_id"`
	Role           string                `json:"role"`
	TargetMemberID string                `json:"target_member_id,omitempty"`
	Protocol       string                `json:"protocol"`
	Endpoints      []TunnelLeaseEndpoint `json:"endpoints"`
	AgentPublicKey string                `json:"agent_public_key,omitempty"`
	ExpiresAt      time.Time             `json:"expires_at"`
	Storage        *StorageLease         `json:"storage,omitempty"`
}

type RoomTransferDestination struct {
	MemberID string      `json:"member_id"`
	Lease    TunnelLease `json:"lease"`
}

type RoomTransfer struct {
	SchemaVersion   string                    `json:"schema_version"`
	BatchID         string                    `json:"batch_id"`
	RoomID          string                    `json:"room_id"`
	ChannelID       string                    `json:"channel_id"`
	PublicationID   string                    `json:"publication_id"`
	TransferID      string                    `json:"transfer_id"`
	SnapshotVersion uint64                    `json:"snapshot_version"`
	Protection      RoomTransferProtection    `json:"protection"`
	LaneID          string                    `json:"lane_id"`
	Attempt         int64                     `json:"attempt"`
	FileSizeBytes   int64                     `json:"file_size_bytes"`
	ChunkSizeBytes  int64                     `json:"chunk_size_bytes"`
	ChunkCount      int64                     `json:"chunk_count"`
	ChunkStart      int64                     `json:"chunk_start"`
	ChunkEnd        int64                     `json:"chunk_end"`
	SourceLease     TunnelLease               `json:"source_lease"`
	Targets         []RoomTransferDestination `json:"targets"`
}

type SourceRangeReceipt struct {
	ReceiptID      string    `json:"receipt_id"`
	TransferID     string    `json:"transfer_id"`
	LaneID         string    `json:"lane_id"`
	ChunkIndex     int64     `json:"chunk_index"`
	Offset         int64     `json:"offset"`
	Length         int64     `json:"length"`
	RangeSHA256    string    `json:"range_sha256"`
	LeaseID        string    `json:"lease_id"`
	CompletedAt    time.Time `json:"completed_at"`
	AgentPublicKey string    `json:"agent_public_key"`
	AgentSignature string    `json:"agent_signature"`
}

type SourceFailureReceipt struct {
	ReceiptID      string    `json:"receipt_id"`
	TransferID     string    `json:"transfer_id"`
	LaneID         string    `json:"lane_id"`
	ChunkIndex     int64     `json:"chunk_index"`
	Code           string    `json:"code"`
	LeaseID        string    `json:"lease_id"`
	ObservedAt     time.Time `json:"observed_at"`
	AgentPublicKey string    `json:"agent_public_key"`
	AgentSignature string    `json:"agent_signature"`
}

type TargetRangeReceipt struct {
	SourceRangeReceipt
	TargetMemberID string `json:"target_member_id"`
}

type FinalTargetReceipt struct {
	ReceiptID      string    `json:"receipt_id"`
	TransferID     string    `json:"transfer_id"`
	TargetMemberID string    `json:"target_member_id"`
	FileSizeBytes  int64     `json:"file_size_bytes"`
	ManifestSHA256 string    `json:"manifest_sha256"`
	CompletedAt    time.Time `json:"completed_at"`
	AgentPublicKey string    `json:"agent_public_key"`
	AgentSignature string    `json:"agent_signature"`
}

type RoomMissingCells struct {
	TargetMemberID string  `json:"target_member_id"`
	ChunkIndices   []int64 `json:"chunk_indices"`
}

type RoomFailure struct {
	Origin               string                `json:"origin"`
	Code                 string                `json:"code"`
	Retryable            bool                  `json:"retryable"`
	TargetMemberID       string                `json:"target_member_id,omitempty"`
	ChunkIndices         []int64               `json:"chunk_indices,omitempty"`
	Detail               string                `json:"detail,omitempty"`
	SourceFailureReceipt *SourceFailureReceipt `json:"source_failure_receipt,omitempty"`
}

type RoomTaskResult struct {
	SourceReads             []SourceReadEvidence       `json:"source_reads,omitempty"`
	Type                    string                     `json:"type"`
	SchemaVersion           string                     `json:"schema_version"`
	ResultID                string                     `json:"result_id"`
	BatchID                 string                     `json:"batch_id"`
	RoomID                  string                     `json:"room_id"`
	TransferID              string                     `json:"transfer_id"`
	LaneID                  string                     `json:"lane_id"`
	Attempt                 int64                      `json:"attempt"`
	WorkerID                string                     `json:"worker_id"`
	WorkerAcknowledgedAt    *time.Time                 `json:"worker_acknowledged_at,omitempty"`
	ExecutableLeaseIssuedAt *time.Time                 `json:"executable_lease_issued_at,omitempty"`
	ExecutionStage          string                     `json:"execution_stage"`
	Runtime                 *DirectRoomTransferRuntime `json:"runtime,omitempty"`
	SourceReceipts          []SourceRangeReceipt       `json:"source_receipts"`
	TargetReceipts          []TargetRangeReceipt       `json:"target_receipts"`
	FinalTargetReceipts     []FinalTargetReceipt       `json:"final_target_receipts"`
	Missing                 []RoomMissingCells         `json:"missing"`
	Failures                []RoomFailure              `json:"failures"`
	ReportedAt              time.Time                  `json:"reported_at"`
	StorageResults          []StorageRangeResult       `json:"storage_results,omitempty"`
}

type DirectRoomTransferRuntime struct {
	LaneID               string    `json:"lane_id"`
	Attempt              int64     `json:"attempt"`
	WorkerID             string    `json:"worker_id"`
	Capability           string    `json:"capability"`
	Transport            string    `json:"transport"`
	BaseURL              string    `json:"base_url"`
	AccessToken          string    `json:"access_token"`
	ExpiresAt            time.Time `json:"expires_at"`
	TLSCertificateSHA256 string    `json:"tls_certificate_sha256,omitempty"`
}

func (runtime DirectRoomTransferRuntime) Validate(workerID, laneID string, attempt int64, now time.Time) error {
	parsed, err := url.Parse(runtime.BaseURL)
	validTransport := runtime.Capability == RoomTransferDirectCapability && runtime.Transport == "worker_http"
	if runtime.Capability == RoomStorageCapability {
		validTransport = err == nil && parsed.Scheme == "https" && runtime.Transport == "worker_https" && validSHA256(runtime.TLSCertificateSHA256)
	}
	if runtime.WorkerID != workerID || runtime.LaneID != laneID || runtime.Attempt != attempt ||
		!validTransport ||
		runtime.AccessToken == "" || runtime.ExpiresAt.IsZero() || !now.Before(runtime.ExpiresAt) ||
		err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("invalid direct room transfer runtime")
	}
	return nil
}

func (b RoomTaskOfferBatch) Validate(now time.Time) error {
	validProtection := b.SchemaVersion == RoomTransferSchemaVersion && b.Protection.Valid() || b.SchemaVersion == RoomStorageSchemaVersion && b.Protection.Storage()
	if b.Type != "room_task_offer_batch" || !validProtection || b.BatchID == "" || b.RoomID == "" ||
		b.ChannelID == "" || b.PublicationID == "" || b.TransferID == "" || b.SnapshotVersion == 0 {
		return errors.New("room batch identity or schema is invalid")
	}
	if b.FileSizeBytes <= 0 || b.ChunkSizeBytes <= 0 || b.ChunkCount != (b.FileSizeBytes+b.ChunkSizeBytes-1)/b.ChunkSizeBytes {
		return errors.New("room batch file layout is invalid")
	}
	if len(b.Targets) == 0 || len(b.Targets) > 10_000 || len(b.Lanes) == 0 || len(b.Lanes) > 256 {
		return errors.New("room batch target or lane count is invalid")
	}
	if b.OfferExpiresAt.IsZero() || !now.Before(b.OfferExpiresAt) {
		return errors.New("room batch offer has expired")
	}
	targets := make(map[string]RoomTransferTarget, len(b.Targets))
	for _, target := range b.Targets {
		validTarget := (target.Kind == "" || target.Kind == "agent") && validEd25519Key(target.ReceiptPublicKey)
		if b.SchemaVersion == RoomStorageSchemaVersion && target.Kind == "object_storage" {
			validTarget = target.ReceiptPublicKey == ""
		}
		if target.MemberID == "" || !validTarget {
			return errors.New("room batch target identity or receipt key is invalid")
		}
		if _, duplicate := targets[target.MemberID]; duplicate {
			return fmt.Errorf("duplicate room target %s", target.MemberID)
		}
		targets[target.MemberID] = target
	}
	lanes := make(map[string]struct{}, len(b.Lanes))
	for _, lane := range b.Lanes {
		for _, intent := range append([]TunnelLeaseIntent{lane.SourceIntent}, lane.TargetIntents...) {
			if intent.TransferID != b.TransferID {
				return errors.New("room intent belongs to another publication")
			}
			expected := RoomTransferE2EECapability
			if b.SchemaVersion == RoomStorageSchemaVersion {
				expected = RoomStorageCapability
			}
			if intent.RequiredWorkerCapability != expected {
				return errors.New("room intent protection capability mismatch")
			}
		}
		if err := lane.Validate(b.ChunkCount, targets, now); err != nil {
			return err
		}
		if _, duplicate := lanes[lane.LaneID]; duplicate {
			return fmt.Errorf("duplicate room source lane %s", lane.LaneID)
		}
		lanes[lane.LaneID] = struct{}{}
	}
	return nil
}

func NormalizeCapabilities(capabilities []string) []string {
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		capability = strings.TrimSpace(capability)
		if capability != "" {
			seen[capability] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for capability := range seen {
		result = append(result, capability)
	}
	sort.Strings(result)
	return result
}

func ProtocolRangesForCapabilities(capabilities []string) []ProtocolRange {
	capabilities = NormalizeCapabilities(capabilities)
	protocols := make([]ProtocolRange, 0, len(capabilities))
	for _, capability := range capabilities {
		protocols = append(protocols, ProtocolRange{Name: capability, Min: 1, Max: 1})
	}
	return protocols
}

func NewCapabilityManifest(actorType, actorID, softwareVersion string, capabilities []string,
	maxConnections, availableConnections int64, now time.Time) CapabilityManifest {
	capabilities = NormalizeCapabilities(capabilities)
	if len(capabilities) == 0 {
		maxConnections = 0
		availableConnections = 0
	}
	if maxConnections < 0 {
		maxConnections = 0
	}
	if availableConnections < 0 {
		availableConnections = 0
	}
	if availableConnections > maxConnections {
		availableConnections = maxConnections
	}
	manifest := CapabilityManifest{SchemaVersion: RoomTransferSchemaVersion, ActorType: actorType,
		ActorID: strings.TrimSpace(actorID), SoftwareVersion: strings.TrimSpace(softwareVersion),
		Protocols: ProtocolRangesForCapabilities(capabilities), Capabilities: capabilities,
		ObservedAt: now.UTC()}
	manifest.Capacity.MaxConnections = maxConnections
	manifest.Capacity.AvailableConnections = availableConnections
	return manifest
}

func NewOrchestratorCapabilityManifest(actorID, softwareVersion string, capabilities []string,
	maxConnections, availableConnections int64, now time.Time) CapabilityManifest {
	return NewCapabilityManifest("orchestrator", actorID, softwareVersion, capabilities, maxConnections, availableConnections, now)
}

func NewWorkerCapabilityManifest(actorID, softwareVersion string, capabilities []string,
	maxConnections, availableConnections int64, now time.Time) CapabilityManifest {
	return NewCapabilityManifest("worker", actorID, softwareVersion, capabilities, maxConnections, availableConnections, now)
}

func SupportsCapability(manifest CapabilityManifest, capability string) bool {
	capability = strings.TrimSpace(capability)
	if capability == "" || manifest.Capacity.AvailableConnections <= 0 {
		return false
	}
	foundCapability := false
	for _, advertised := range manifest.Capabilities {
		if advertised == capability {
			foundCapability = true
			break
		}
	}
	if !foundCapability {
		return false
	}
	for _, protocol := range manifest.Protocols {
		if protocol.Name == capability && protocol.Min <= 1 && protocol.Max >= 1 {
			return true
		}
	}
	return false
}

func (lane RoomSourceLane) Validate(chunkCount int64, targets map[string]RoomTransferTarget, now time.Time) error {
	if lane.LaneID == "" || lane.Attempt <= 0 || lane.ChunkStart < 0 || lane.ChunkEnd < lane.ChunkStart || lane.ChunkEnd >= chunkCount ||
		len(lane.TargetMemberIDs) == 0 || len(lane.TargetMemberIDs) != len(lane.TargetIntents) {
		return errors.New("room source lane range or target list is invalid")
	}
	if err := lane.SourceIntent.Validate(TunnelLeaseRoleSourceRead, "", lane, now); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(lane.TargetMemberIDs))
	intents := make(map[string]TunnelLeaseIntent, len(lane.TargetIntents))
	for _, intent := range lane.TargetIntents {
		if _, exists := targets[intent.TargetMemberID]; !exists {
			return errors.New("lane target intent is outside the target snapshot")
		}
		if err := intent.Validate(TunnelLeaseRoleTargetWrite, intent.TargetMemberID, lane, now); err != nil {
			return err
		}
		if _, duplicate := intents[intent.TargetMemberID]; duplicate {
			return errors.New("lane contains duplicate target intents")
		}
		intents[intent.TargetMemberID] = intent
	}
	for _, memberID := range lane.TargetMemberIDs {
		if _, exists := targets[memberID]; !exists {
			return errors.New("lane target is outside the target snapshot")
		}
		if _, duplicate := seen[memberID]; duplicate {
			return errors.New("lane contains duplicate targets")
		}
		seen[memberID] = struct{}{}
		if _, ok := intents[memberID]; !ok {
			return errors.New("lane target has no lease intent")
		}
	}
	return nil
}

func (intent TunnelLeaseIntent) Validate(role, target string, lane RoomSourceLane, now time.Time) error {
	if intent.IntentID == "" || intent.TransferID == "" || intent.Signature == "" || intent.Role != role ||
		intent.LaneID != lane.LaneID || intent.Attempt != lane.Attempt || intent.ChunkStart != lane.ChunkStart ||
		intent.ChunkEnd != lane.ChunkEnd || intent.OrchestratorID == "" || (intent.RequiredWorkerCapability != RoomTransferE2EECapability && intent.RequiredWorkerCapability != RoomStorageCapability) ||
		intent.ExpiresAt.IsZero() || !now.Before(intent.ExpiresAt) || intent.TargetMemberID != target {
		return errors.New("room tunnel lease intent is invalid")
	}
	return nil
}

func (lease TunnelLease) Validate(role, target string, now time.Time) error {
	if lease.LeaseID == "" || lease.IntentID == "" || lease.Role != role || (lease.Protocol != RoomTransferDirectCapability && lease.Protocol != RoomStorageCapability) ||
		lease.TargetMemberID != target || lease.ExpiresAt.IsZero() || !now.Before(lease.ExpiresAt) {
		return errors.New("tunnel lease identity, role, protocol, or expiry is invalid")
	}
	if len(lease.Endpoints) == 0 || len(lease.Endpoints) > 8 {
		return errors.New("tunnel lease must contain between 1 and 8 endpoints")
	}
	for _, endpoint := range lease.Endpoints {
		parsed, err := url.Parse(endpoint.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return errors.New("tunnel lease endpoint must be an absolute HTTP(S) URL")
		}
	}
	if lease.Storage != nil {
		if lease.Protocol != RoomStorageCapability || lease.AgentPublicKey != "" || len(lease.Endpoints) != 1 {
			return errors.New("storage lease must have one scoped route endpoint and no agent key")
		}
		endpoint, _ := url.Parse(lease.Endpoints[0].URL)
		if endpoint.Scheme != "https" || endpoint.User != nil || endpoint.Fragment != "" || lease.Endpoints[0].Method != "POST" {
			return errors.New("storage route control requires HTTPS POST")
		}
		return lease.Storage.Validate(role)
	}
	if !validEd25519Key(lease.AgentPublicKey) {
		return errors.New("tunnel lease requires an Ed25519 agent public key")
	}
	return nil
}

func (receipt SourceRangeReceipt) Verify(expectedKey string, now time.Time) error {
	if receipt.ReceiptID == "" || receipt.TransferID == "" || receipt.LaneID == "" || receipt.ChunkIndex < 0 ||
		receipt.Offset < 0 || receipt.Length <= 0 || receipt.LeaseID == "" || receipt.CompletedAt.IsZero() ||
		receipt.CompletedAt.After(now.Add(5*time.Minute)) || receipt.AgentPublicKey != expectedKey || !validSHA256(receipt.RangeSHA256) {
		return errors.New("source range receipt is invalid")
	}
	return verifyReceiptSignature(expectedKey, receipt.AgentSignature, receiptMessage("beam:room-source-range-receipt",
		receipt.ReceiptID, receipt.TransferID, receipt.LaneID, fmt.Sprint(receipt.ChunkIndex), fmt.Sprint(receipt.Offset),
		fmt.Sprint(receipt.Length), receipt.RangeSHA256, receipt.LeaseID, receipt.CompletedAt.Format(time.RFC3339Nano), receipt.AgentPublicKey))
}

func (receipt SourceFailureReceipt) Verify(expectedKey string, now time.Time) error {
	if receipt.ReceiptID == "" || receipt.TransferID == "" || receipt.LaneID == "" || receipt.ChunkIndex < 0 ||
		receipt.LeaseID == "" || receipt.ObservedAt.IsZero() || receipt.ObservedAt.After(now.Add(5*time.Minute)) ||
		receipt.AgentPublicKey != expectedKey || (receipt.Code != "source_file_mutated" && receipt.Code != "source_integrity_failed") {
		return errors.New("source failure receipt is invalid")
	}
	return verifyReceiptSignature(expectedKey, receipt.AgentSignature, receiptMessage("beam:room-source-failure-receipt",
		receipt.ReceiptID, receipt.TransferID, receipt.LaneID, fmt.Sprint(receipt.ChunkIndex), receipt.Code, receipt.LeaseID,
		receipt.ObservedAt.Format(time.RFC3339Nano), receipt.AgentPublicKey))
}

func (receipt TargetRangeReceipt) Verify(expectedKey string, now time.Time) error {
	if receipt.TargetMemberID == "" {
		return errors.New("target range receipt member is required")
	}
	base := receipt.SourceRangeReceipt
	if base.ReceiptID == "" || base.TransferID == "" || base.LaneID == "" || base.ChunkIndex < 0 || base.Offset < 0 ||
		base.Length <= 0 || base.LeaseID == "" || base.CompletedAt.IsZero() || base.CompletedAt.After(now.Add(5*time.Minute)) ||
		base.AgentPublicKey != expectedKey || !validSHA256(base.RangeSHA256) {
		return errors.New("target range receipt is invalid")
	}
	return verifyReceiptSignature(expectedKey, base.AgentSignature, receiptMessage("beam:room-target-range-receipt",
		base.ReceiptID, base.TransferID, base.LaneID, fmt.Sprint(base.ChunkIndex), fmt.Sprint(base.Offset), fmt.Sprint(base.Length),
		base.RangeSHA256, base.LeaseID, base.CompletedAt.Format(time.RFC3339Nano), base.AgentPublicKey, receipt.TargetMemberID))
}

func (receipt FinalTargetReceipt) Verify(expectedKey string, now time.Time) error {
	if receipt.ReceiptID == "" || receipt.TransferID == "" || receipt.TargetMemberID == "" || receipt.FileSizeBytes <= 0 ||
		receipt.CompletedAt.IsZero() || receipt.CompletedAt.After(now.Add(5*time.Minute)) || receipt.AgentPublicKey != expectedKey ||
		!validSHA256(receipt.ManifestSHA256) {
		return errors.New("final target receipt is invalid")
	}
	return verifyReceiptSignature(expectedKey, receipt.AgentSignature, receiptMessage("beam:room-final-target-receipt",
		receipt.ReceiptID, receipt.TransferID, receipt.TargetMemberID, fmt.Sprint(receipt.FileSizeBytes), receipt.ManifestSHA256,
		receipt.CompletedAt.Format(time.RFC3339Nano), receipt.AgentPublicKey))
}

func receiptMessage(domain string, fields ...string) []byte {
	return []byte(domain + "\x00" + strings.Join(fields, "\n"))
}

func verifyReceiptSignature(publicKey, signature string, message []byte) error {
	key, err := base64.RawURLEncoding.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("receipt public key is invalid")
	}
	signed, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || len(signed) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(key), message, signed) {
		return errors.New("receipt signature verification failed")
	}
	return nil
}

func validEd25519Key(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == ed25519.PublicKeySize
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(strings.TrimSpace(value))
	return err == nil && len(decoded) == sha256.Size
}

func ChunkRange(fileSize, chunkSize, chunkIndex int64) (int64, int64) {
	offset := chunkIndex * chunkSize
	return offset, min(chunkSize, fileSize-offset)
}
