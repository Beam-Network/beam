package contracts

import (
	"encoding/json"
	"testing"
	"time"
)

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
