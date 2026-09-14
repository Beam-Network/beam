package contracts

import (
	"errors"
	"math"
	"time"
)

// These counters measure admitted source payloads, independently of fanout.
// Wire bytes include the MLS envelope when present; payload bytes do not.
type SourceReadEvidence struct {
	ChunkIndex   int64 `json:"chunk_index"`
	ReadCount    int64 `json:"read_count"`
	PayloadBytes int64 `json:"payload_bytes"`
	WireBytes    int64 `json:"wire_bytes"`
}

func (read SourceReadEvidence) Validate(fileSize, chunkSize, start, end int64, protection RoomTransferProtection) error {
	if read.ChunkIndex < start || read.ChunkIndex > end || read.ReadCount <= 0 {
		return errors.New("source read counters are outside the assignment")
	}
	_, length := ChunkRange(fileSize, chunkSize, read.ChunkIndex)
	wireLength := length
	if protection.Valid() {
		wireLength += 25
	}
	if length <= 0 || wireLength <= 0 || read.ReadCount > math.MaxInt64/wireLength ||
		read.PayloadBytes != length*read.ReadCount || read.WireBytes != wireLength*read.ReadCount {
		return errors.New("source read counters do not match the protected range")
	}
	return nil
}

// StorageLease is an immutable endpoint identity. The lease endpoint redeems
// short-lived provider routes; neither credentials nor provider URLs belong in
// durable assignments or checkpoints.
type StorageLease struct {
	MemberID   string `json:"member_id"`
	ResourceID string `json:"resource_id"`
	SizeBytes  int64  `json:"size_bytes"`
	ETag       string `json:"etag,omitempty"`
	VersionID  string `json:"version_id,omitempty"`
}

func (storage StorageLease) Validate(role string) error {
	if storage.MemberID == "" || storage.ResourceID == "" || storage.SizeBytes <= 0 {
		return errors.New("storage lease identity is invalid")
	}
	if role == TunnelLeaseRoleSourceRead && storage.ETag == "" && storage.VersionID == "" {
		return errors.New("storage source must be frozen by provider identity")
	}
	return nil
}

type StorageRouteRequest struct {
	ContentMD5    string `json:"content_md5,omitempty"`
	SchemaVersion string `json:"schema_version"`
	LeaseID       string `json:"lease_id"`
	TransferID    string `json:"transfer_id"`
	LaneID        string `json:"lane_id"`
	Attempt       int64  `json:"attempt"`
	WorkerID      string `json:"worker_id"`
	ChunkIndex    int64  `json:"chunk_index"`
}

// StorageRoute exists only while executing one chunk. Provider-specific route
// signing and multipart preparation remain owned by the credential adapter.
type StorageRoute struct {
	ChunkIndex int64        `json:"chunk_index"`
	Offset     int64        `json:"offset"`
	Length     int64        `json:"length"`
	ExpiresAt  time.Time    `json:"expires_at"`
	Endpoint   HTTPEndpoint `json:"endpoint"`
	PartNumber int64        `json:"part_number,omitempty"`
	UploadID   string       `json:"upload_id,omitempty"`
}

// StorageRangeResult records worker-observed delivery evidence. It is not a
// final receipt: Core requires provider verification and upload finalization.
type StorageRangeResult struct {
	LeaseID     string    `json:"lease_id"`
	MemberID    string    `json:"member_id"`
	Role        string    `json:"role"`
	ChunkIndex  int64     `json:"chunk_index"`
	Offset      int64     `json:"offset"`
	Length      int64     `json:"length"`
	RangeSHA256 string    `json:"range_sha256"`
	ETag        string    `json:"etag,omitempty"`
	VersionID   string    `json:"version_id,omitempty"`
	PartNumber  int64     `json:"part_number,omitempty"`
	UploadID    string    `json:"upload_id,omitempty"`
	CompletedAt time.Time `json:"completed_at"`
}

// MultipartAttemptPartNumber follows the standard transfer part-slot contract.
// Room attempt numbers are one-based; each logical chunk reserves three slots.
func MultipartAttemptPartNumber(chunkIndex, attempt int64) int64 {
	if chunkIndex < 0 || chunkIndex >= 10000/3 || attempt < 1 {
		return 0
	}
	return chunkIndex*3 + (attempt-1)%3 + 1
}
