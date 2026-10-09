package roomtransfer

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

func TestTargetFailureReceiptIsBoundToMemberLeaseAndChunk(t *testing.T) {
	now := time.Now().UTC()
	_, memberKey, _ := ed25519.GenerateKey(rand.Reader)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	lease := func(id, member string, key ed25519.PrivateKey) contracts.TunnelLease {
		return contracts.TunnelLease{LeaseID: id, Role: contracts.TunnelLeaseRoleTargetWrite, TargetMemberID: member,
			Protocol: contracts.RoomTransferDirectCapability, ExpiresAt: now.Add(time.Hour),
			AgentPublicKey: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))}
	}
	batch := contracts.RoomTaskOfferBatch{SchemaVersion: contracts.RoomTransferSchemaVersion, TransferID: "transfer-1",
		FileSizeBytes: 24, ChunkSizeBytes: 6, ChunkCount: 4}
	offer := contracts.RoomSourceLane{LaneID: "lane-1", Attempt: 1, ChunkStart: 1, ChunkEnd: 2, TargetMemberIDs: []string{"member-a", "member-b"}}
	lane := LaneRecord{TargetLeases: map[string]contracts.TunnelLease{
		"member-a": lease("lease-a", "member-a", memberKey), "member-b": lease("lease-b", "member-b", otherKey)}}
	service := &Service{config: Config{Now: func() time.Time { return now }}}
	sign := func(receipt *contracts.TargetFailureReceipt, key ed25519.PrivateKey) {
		receipt.AgentPublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		message := "beam:room-target-failure-receipt\x00" + strings.Join([]string{receipt.ReceiptID, receipt.TransferID,
			receipt.LaneID, fmt.Sprint(receipt.ChunkIndex), receipt.TargetMemberID, receipt.Code, receipt.LeaseID,
			receipt.ObservedAt.Format(time.RFC3339Nano), receipt.AgentPublicKey}, "\n")
		receipt.AgentSignature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(message)))
	}
	build := func(mutate func(*contracts.RoomFailure, *contracts.TargetFailureReceipt) ed25519.PrivateKey) contracts.RoomTaskResult {
		receipt := contracts.TargetFailureReceipt{ReceiptID: "target-failure-1", TransferID: batch.TransferID, LaneID: offer.LaneID,
			ChunkIndex: 2, TargetMemberID: "member-a", Code: "target_storage_full", LeaseID: "lease-a", ObservedAt: now}
		failure := contracts.RoomFailure{Origin: "target_agent", Code: receipt.Code, Retryable: true,
			TargetMemberID: receipt.TargetMemberID, ChunkIndices: []int64{2}}
		signer := memberKey
		if mutate != nil {
			if key := mutate(&failure, &receipt); key != nil {
				signer = key
			}
		}
		sign(&receipt, signer)
		failure.TargetFailureReceipt = &receipt
		return contracts.RoomTaskResult{SchemaVersion: batch.SchemaVersion, Failures: []contracts.RoomFailure{failure}}
	}
	if err := service.verifyReceipts(batch, offer, lane, build(nil)); err != nil {
		t.Fatalf("valid signed target failure rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*contracts.RoomFailure, *contracts.TargetFailureReceipt) ed25519.PrivateKey
	}{
		{"origin", func(failure *contracts.RoomFailure, _ *contracts.TargetFailureReceipt) ed25519.PrivateKey {
			failure.Origin = "worker"
			return nil
		}},
		{"code", func(failure *contracts.RoomFailure, _ *contracts.TargetFailureReceipt) ed25519.PrivateKey {
			failure.Code = "target_write_failed"
			return nil
		}},
		{"failure member", func(failure *contracts.RoomFailure, _ *contracts.TargetFailureReceipt) ed25519.PrivateKey {
			failure.TargetMemberID = "member-b"
			return nil
		}},
		{"unknown member", func(failure *contracts.RoomFailure, receipt *contracts.TargetFailureReceipt) ed25519.PrivateKey {
			failure.TargetMemberID, receipt.TargetMemberID = "member-z", "member-z"
			return nil
		}},
		{"transfer", func(_ *contracts.RoomFailure, receipt *contracts.TargetFailureReceipt) ed25519.PrivateKey {
			receipt.TransferID = "transfer-2"
			return nil
		}},
		{"lane", func(_ *contracts.RoomFailure, receipt *contracts.TargetFailureReceipt) ed25519.PrivateKey {
			receipt.LaneID = "lane-2"
			return nil
		}},
		{"chunk outside offer", func(failure *contracts.RoomFailure, receipt *contracts.TargetFailureReceipt) ed25519.PrivateKey {
			failure.ChunkIndices, receipt.ChunkIndex = []int64{3}, 3
			return nil
		}},
		{"chunk not reported", func(failure *contracts.RoomFailure, _ *contracts.TargetFailureReceipt) ed25519.PrivateKey {
			failure.ChunkIndices = []int64{1}
			return nil
		}},
		{"lease", func(_ *contracts.RoomFailure, receipt *contracts.TargetFailureReceipt) ed25519.PrivateKey {
			receipt.LeaseID = "lease-b"
			return nil
		}},
		{"another member's key", func(*contracts.RoomFailure, *contracts.TargetFailureReceipt) ed25519.PrivateKey {
			return otherKey
		}},
		{"with source receipt", func(failure *contracts.RoomFailure, _ *contracts.TargetFailureReceipt) ed25519.PrivateKey {
			failure.SourceFailureReceipt = &contracts.SourceFailureReceipt{Code: "source_file_mutated"}
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if service.verifyReceipts(batch, offer, lane, build(test.mutate)) == nil {
				t.Fatal("mismatched target failure receipt was accepted")
			}
		})
	}
	tampered := build(nil)
	tampered.Failures[0].TargetFailureReceipt.ObservedAt = now.Add(time.Second)
	if service.verifyReceipts(batch, offer, lane, tampered) == nil {
		t.Fatal("tampered target failure receipt was accepted")
	}
}
