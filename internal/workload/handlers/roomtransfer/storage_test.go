package roomtransfer

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestStorageFanoutReadsEachRangeOnceAndRetainsBufferAcrossDestinationRetry(t *testing.T) {
	now := time.Now().UTC()
	content := bytes.Repeat([]byte("a source range shared by all recipients\n"), 2000)
	chunkSize := int64(16384)
	count := (int64(len(content)) + chunkSize - 1) / chunkSize
	var mu sync.Mutex
	reads := map[int64]int{}
	writes := map[string]int{}
	var baseURL string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/routes/") {
			var request contracts.StorageRouteRequest
			if json.NewDecoder(r.Body).Decode(&request) != nil || request.WorkerID != "worker-1" || request.Attempt != 1 || r.Header.Get("X-Beam-Path-Token") != "scoped-token" {
				http.Error(w, "unauthorized", http.StatusForbidden)
				return
			}
			index := request.ChunkIndex
			offset, length := index*chunkSize, min(chunkSize, int64(len(content))-index*chunkSize)
			role := strings.TrimPrefix(r.URL.Path, "/routes/")
			method, path := "PUT", "/destination/"+role+"/"+fmt.Sprint(index)
			if role == "source" {
				method, path = "GET", "/source/"+fmt.Sprint(index)
			}
			_ = json.NewEncoder(w).Encode(contracts.StorageRoute{ChunkIndex: index, Offset: offset, Length: length,
				ExpiresAt: now.Add(5 * time.Minute), Endpoint: contracts.HTTPEndpoint{URL: baseURL + path, Method: method, Headers: map[string]string{"Content-MD5": request.ContentMD5}}, PartNumber: contracts.MultipartAttemptPartNumber(index, request.Attempt), UploadID: "upload-" + role})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/source/") {
			index, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/source/"), 10, 64)
			offset, length := index*chunkSize, min(chunkSize, int64(len(content))-index*chunkSize)
			if r.Header.Get("If-Match") != `"frozen"` || r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", offset, offset+length-1) {
				http.Error(w, "source condition missing", 412)
				return
			}
			mu.Lock()
			reads[index]++
			mu.Unlock()
			w.Header().Set("ETag", `"frozen"`)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, len(content)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[offset : offset+length])
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/destination/"), "/")
		index, _ := strconv.ParseInt(parts[1], 10, 64)
		offset, length := index*chunkSize, min(chunkSize, int64(len(content))-index*chunkSize)
		payload, _ := io.ReadAll(r.Body)
		digest := md5.Sum(payload)
		if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(digest[:]) {
			t.Error("provider checksum is not bound to the source payload")
		}
		if !bytes.Equal(payload, content[offset:offset+length]) {
			t.Error("destination payload mismatch")
			http.Error(w, "mismatch", 400)
			return
		}
		mu.Lock()
		writes[r.URL.Path]++
		attempt := writes[r.URL.Path]
		mu.Unlock()
		if parts[0] == "member-7" && attempt == 1 {
			http.Error(w, "retryable", 503)
			return
		}
		w.Header().Set("ETag", `"part"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	baseURL = server.URL
	lease := func(member, role string) contracts.TunnelLease {
		return contracts.TunnelLease{LeaseID: member, IntentID: member, Role: role, Protocol: contracts.RoomStorageCapability,
			ExpiresAt: now.Add(10 * time.Minute), Storage: &contracts.StorageLease{MemberID: member, ResourceID: member, SizeBytes: int64(len(content)), ETag: `"frozen"`},
			Endpoints: []contracts.TunnelLeaseEndpoint{{URL: baseURL + "/routes/" + member, Method: "POST", Headers: map[string]string{"X-Beam-Path-Token": "scoped-token"}}}}
	}
	transfer := contracts.RoomTransfer{SchemaVersion: contracts.RoomStorageSchemaVersion, BatchID: "batch", RoomID: "room", ChannelID: "channel",
		PublicationID: "publication", TransferID: "publication", SnapshotVersion: 1, LaneID: "lane", Attempt: 1,
		Protection: contracts.RoomTransferProtection{Scheme: contracts.RoomStorageProtectionScheme}, FileSizeBytes: int64(len(content)),
		ChunkSizeBytes: chunkSize, ChunkCount: count, ChunkStart: 0, ChunkEnd: count - 1, SourceLease: lease("source", contracts.TunnelLeaseRoleSourceRead)}
	for index := 0; index < 101; index++ {
		id := fmt.Sprintf("member-%d", index)
		targetLease := lease(id, contracts.TunnelLeaseRoleTargetWrite)
		targetLease.TargetMemberID = id
		transfer.Targets = append(transfer.Targets, contracts.RoomTransferDestination{MemberID: id, Lease: targetLease})
	}
	handler := NewHandler(Config{StorageListenAddress: "127.0.0.1:0", StorageAdvertiseURL: "https://127.0.0.1:{port}"})
	handler.storageClient = server.Client()
	handler.storageClient.CheckRedirect = storageHTTPClient().CheckRedirect
	t.Cleanup(func() {
		if handler.storageServer != nil {
			_ = handler.storageServer.http.Close()
		}
	})
	payload, _ := json.Marshal(transfer)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := handler.Execute(ctx, domain.Spec{Payload: payload, Identity: domain.Identity{WorkerID: "worker-1"}, Resources: domain.Resources{MemoryBytes: 96 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outputs["room_failures"] != "null" || result.Outputs["missing"] != "[]" {
		t.Fatalf("unexpected failure: %v", result.Outputs)
	}
	if result.BytesProcessed != int64(len(content)) {
		t.Fatalf("source bytes=%d, expected=%d", result.BytesProcessed, len(content))
	}
	var measured []contracts.SourceReadEvidence
	if err := json.Unmarshal([]byte(result.Outputs["source_reads"]), &measured); err != nil || len(measured) != int(count) {
		t.Fatalf("missing source measurements: %v", err)
	}
	for _, read := range measured {
		_, length := contracts.ChunkRange(transfer.FileSizeBytes, chunkSize, read.ChunkIndex)
		if read.ReadCount != int64(reads[read.ChunkIndex]) || read.ReadCount != 1 || read.PayloadBytes != length || read.WireBytes != length {
			t.Fatalf("reported source measurement disagrees with HTTP reads: %+v", read)
		}
	}
	for index := int64(0); index < count; index++ {
		if reads[index] != 1 {
			t.Fatalf("source chunk %d read %d times", index, reads[index])
		}
	}
	if len(writes) != 101*int(count) {
		t.Fatalf("destination coverage=%d", len(writes))
	}
	var evidence []contracts.StorageRangeResult
	if err := json.Unmarshal([]byte(result.Outputs["storage_results"]), &evidence); err != nil || len(evidence) != 102*int(count) {
		t.Fatalf("missing storage evidence: %v", err)
	}
	if strings.Contains(result.Outputs["storage_results"], baseURL) || strings.Contains(result.Outputs["storage_results"], "scoped-token") {
		t.Fatal("transient route leaked into outputs")
	}
}
func TestPendingAgentsDoNotBlockStorageAndCoalescedReceiptsComplete(t *testing.T) {
	now := time.Now().UTC()
	payload := []byte("one retained source payload")
	stored := make(chan struct{}, 1)
	var baseURL string
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/route" {
			var request contracts.StorageRouteRequest
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				t.Error("invalid route request")
				return
			}
			_ = json.NewEncoder(w).Encode(contracts.StorageRoute{ChunkIndex: 0, Length: int64(len(payload)), ExpiresAt: now.Add(time.Minute),
				PartNumber: 1, UploadID: "upload", Endpoint: contracts.HTTPEndpoint{URL: baseURL + "/part", Method: "PUT", Headers: map[string]string{"Content-MD5": request.ContentMD5}}})
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, payload) {
			t.Error("payload changed")
		}
		w.Header().Set("ETag", `"part"`)
		stored <- struct{}{}
	}))
	defer provider.Close()
	baseURL = provider.URL
	target := contracts.RoomTransferDestination{MemberID: "bucket", Lease: contracts.TunnelLease{
		LeaseID: "bucket", IntentID: "bucket", Role: contracts.TunnelLeaseRoleTargetWrite, TargetMemberID: "bucket", Protocol: contracts.RoomStorageCapability,
		ExpiresAt: now.Add(2 * time.Minute), Storage: &contracts.StorageLease{MemberID: "bucket", ResourceID: "bucket", SizeBytes: int64(len(payload))},
		Endpoints: []contracts.TunnelLeaseEndpoint{{URL: baseURL + "/route", Method: "POST", Headers: map[string]string{"X-Beam-Path-Token": "token"}}}}}
	transfer := contracts.RoomTransfer{SchemaVersion: contracts.RoomStorageSchemaVersion, TransferID: "publication", LaneID: "lane", Attempt: 1,
		FileSizeBytes: int64(len(payload)), ChunkSizeBytes: int64(len(payload)), ChunkCount: 1, ChunkStart: 0, ChunkEnd: 0}
	for i := 0; i < 9; i++ {
		transfer.Targets = append(transfer.Targets, contracts.RoomTransferDestination{MemberID: fmt.Sprint("agent-", i)})
	}
	transfer.Targets = append(transfer.Targets, target)
	current := &chunk{payload: payload, targets: map[string]contracts.TargetRangeReceipt{}, finals: map[string]contracts.FinalTargetReceipt{}}
	active := &session{transfer: transfer, chunks: map[int64]*chunk{0: current}, changed: make(chan struct{}, 1)}
	handler := NewHandler(Config{})
	handler.storageClient = provider.Client()
	resume := checkpointValue{}
	initializeCheckpoint(&resume)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan []contracts.RoomFailure, 1)
	go func() {
		failures, err := handler.deliverChunk(ctx, "worker", active, current, 0, &resume)
		if err != nil {
			t.Error(err)
		}
		done <- failures
	}()
	select {
	case <-stored:
	case <-ctx.Done():
		t.Fatal("pending agents blocked the storage upload")
	}
	// All nine acknowledgements use one coalesced notification, as a burst does.
	active.mu.Lock()
	for _, target := range transfer.Targets[:9] {
		current.targets[target.MemberID] = contracts.TargetRangeReceipt{TargetMemberID: target.MemberID}
	}
	active.signalLocked()
	active.mu.Unlock()
	select {
	case failures := <-done:
		if len(failures) != 0 {
			t.Fatalf("unexpected failures: %v", failures)
		}
	case <-ctx.Done():
		t.Fatal("coalesced agent receipts did not wake all deliveries")
	}
	if len(resume.TargetReceipts) != 9 || len(resume.StorageResults) != 1 {
		t.Fatal("missing destination coverage")
	}
}
