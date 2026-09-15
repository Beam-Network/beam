package roomtransfer

import (
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

func TestHybridEvidenceIsBoundToLeaseRangeAndSourceHash(t *testing.T) {
	now := time.Now().UTC()
	batch := contracts.RoomTaskOfferBatch{SchemaVersion: contracts.RoomStorageSchemaVersion, TransferID: "publication", FileSizeBytes: 12, ChunkSizeBytes: 6, ChunkCount: 2}
	offer := contracts.RoomSourceLane{LaneID: "lane", Attempt: 1, ChunkStart: 0, ChunkEnd: 1, TargetMemberIDs: []string{"bucket"}}
	source := contracts.TunnelLease{LeaseID: "source-lease", Role: contracts.TunnelLeaseRoleSourceRead, ExpiresAt: now.Add(time.Minute), Storage: &contracts.StorageLease{MemberID: "source", ResourceID: "source", SizeBytes: 12, ETag: "frozen"}}
	target := contracts.TunnelLease{LeaseID: "target-lease", Role: contracts.TunnelLeaseRoleTargetWrite, TargetMemberID: "bucket", ExpiresAt: now.Add(time.Minute), Storage: &contracts.StorageLease{MemberID: "bucket", ResourceID: "bucket", SizeBytes: 12}}
	lane := LaneRecord{SourceLease: &source, TargetLeases: map[string]contracts.TunnelLease{"bucket": target}}
	digest := strings.Repeat("a", 64)
	result := contracts.RoomTaskResult{StorageResults: []contracts.StorageRangeResult{
		{LeaseID: source.LeaseID, MemberID: "source", Role: source.Role, ChunkIndex: 0, Length: 6, RangeSHA256: digest, CompletedAt: now},
		{LeaseID: target.LeaseID, MemberID: "bucket", Role: target.Role, ChunkIndex: 0, Length: 6, RangeSHA256: digest, UploadID: "upload", PartNumber: 1, ETag: "part", CompletedAt: now},
	}}
	if err := verifyStorageEvidence(batch, offer, lane, result, now); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*contracts.StorageRangeResult)
	}{
		{"wrong lease", func(value *contracts.StorageRangeResult) { value.LeaseID = "other-attempt" }},
		{"wrong member", func(value *contracts.StorageRangeResult) { value.MemberID = "another-bucket" }},
		{"wrong range", func(value *contracts.StorageRangeResult) { value.Offset = 6 }},
		{"wrong bytes", func(value *contracts.StorageRangeResult) { value.RangeSHA256 = strings.Repeat("b", 64) }},
		{"missing upload", func(value *contracts.StorageRangeResult) { value.UploadID = "" }},
		{"late receipt", func(value *contracts.StorageRangeResult) { value.CompletedAt = now.Add(time.Hour) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := result
			candidate.StorageResults = append([]contracts.StorageRangeResult(nil), result.StorageResults...)
			test.mutate(&candidate.StorageResults[1])
			if verifyStorageEvidence(batch, offer, lane, candidate, now) == nil {
				t.Fatal("invalid evidence was accepted")
			}
		})
	}
	batch.SchemaVersion = contracts.RoomTransferSchemaVersion
	if verifyStorageEvidence(batch, offer, lane, result, now) == nil {
		t.Fatal("storage evidence entered the agent-only contract")
	}
}
