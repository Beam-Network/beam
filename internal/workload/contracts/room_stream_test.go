package contracts

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamDefinitionDetailsFollowTheDirectBounds(t *testing.T) {
	valid := StreamDefinitionDetails{SessionID: "session-1", Protocol: RoomStreamProtectionProtocol, BackpressurePolicy: "drop_oldest",
		MaxBufferBytes: 1 << 20, HeartbeatTimeoutMS: 30_000}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bound := range []int64{RoomStreamMinHeartbeatTimeoutMS, RoomStreamMaxHeartbeatTimeoutMS} {
		value := valid
		value.HeartbeatTimeoutMS = bound
		if err := value.Validate(); err != nil {
			t.Fatalf("heartbeat bound %d rejected: %v", bound, err)
		}
	}
	for _, bound := range []int64{RoomStreamMinBufferBytes, RoomStreamMaxBufferBytes} {
		value := valid
		value.MaxBufferBytes = bound
		if err := value.Validate(); err != nil {
			t.Fatalf("buffer bound %d rejected: %v", bound, err)
		}
	}
	cases := map[string]func(*StreamDefinitionDetails){
		"heartbeat below range": func(value *StreamDefinitionDetails) { value.HeartbeatTimeoutMS = 2_999 },
		"heartbeat above range": func(value *StreamDefinitionDetails) { value.HeartbeatTimeoutMS = 300_001 },
		"buffer below range":    func(value *StreamDefinitionDetails) { value.MaxBufferBytes = RoomStreamMinBufferBytes - 1 },
		"buffer above range":    func(value *StreamDefinitionDetails) { value.MaxBufferBytes = RoomStreamMaxBufferBytes + 1 },
		"drop_newest":           func(value *StreamDefinitionDetails) { value.BackpressurePolicy = "drop_newest" },
		"plaintext":             func(value *StreamDefinitionDetails) { value.Protocol = "raw" },
		"missing session":       func(value *StreamDefinitionDetails) { value.SessionID = "" },
		"missing heartbeat":     func(value *StreamDefinitionDetails) { value.HeartbeatTimeoutMS = 0 },
	}
	for name, mutate := range cases {
		value := valid
		mutate(&value)
		if err := value.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	unit := StreamUnitDetails{SessionID: "session-1", Protocol: RoomStreamProtectionProtocol, BackpressurePolicy: "block",
		MaxBufferBytes: 1 << 20, HeartbeatTimeoutMS: 30_000}
	if err := unit.Validate(); err != nil {
		t.Fatal(err)
	}
	unit.HeartbeatTimeoutMS = 0
	if err := unit.Validate(); err == nil {
		t.Fatal("stream unit details without heartbeat_timeout_ms were accepted")
	}
}

func TestRoomWorkerAccessTokensAreCanonicalBase64URL(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))
	if !ValidRoomWorkerAccessToken(token) {
		t.Fatalf("canonical token %q rejected", token)
	}
	for _, invalid := range []string{token[:42], token + "A", strings.Repeat("S", 43), strings.Repeat("+", 43), ""} {
		if ValidRoomWorkerAccessToken(invalid) {
			t.Fatalf("token %q accepted", invalid)
		}
	}
}

func TestStreamDetailsEncodeTheirWireShapes(t *testing.T) {
	encoded, err := json.Marshal(StreamResultDetails{TerminalReason: RoomStreamWorkerFailed})
	if err != nil || string(encoded) != `{"sequence":0,"bytes":0,"buffered_bytes":0,"dropped":0,"targets":[],"terminal_reason":"worker_failed"}` {
		t.Fatalf("result %s err=%v", encoded, err)
	}
	encoded, err = json.Marshal(StreamProgressDetails{Sequence: 3, Targets: []RoomStreamTargetState{{TargetMemberID: "bob", State: "active", DeliveredThrough: 3}}})
	if err != nil || string(encoded) != `{"sequence":3,"bytes":0,"buffered_bytes":0,"dropped":0,"targets":[{"target_member_id":"bob","state":"active","delivered_through":3,"reason":null}]}` {
		t.Fatalf("counters progress %s err=%v", encoded, err)
	}
	encoded, err = json.Marshal(StreamProgressDetails{Sequence: 3, Runtime: &RoomStreamRuntime{SchemaVersion: RoomStreamRuntimeSchema}})
	if err != nil || !strings.HasPrefix(string(encoded), `{"runtime":{"schema_version":"room-stream-runtime/v1"`) || strings.Contains(string(encoded), `"sequence"`) {
		t.Fatalf("runtime progress %s err=%v", encoded, err)
	}
}
