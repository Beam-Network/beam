package roomtransfer

import (
	"bytes"
	"context"
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
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
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

func TestTargetFailureRouteAcceptsOnlyTheMembersSignedLeaseReceipt(t *testing.T) {
	now := time.Now().UTC()
	_, sourceKey, _ := ed25519.GenerateKey(rand.Reader)
	_, keyA, _ := ed25519.GenerateKey(rand.Reader)
	_, keyB, _ := ed25519.GenerateKey(rand.Reader)
	transfer := contracts.RoomTransfer{TransferID: "transfer-1", LaneID: "lane-1", Attempt: 1, ChunkStart: 0, ChunkEnd: 1,
		SourceLease: directLease("source-lease", contracts.TunnelLeaseRoleSourceRead, "", sourceKey.Public().(ed25519.PublicKey), now),
		Targets: []contracts.RoomTransferDestination{
			{MemberID: "member-a", Lease: directLease("lease-a", contracts.TunnelLeaseRoleTargetWrite, "member-a", keyA.Public().(ed25519.PublicKey), now)},
			{MemberID: "member-b", Lease: directLease("lease-b", contracts.TunnelLeaseRoleTargetWrite, "member-b", keyB.Public().(ed25519.PublicKey), now)}}}
	active := &session{transfer: transfer, token: "runtime-token", now: func() time.Time { return now },
		chunks: make(map[int64]*chunk), completed: make(map[int64]*completedChunk),
		targetFailures: make(map[string]contracts.TargetFailureReceipt), changed: make(chan struct{}, 1)}
	valid := func(mutate func(*contracts.TargetFailureReceipt)) contracts.TargetFailureReceipt {
		receipt := contracts.TargetFailureReceipt{ReceiptID: "target-failure-1", TransferID: transfer.TransferID, LaneID: transfer.LaneID,
			ChunkIndex: 1, TargetMemberID: "member-a", Code: "target_write_failed", LeaseID: "lease-a", ObservedAt: now}
		if mutate != nil {
			mutate(&receipt)
		}
		return receipt
	}
	encode := func(receipt contracts.TargetFailureReceipt, key ed25519.PrivateKey) []byte {
		signTargetFailure(&receipt, key)
		encoded, _ := json.Marshal(receipt)
		return encoded
	}
	post := func(member, chunk, pathToken string, body []byte) int {
		request := httptest.NewRequest(http.MethodPost, "/targets/"+member+"/failures/"+chunk, bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer runtime-token")
		request.Header.Set("X-Beam-Path-Token", pathToken)
		response := httptest.NewRecorder()
		active.ServeHTTP(response, request, []string{"targets", member, "failures", chunk})
		return response.Code
	}
	for _, test := range []struct {
		name      string
		member    string
		chunk     string
		pathToken string
		body      []byte
		status    int
	}{
		{"another member's path token", "member-a", "1", "path-token-lease-b", encode(valid(nil), keyA), http.StatusForbidden},
		{"chunk outside lane", "member-a", "2", "path-token-lease-a", encode(valid(nil), keyA), http.StatusForbidden},
		{"malformed", "member-a", "1", "path-token-lease-a", []byte("{"), http.StatusBadRequest},
		{"another member's key", "member-a", "1", "path-token-lease-a", encode(valid(nil), keyB), http.StatusBadRequest},
		{"another member's lease", "member-a", "1", "path-token-lease-a",
			encode(valid(func(r *contracts.TargetFailureReceipt) { r.LeaseID = "lease-b" }), keyA), http.StatusBadRequest},
		{"member mismatch", "member-b", "1", "path-token-lease-b", encode(valid(nil), keyB), http.StatusBadRequest},
		{"chunk mismatch", "member-a", "0", "path-token-lease-a", encode(valid(nil), keyA), http.StatusBadRequest},
		{"transfer mismatch", "member-a", "1", "path-token-lease-a",
			encode(valid(func(r *contracts.TargetFailureReceipt) { r.TransferID = "transfer-2" }), keyA), http.StatusBadRequest},
		{"lane mismatch", "member-a", "1", "path-token-lease-a",
			encode(valid(func(r *contracts.TargetFailureReceipt) { r.LaneID = "lane-2" }), keyA), http.StatusBadRequest},
		{"unknown code", "member-a", "1", "path-token-lease-a",
			encode(valid(func(r *contracts.TargetFailureReceipt) { r.Code = "worker_unreachable" }), keyA), http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			if status := post(test.member, test.chunk, test.pathToken, test.body); status != test.status {
				t.Fatalf("status=%d, want %d", status, test.status)
			}
			if len(active.targetFailures) != 0 {
				t.Fatal("rejected target failure was stored")
			}
		})
	}
	if status := post("member-a", "1", "path-token-lease-a", encode(valid(nil), keyA)); status != http.StatusNoContent {
		t.Fatalf("valid target failure status=%d", status)
	}
	if !active.targetFailed("member-a") || active.targetFailed("member-b") || active.allTargetsFailed() {
		t.Fatalf("target failure was not scoped to its member: %+v", active.targetFailures)
	}
	failures := active.withTargetFailures([]contracts.RoomFailure{{Origin: "target_agent", Code: "target_write_failed",
		Retryable: true, TargetMemberID: "member-a", ChunkIndices: []int64{1}}})
	if len(failures) != 1 || failures[0].TargetFailureReceipt == nil || failures[0].TargetFailureReceipt.LeaseID != "lease-a" {
		t.Fatalf("signed failure did not replace the inferred one: %+v", failures)
	}
}

func TestSignedTargetFailureStopsOnlyThatMembersDelivery(t *testing.T) {
	for _, test := range []struct {
		name    string
		members []string
	}{
		{"other members continue", []string{"member-a", "member-b"}},
		{"lane ends when every member failed", []string{"member-a"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			_, sourceKey, _ := ed25519.GenerateKey(rand.Reader)
			keys := map[string]ed25519.PrivateKey{}
			transfer := contracts.RoomTransfer{SchemaVersion: contracts.RoomTransferSchemaVersion,
				BatchID: "batch-1", RoomID: "room-1", ChannelID: "channel-1", PublicationID: "transfer-1",
				TransferID: "transfer-1", SnapshotVersion: 1, LaneID: "lane-1", Attempt: 1,
				Protection:    contracts.RoomTransferProtection{Scheme: contracts.RoomTransferProtectionScheme, KeyEpoch: 1},
				FileSizeBytes: 12, ChunkSizeBytes: 6, ChunkCount: 2, ChunkStart: 0, ChunkEnd: 1,
				SourceLease: directLease("source-lease", contracts.TunnelLeaseRoleSourceRead, "", sourceKey.Public().(ed25519.PublicKey), now)}
			for _, member := range test.members {
				_, key, _ := ed25519.GenerateKey(rand.Reader)
				keys[member] = key
				transfer.Targets = append(transfer.Targets, contracts.RoomTransferDestination{MemberID: member,
					Lease: directLease("lease-"+member, contracts.TunnelLeaseRoleTargetWrite, member, key.Public().(ed25519.PublicKey), now)})
			}
			payload, _ := json.Marshal(transfer)
			handler := NewHandler(Config{ListenAddress: "127.0.0.1:0"})
			defer handler.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			runtimes := make(chan contracts.DirectRoomTransferRuntime, 1)
			ctx = workloadprogress.WithReporter(ctx, func(outputs map[string]string) {
				var runtime contracts.DirectRoomTransferRuntime
				if json.Unmarshal([]byte(outputs["room_transfer_runtime"]), &runtime) == nil {
					runtimes <- runtime
				}
			})
			type outcome struct {
				result domain.Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := handler.Execute(ctx, domain.Spec{Payload: payload, Identity: domain.Identity{WorkerID: "worker-1"},
					Resources: domain.Resources{MemoryBytes: 96 << 20}})
				done <- outcome{result, err}
			}()
			var runtime contracts.DirectRoomTransferRuntime
			select {
			case runtime = <-runtimes:
			case <-ctx.Done():
				t.Fatal("worker did not publish its room runtime")
			}
			// until retries 425 responses, as agents do while the Worker is not ready.
			until := func(want int, method, path, pathToken string, header map[string]string, body []byte) {
				for {
					request, _ := http.NewRequestWithContext(ctx, method, runtime.BaseURL+path, bytes.NewReader(body))
					request.Header.Set("Authorization", "Bearer "+runtime.AccessToken)
					request.Header.Set("X-Beam-Path-Token", pathToken)
					for name, value := range header {
						request.Header.Set(name, value)
					}
					response, err := http.DefaultClient.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					content, _ := io.ReadAll(response.Body)
					response.Body.Close()
					if response.StatusCode == want {
						return
					}
					if response.StatusCode != http.StatusTooEarly || ctx.Err() != nil {
						t.Fatalf("%s %s status=%d body=%s", method, path, response.StatusCode, content)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			sourceHashes := map[int64]string{}
			sendSource := func(index int64) {
				plaintext := []byte(fmt.Sprintf("chunk%d", index))
				protected := make([]byte, len(plaintext)+protectedChunkOverhead)
				protected[0] = 1
				binary.BigEndian.PutUint64(protected[1:9], transfer.Protection.KeyEpoch)
				copy(protected[9:], plaintext)
				receipt := contracts.SourceRangeReceipt{ReceiptID: fmt.Sprintf("source-%d", index), TransferID: transfer.TransferID,
					LaneID: transfer.LaneID, ChunkIndex: index, Offset: index * transfer.ChunkSizeBytes, Length: int64(len(plaintext)),
					RangeSHA256: digest(protected), LeaseID: transfer.SourceLease.LeaseID, CompletedAt: now}
				signSourceReceipt(&receipt, sourceKey)
				sourceHashes[index] = receipt.RangeSHA256
				encoded, _ := json.Marshal(receipt)
				until(http.StatusNoContent, http.MethodPost, fmt.Sprintf("/source/chunks/%d", index), "path-token-source-lease",
					map[string]string{"X-Beam-Source-Receipt": base64.RawURLEncoding.EncodeToString(encoded)}, protected)
			}
			deliver := func(member string, index int64) {
				path := fmt.Sprintf("/targets/%s/chunks/%d", member, index)
				until(http.StatusOK, http.MethodGet, path, "path-token-lease-"+member, nil, nil)
				receipt := contracts.TargetRangeReceipt{TargetMemberID: member, SourceRangeReceipt: contracts.SourceRangeReceipt{
					ReceiptID: fmt.Sprintf("target-%s-%d", member, index), TransferID: transfer.TransferID, LaneID: transfer.LaneID,
					ChunkIndex: index, Offset: index * transfer.ChunkSizeBytes, Length: 6, RangeSHA256: sourceHashes[index],
					LeaseID: "lease-" + member, CompletedAt: now}}
				signTargetRangeReceipt(&receipt, keys[member])
				encoded, _ := json.Marshal(targetResponse{RangeReceipt: receipt})
				until(http.StatusNoContent, http.MethodPost, path+"/receipt", "path-token-lease-"+member, nil, encoded)
			}
			failure := contracts.TargetFailureReceipt{ReceiptID: "target-failure-a", TransferID: transfer.TransferID,
				LaneID: transfer.LaneID, ChunkIndex: 0, TargetMemberID: "member-a", Code: "target_storage_full",
				LeaseID: "lease-member-a", ObservedAt: now}
			signTargetFailure(&failure, keys["member-a"])
			encodedFailure, _ := json.Marshal(failure)

			sendSource(0)
			until(http.StatusNoContent, http.MethodPost, "/targets/member-a/failures/0", "path-token-lease-member-a", nil, encodedFailure)
			if len(test.members) > 1 {
				deliver("member-b", 0)
				sendSource(1)
				deliver("member-b", 1)
			}
			var finished outcome
			select {
			case finished = <-done:
			case <-ctx.Done():
				t.Fatal("lane kept waiting for a member that signed a persist failure")
			}
			if finished.err != nil {
				t.Fatal(finished.err)
			}
			var failures []contracts.RoomFailure
			var receipts []contracts.TargetRangeReceipt
			var missing []contracts.RoomMissingCells
			for key, destination := range map[string]any{"room_failures": &failures, "target_receipts": &receipts, "missing": &missing} {
				if err := json.Unmarshal([]byte(finished.result.Outputs[key]), destination); err != nil {
					t.Fatalf("decode %s: %v", key, err)
				}
			}
			if len(failures) != 1 || failures[0].Origin != "target_agent" || failures[0].Code != "target_storage_full" ||
				!failures[0].Retryable || failures[0].TargetMemberID != "member-a" || len(failures[0].ChunkIndices) != 1 ||
				failures[0].ChunkIndices[0] != 0 || failures[0].TargetFailureReceipt == nil ||
				failures[0].TargetFailureReceipt.Verify(transfer.Targets[0].Lease.AgentPublicKey, now) != nil {
				t.Fatalf("lane result lacks the signed target failure: %+v", failures)
			}
			if len(receipts) != 2*(len(test.members)-1) {
				t.Fatalf("target receipts=%+v", receipts)
			}
			for _, receipt := range receipts {
				if receipt.TargetMemberID != "member-b" {
					t.Fatalf("failed member was still delivered: %+v", receipt)
				}
			}
			if len(missing) != 1 || missing[0].TargetMemberID != "member-a" || len(missing[0].ChunkIndices) != 2 {
				t.Fatalf("missing=%+v", missing)
			}
		})
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

func signTargetRangeReceipt(receipt *contracts.TargetRangeReceipt, privateKey ed25519.PrivateKey) {
	receipt.AgentPublicKey = base64.RawURLEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey))
	fields := []string{receipt.ReceiptID, receipt.TransferID, receipt.LaneID, fmt.Sprint(receipt.ChunkIndex),
		fmt.Sprint(receipt.Offset), fmt.Sprint(receipt.Length), receipt.RangeSHA256, receipt.LeaseID,
		receipt.CompletedAt.Format(time.RFC3339Nano), receipt.AgentPublicKey, receipt.TargetMemberID}
	message := []byte("beam:room-target-range-receipt\x00" + strings.Join(fields, "\n"))
	receipt.AgentSignature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))
}

func signTargetFailure(receipt *contracts.TargetFailureReceipt, privateKey ed25519.PrivateKey) {
	receipt.AgentPublicKey = base64.RawURLEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey))
	fields := []string{receipt.ReceiptID, receipt.TransferID, receipt.LaneID, fmt.Sprint(receipt.ChunkIndex),
		receipt.TargetMemberID, receipt.Code, receipt.LeaseID, receipt.ObservedAt.Format(time.RFC3339Nano), receipt.AgentPublicKey}
	message := []byte("beam:room-target-failure-receipt\x00" + strings.Join(fields, "\n"))
	receipt.AgentSignature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))
}

func directLease(id, role, member string, key ed25519.PublicKey, now time.Time) contracts.TunnelLease {
	return contracts.TunnelLease{LeaseID: id, IntentID: "intent-" + id, Role: role, TargetMemberID: member,
		Protocol: contracts.RoomTransferDirectCapability, AgentPublicKey: base64.RawURLEncoding.EncodeToString(key),
		Endpoints: []contracts.TunnelLeaseEndpoint{{URL: "https://worker.room.invalid/v1/room-transfers/transfer-1",
			Headers: map[string]string{"X-Beam-Path-Token": "path-token-" + id}}}, ExpiresAt: now.Add(time.Hour)}
}
