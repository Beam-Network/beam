package contracts

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTargetFailureReceiptVerifiesAndRejectsTampering(t *testing.T) {
	now := time.Now().UTC()
	_, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey))
	sign := func(receipt *TargetFailureReceipt, signer ed25519.PrivateKey) {
		receipt.AgentPublicKey = base64.RawURLEncoding.EncodeToString(signer.Public().(ed25519.PublicKey))
		message := "beam:room-target-failure-receipt\x00" + strings.Join([]string{receipt.ReceiptID, receipt.TransferID,
			receipt.LaneID, fmt.Sprint(receipt.ChunkIndex), receipt.TargetMemberID, receipt.Code, receipt.LeaseID,
			receipt.ObservedAt.Format(time.RFC3339Nano), receipt.AgentPublicKey}, "\n")
		receipt.AgentSignature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(message)))
	}
	valid := TargetFailureReceipt{ReceiptID: "target-failure-1", TransferID: "transfer-1", LaneID: "lane-1", ChunkIndex: 3,
		TargetMemberID: "member-b", Code: "target_storage_full", LeaseID: "target-lease", ObservedAt: now.Add(-time.Second)}
	sign(&valid, privateKey)
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TargetFailureReceipt
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Verify(key, now); err != nil {
		t.Fatalf("signed receipt did not survive the wire: %v", err)
	}
	for _, field := range []string{"receipt_id", "transfer_id", "lane_id", "chunk_index", "target_member_id", "code",
		"lease_id", "observed_at", "agent_public_key", "agent_signature"} {
		if !strings.Contains(string(encoded), `"`+field+`":`) {
			t.Fatalf("wire receipt lacks %s: %s", field, encoded)
		}
	}
	for _, test := range []struct {
		name   string
		resign bool
		mutate func(*TargetFailureReceipt)
	}{
		{"receipt", false, func(value *TargetFailureReceipt) { value.ReceiptID = "target-failure-2" }},
		{"transfer", false, func(value *TargetFailureReceipt) { value.TransferID = "transfer-2" }},
		{"lane", false, func(value *TargetFailureReceipt) { value.LaneID = "lane-2" }},
		{"chunk", false, func(value *TargetFailureReceipt) { value.ChunkIndex = 4 }},
		{"member", false, func(value *TargetFailureReceipt) { value.TargetMemberID = "member-c" }},
		{"code", false, func(value *TargetFailureReceipt) { value.Code = "target_write_failed" }},
		{"lease", false, func(value *TargetFailureReceipt) { value.LeaseID = "other-lease" }},
		{"observed", false, func(value *TargetFailureReceipt) { value.ObservedAt = value.ObservedAt.Add(time.Nanosecond) }},
		{"signature", false, func(value *TargetFailureReceipt) {
			signature, _ := base64.RawURLEncoding.DecodeString(value.AgentSignature)
			signature[0] ^= 1
			value.AgentSignature = base64.RawURLEncoding.EncodeToString(signature)
		}},
		{"other signer", true, func(*TargetFailureReceipt) {}},
		{"unknown code", true, func(value *TargetFailureReceipt) { value.Code = "target_disconnected" }},
		{"negative chunk", true, func(value *TargetFailureReceipt) { value.ChunkIndex = -1 }},
		{"missing member", true, func(value *TargetFailureReceipt) { value.TargetMemberID = "" }},
		{"future", true, func(value *TargetFailureReceipt) { value.ObservedAt = now.Add(6 * time.Minute) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if test.resign {
				signer := privateKey
				if test.name == "other signer" {
					signer = otherKey
				}
				sign(&candidate, signer)
			}
			if candidate.Verify(key, now) == nil {
				t.Fatal("tampered target failure receipt was accepted")
			}
		})
	}
	nearFuture := valid
	nearFuture.ObservedAt = now.Add(4 * time.Minute)
	sign(&nearFuture, privateKey)
	if err := nearFuture.Verify(key, now); err != nil {
		t.Fatalf("receipt within clock skew was rejected: %v", err)
	}
}

func TestSignedIntentDeadlineSurvivesForwarding(t *testing.T) {
	for _, deadline := range []string{"2030-01-01T00:00:00.000Z", "2030-01-01T00:00:00.070Z", "2030-01-01T00:00:00.530Z", "2030-01-01T00:00:00.831Z"} {
		t.Run(deadline, func(t *testing.T) {
			wire := map[string]any{"intent_id": "intent-1", "transfer_id": "transfer-1", "lane_id": "lane-1",
				"attempt": 1, "role": TunnelLeaseRoleSourceRead, "chunk_start": 0, "chunk_end": 0,
				"orchestrator_id": "orchestrator-1", "required_worker_capability": RoomTransferE2EECapability,
				"expires_at": deadline, "signature": "opaque-signature"}
			encoded, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			var intent TunnelLeaseIntent
			if err := json.Unmarshal(encoded, &intent); err != nil {
				t.Fatal(err)
			}
			lane := RoomSourceLane{LaneID: "lane-1", Attempt: 1, ChunkStart: 0, ChunkEnd: 0}
			now := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
			if err := intent.Validate(TunnelLeaseRoleSourceRead, "", lane, now); err != nil {
				t.Fatal(err)
			}
			forwarded, err := json.Marshal(intent)
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(forwarded, &result); err != nil {
				t.Fatal(err)
			}
			if result["expires_at"] != deadline || result["signature"] != wire["signature"] {
				t.Fatal("signed intent changed during forwarding")
			}
			expiry, err := intent.Deadline()
			if err != nil {
				t.Fatal(err)
			}
			if intent.Validate(TunnelLeaseRoleSourceRead, "", lane, expiry) == nil {
				t.Fatal("expired intent accepted")
			}
			intent.ExpiresAt = "not-a-deadline"
			if intent.Validate(TunnelLeaseRoleSourceRead, "", lane, now) == nil {
				t.Fatal("invalid deadline accepted")
			}
		})
	}
}
