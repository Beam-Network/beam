package roomtransfer

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestValidateDirectRoomTransfer(t *testing.T) {
	now := time.Now().UTC()
	_, sourceKey, _ := ed25519.GenerateKey(rand.Reader)
	_, targetKey, _ := ed25519.GenerateKey(rand.Reader)
	transfer := contracts.RoomTransfer{SchemaVersion: contracts.RoomTransferSchemaVersion,
		BatchID: "batch-1", RoomID: "room-1", ChannelID: "channel-1", PublicationID: "transfer-1",
		TransferID: "transfer-1", SnapshotVersion: 1, LaneID: "lane-1", Attempt: 1,
		Protection:    contracts.RoomTransferProtection{Scheme: contracts.RoomTransferProtectionScheme, KeyEpoch: 1},
		FileSizeBytes: 12, ChunkSizeBytes: 6, ChunkCount: 2, ChunkStart: 0, ChunkEnd: 1,
		SourceLease: directLease("source-lease", contracts.TunnelLeaseRoleSourceRead, "", sourceKey.Public().(ed25519.PublicKey), now),
		Targets: []contracts.RoomTransferDestination{{MemberID: "member-b",
			Lease: directLease("target-lease", contracts.TunnelLeaseRoleTargetWrite, "member-b", targetKey.Public().(ed25519.PublicKey), now)}}}
	payload, _ := json.Marshal(transfer)
	handler := NewHandler(Config{ListenAddress: "127.0.0.1:0", AdvertiseURL: "http://worker.example:9470"})
	if err := handler.Validate(domain.Spec{Payload: payload, Resources: domain.Resources{MemoryBytes: 96 << 20}}); err != nil {
		t.Fatal(err)
	}
	transfer.FileSizeBytes, transfer.ChunkSizeBytes = 100<<20, (128<<20)+(1<<20)
	transfer.ChunkCount, transfer.ChunkEnd = 1, 0
	payload, _ = json.Marshal(transfer)
	if err := handler.Validate(domain.Spec{Payload: payload, Resources: domain.Resources{MemoryBytes: 132 << 20}}); err != nil {
		t.Fatalf("provider floor plus jitter rejected: %v", err)
	}
	if err := handler.Validate(domain.Spec{Payload: payload, Resources: domain.Resources{MemoryBytes: 131 << 20}}); err == nil {
		t.Fatal("undersized source buffer reservation accepted")
	}
	transfer.SourceLease.Protocol = contracts.RoomTransferCapability
	payload, _ = json.Marshal(transfer)
	if err := handler.Validate(domain.Spec{Payload: payload, Resources: domain.Resources{MemoryBytes: 132 << 20}}); err == nil {
		t.Fatal("legacy room transfer protocol was accepted")
	}
}

