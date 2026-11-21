package roomtransfer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
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
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

func TestStorageSourceLanePreparesTheNextRangeWhileTheCurrentOneIsDelivered(t *testing.T) {
	for _, test := range []struct {
		name   string
		memory func(fileSize, chunkSize int64) int64
		read   bool
	}{
		{"two reserved chunks read the next range ahead", contracts.RoomStorageSourceMemoryBytes, true},
		{"one reserved chunk resolves only the next route ahead", contracts.RoomBufferMemoryBytes, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			chunkSize := int64(32 << 10)
			content := bytes.Repeat([]byte("range"), int(2*chunkSize/5))
			var mu sync.Mutex
			var events []string
			observe := func(event string) {
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
			}
			seen := func(event string) bool {
				mu.Lock()
				defer mu.Unlock()
				for _, value := range events {
					if value == event {
						return true
					}
				}
				return false
			}
			var baseURL string
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/route" {
					var request contracts.StorageRouteRequest
					_ = json.NewDecoder(r.Body).Decode(&request)
					observe(fmt.Sprint("route:", request.ChunkIndex))
					offset, length := request.ChunkIndex*chunkSize, min(chunkSize, int64(len(content))-request.ChunkIndex*chunkSize)
					_ = json.NewEncoder(w).Encode(contracts.StorageRoute{ChunkIndex: request.ChunkIndex, Offset: offset, Length: length,
						ExpiresAt: now.Add(5 * time.Minute), Endpoint: contracts.HTTPEndpoint{URL: baseURL + "/source/" + fmt.Sprint(request.ChunkIndex), Method: "GET"}})
					return
				}
				index, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/source/"), 10, 64)
				observe(fmt.Sprint("read:", index))
				offset, length := index*chunkSize, min(chunkSize, int64(len(content))-index*chunkSize)
				w.Header().Set("ETag", `"frozen"`)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, len(content)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(content[offset : offset+length])
			}))
			defer provider.Close()
			baseURL = provider.URL
			_, targetKey, _ := ed25519.GenerateKey(rand.Reader)
			targetLease := directLease("target-lease", contracts.TunnelLeaseRoleTargetWrite, "member-b", targetKey.Public().(ed25519.PublicKey), now)
			targetLease.Protocol = contracts.RoomStorageCapability
			transfer := contracts.RoomTransfer{SchemaVersion: contracts.RoomStorageSchemaVersion, BatchID: "batch", RoomID: "room",
				ChannelID: "channel", PublicationID: "publication", TransferID: "publication", SnapshotVersion: 1, LaneID: "lane", Attempt: 1,
				Protection: contracts.RoomTransferProtection{Scheme: contracts.RoomStorageProtectionScheme}, FileSizeBytes: int64(len(content)),
				ChunkSizeBytes: chunkSize, ChunkCount: 2, ChunkStart: 0, ChunkEnd: 1,
				SourceLease: contracts.TunnelLease{LeaseID: "source", IntentID: "source", Role: contracts.TunnelLeaseRoleSourceRead,
					Protocol: contracts.RoomStorageCapability, ExpiresAt: now.Add(10 * time.Minute),
					Storage:   &contracts.StorageLease{MemberID: "source", ResourceID: "source", SizeBytes: int64(len(content)), ETag: `"frozen"`},
					Endpoints: []contracts.TunnelLeaseEndpoint{{URL: baseURL + "/route", Method: "POST", Headers: map[string]string{"X-Beam-Path-Token": "scoped"}}}},
				Targets: []contracts.RoomTransferDestination{{MemberID: "member-b", Lease: targetLease}}}
			handler := NewHandler(Config{StorageListenAddress: "127.0.0.1:0", StorageAdvertiseURL: "https://127.0.0.1:{port}"})
			handler.storageClient = provider.Client()
			t.Cleanup(func() { _ = handler.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			runtimes := make(chan contracts.DirectRoomTransferRuntime, 4)
			ctx = workloadprogress.WithReporter(ctx, func(outputs map[string]string) {
				var runtime contracts.DirectRoomTransferRuntime
				if json.Unmarshal([]byte(outputs["room_transfer_runtime"]), &runtime) == nil {
					runtimes <- runtime
				}
			})
			payload, _ := json.Marshal(transfer)
			done := make(chan error, 1)
			go func() {
				_, err := handler.Execute(ctx, domain.Spec{Payload: payload, Identity: domain.Identity{WorkerID: "worker-1"},
					Resources: domain.Resources{MemoryBytes: test.memory(transfer.FileSizeBytes, chunkSize)}})
				done <- err
			}()
			var runtime contracts.DirectRoomTransferRuntime
			select {
			case runtime = <-runtimes:
			case err := <-done:
				t.Fatalf("lane ended before its runtime: %v", err)
			}
			client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13,
				ServerName: runtime.TLSCertificateSHA256[:32] + "." + runtime.TLSCertificateSHA256[32:] + ".room-worker.invalid"}}}
			deliver := func(index int64, before func()) {
				path := fmt.Sprintf("%s/targets/member-b/chunks/%d", runtime.BaseURL, index)
				request, _ := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
				request.Header.Set("Authorization", "Bearer "+runtime.AccessToken)
				request.Header.Set("X-Beam-Path-Token", "path-token-target-lease")
				response, err := client.Do(request)
				if err != nil || response.StatusCode != http.StatusOK {
					t.Fatalf("download %d: %v %v", index, response, err)
				}
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				before()
				offset := index * chunkSize
				receipt := contracts.TargetRangeReceipt{TargetMemberID: "member-b", SourceRangeReceipt: contracts.SourceRangeReceipt{
					ReceiptID: fmt.Sprint("target-", index), TransferID: transfer.TransferID, LaneID: transfer.LaneID, ChunkIndex: index,
					Offset: offset, Length: int64(len(body)), RangeSHA256: digest(body), LeaseID: "target-lease", CompletedAt: now}}
				signTargetRangeReceipt(&receipt, targetKey)
				encoded, _ := json.Marshal(targetResponse{RangeReceipt: receipt})
				request, _ = http.NewRequestWithContext(ctx, http.MethodPost, path+"/receipt", bytes.NewReader(encoded))
				request.Header.Set("Authorization", "Bearer "+runtime.AccessToken)
				request.Header.Set("X-Beam-Path-Token", "path-token-target-lease")
				response, err = client.Do(request)
				if err != nil || response.StatusCode != http.StatusNoContent {
					t.Fatalf("receipt %d: %v %v", index, response, err)
				}
				response.Body.Close()
			}
			deliver(0, func() {
				deadline := time.Now().Add(5 * time.Second)
				for !seen("route:1") && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if !seen("route:1") {
					t.Fatal("the next route was not resolved while the current range was delivered")
				}
				if test.read {
					for !seen("read:1") && time.Now().Before(deadline) {
						time.Sleep(5 * time.Millisecond)
					}
				} else {
					time.Sleep(100 * time.Millisecond)
				}
				if seen("read:1") != test.read {
					t.Fatalf("next range read ahead=%v, want %v", seen("read:1"), test.read)
				}
			})
			deliver(1, func() {})
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			reads := 0
			for _, event := range events {
				if strings.HasPrefix(event, "read:") {
					reads++
				}
			}
			if reads != 2 {
				t.Fatalf("source ranges read %d times: %v", reads, events)
			}
		})
	}
}

func TestStorageClientKeepsAWarmConnectionPerLane(t *testing.T) {
	transport, ok := storageHTTPClient().Transport.(*http.Transport)
	if !ok || transport.MaxIdleConnsPerHost < storageIdleConnectionsPerHost || transport.MaxIdleConns != 0 {
		t.Fatalf("storage transport keeps too few idle connections: %+v", transport)
	}
}