func TestDirectSessionBackpressuresAndAcceptsCompletedSourceRetry(t *testing.T) {
	now := time.Now().UTC()
	_, sourceKey, _ := ed25519.GenerateKey(rand.Reader)
	_, targetKey, _ := ed25519.GenerateKey(rand.Reader)
	transfer := contracts.RoomTransfer{SchemaVersion: contracts.RoomTransferSchemaVersion,
		BatchID: "batch-1", RoomID: "room-1", ChannelID: "channel-1", PublicationID: "transfer-1",
		TransferID: "transfer-1", SnapshotVersion: 1, LaneID: "lane-1", Attempt: 1,
		Protection:    contracts.RoomTransferProtection{Scheme: contracts.RoomTransferProtectionScheme, KeyEpoch: 1},
		FileSizeBytes: 12, ChunkSizeBytes: 6, ChunkCount: 2, ChunkStart: 0, ChunkEnd: 1,
		SourceLease: directLease("source-lease", contracts.TunnelLeaseRoleSourceRead, "", sourceKey.Public().(ed25519.PublicKey), now),
		Targets: []contracts.RoomTransferDestination{{MemberID: "member-b",
			Lease: directLease("target-lease", contracts.TunnelLeaseRoleTargetWrite, "member-b", targetKey.Public().(ed25519.PublicKey), now)}}}
	active := &session{transfer: transfer, token: "runtime-token", now: func() time.Time { return now },
		chunks: make(map[int64]*chunk), completed: make(map[int64]*completedChunk), expected: 0, changed: make(chan struct{}, 1)}

	post := func(index int64, payload []byte, readBody bool) *httptest.ResponseRecorder {
		plaintextLength := int64(len(payload))
		protected := make([]byte, len(payload)+protectedChunkOverhead)
		protected[0] = 1
		binary.BigEndian.PutUint64(protected[1:9], transfer.Protection.KeyEpoch)
		copy(protected[9:], payload)
		receipt := contracts.SourceRangeReceipt{ReceiptID: fmt.Sprintf("receipt-%d", index), TransferID: transfer.TransferID,
			LaneID: transfer.LaneID, ChunkIndex: index, Offset: index * transfer.ChunkSizeBytes, Length: plaintextLength,
			RangeSHA256: digest(protected), LeaseID: transfer.SourceLease.LeaseID, CompletedAt: now}
		signSourceReceipt(&receipt, sourceKey)
		encoded, _ := json.Marshal(receipt)
		body := bytes.NewReader(protected)
		request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/source/chunks/%d", index), body)
		request.Header.Set("Authorization", "Bearer runtime-token")
		request.Header.Set("X-Beam-Path-Token", "path-token-source-lease")
		request.Header.Set("X-Beam-Source-Receipt", base64.RawURLEncoding.EncodeToString(encoded))
		response := httptest.NewRecorder()
		active.ServeHTTP(response, request, []string{"source", "chunks", fmt.Sprint(index)})
		if readBody != (body.Len() == 0) {
			t.Fatalf("chunk %d consumed %d body bytes, want readBody=%v", index, len(protected)-body.Len(), readBody)
		}
		// Exercise the real HTTP admission handshake, not only the handler.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			active.ServeHTTP(w, r, []string{"source", "chunks", fmt.Sprint(index)})
		}))
		defer server.Close()
		body = bytes.NewReader(protected)
		retry, _ := http.NewRequest(http.MethodPost, server.URL, io.NopCloser(body))
		retry.ContentLength = int64(len(protected))
		retry.Header = request.Header.Clone()
		retry.Header.Set("Expect", "100-continue")
		transport := http.DefaultTransport.(*http.Transport).Clone()
		defer transport.CloseIdleConnections()
		result, err := (&http.Client{Transport: transport}).Do(retry)
		if err != nil {
			t.Fatal(err)
		}
		result.Body.Close()
		if body.Len() != len(protected) || result.StatusCode != response.Code {
			t.Fatalf("retry sent %d bytes, status=%d", len(protected)-body.Len(), result.StatusCode)
		}
		return response
	}
	if response := post(1, []byte("ghijkl"), false); response.Code != http.StatusTooEarly || len(active.chunks) != 0 {
		t.Fatalf("future chunk status=%d buffered=%d", response.Code, len(active.chunks))
	}
	if response := post(0, []byte("abcdef"), true); response.Code != http.StatusNoContent || len(active.chunks) != 1 {
		t.Fatalf("current chunk status=%d buffered=%d", response.Code, len(active.chunks))
	}
	active.release(0)
	if response := post(0, []byte("abcdef"), false); response.Code != http.StatusNoContent || len(active.chunks) != 0 {
		t.Fatalf("completed retry status=%d buffered=%d", response.Code, len(active.chunks))
	}
}

func digest(payload []byte) string {
	value := sha256.Sum256(payload)
	return hex.EncodeToString(value[:])
}

func signSourceReceipt(receipt *contracts.SourceRangeReceipt, privateKey ed25519.PrivateKey) {
	receipt.AgentPublicKey = base64.RawURLEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey))
	fields := []string{receipt.ReceiptID, receipt.TransferID, receipt.LaneID, fmt.Sprint(receipt.ChunkIndex),
		fmt.Sprint(receipt.Offset), fmt.Sprint(receipt.Length), receipt.RangeSHA256, receipt.LeaseID,
		receipt.CompletedAt.Format(time.RFC3339Nano), receipt.AgentPublicKey}
	message := []byte("beam:room-source-range-receipt\x00" + strings.Join(fields, "\n"))
	receipt.AgentSignature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))
}

func directLease(id, role, member string, key ed25519.PublicKey, now time.Time) contracts.TunnelLease {
	return contracts.TunnelLease{LeaseID: id, IntentID: "intent-" + id, Role: role, TargetMemberID: member,
		Protocol: contracts.RoomTransferDirectCapability, AgentPublicKey: base64.RawURLEncoding.EncodeToString(key),
		Endpoints: []contracts.TunnelLeaseEndpoint{{URL: "https://worker.room.invalid/v1/room-transfers/transfer-1",
			Headers: map[string]string{"X-Beam-Path-Token": "path-token-" + id}}}, ExpiresAt: now.Add(time.Hour)}
}
