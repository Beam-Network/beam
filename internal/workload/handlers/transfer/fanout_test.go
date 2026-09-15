package transfer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestSourceGroupReadsOnceAcrossDestinationBatchesAndRetry(t *testing.T) {
	var reads atomic.Int64
	var mu sync.Mutex
	writes := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads.Add(1)
			if r.Header.Get("If-Match") != "frozen" {
				t.Error("missing immutable source condition")
			}
			w.Header().Set("Content-Range", "bytes 4-11/16")
			w.WriteHeader(206)
			_, _ = w.Write([]byte("abcdefgh"))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "abcdefgh" {
			t.Errorf("unexpected destination content: %q", body)
		}
		mu.Lock()
		writes[r.URL.Path]++
		attempt := writes[r.URL.Path]
		mu.Unlock()
		if r.URL.Path == "/7" && attempt == 1 {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/99" {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("ETag", "verified")
		w.WriteHeader(200)
	}))
	defer server.Close()
	group := contracts.MultipartTransfer{TransferID: "transfer", SourceGroupID: "range", DestinationConcurrency: 8}
	for i := 0; i < 101; i++ {
		group.Parts = append(group.Parts, contracts.TransferPart{Index: i, TaskID: fmt.Sprint(i), OfferID: fmt.Sprint(i), ETagRequired: true,
			Length: 8, Source: contracts.HTTPEndpoint{URL: server.URL + "/source", Headers: map[string]string{"Range": "bytes=4-11", "If-Match": "frozen"}}, Destination: contracts.HTTPEndpoint{URL: server.URL + "/" + strconv.Itoa(i)}})
	}
	payload, _ := json.Marshal(group)
	spec := domain.Spec{Resources: domain.Resources{MemoryBytes: (4 << 20) + 8}, Payload: payload}
	handler := NewHandler(nil)
	if err := handler.Validate(spec); err != nil {
		t.Fatal(err)
	}
	result, err := handler.Execute(context.Background(), spec)
	if err == nil || err.Error() != "source_group_delivery_failed" {
		t.Fatalf("error=%v", err)
	}
	if reads.Load() != 1 || result.BytesProcessed != 800 || result.Outputs["source.payload_bytes"] != "8" {
		t.Fatalf("source reads=%d result=%+v", reads.Load(), result)
	}
	for i := 0; i < 101; i++ {
		want := 1
		if i == 7 {
			want = 2
		}
		if writes["/"+strconv.Itoa(i)] != want {
			t.Errorf("destination %d writes=%d", i, writes["/"+strconv.Itoa(i)])
		}
		if i != 99 && result.Outputs[fmt.Sprintf("part.%d.state", i)] != "completed" {
			t.Errorf("lost successful destination %d", i)
		}
	}
}

func TestSourceGroupFailsBeforeUploadOnChangedSource(t *testing.T) {
	var writes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		w.WriteHeader(412)
	}))
	defer server.Close()
	group := contracts.MultipartTransfer{TransferID: "transfer", SourceGroupID: "range", DestinationConcurrency: 1, Parts: []contracts.TransferPart{{TaskID: "task", OfferID: "offer", Length: 8, Source: contracts.HTTPEndpoint{URL: server.URL}, Destination: contracts.HTTPEndpoint{URL: server.URL}}}}
	payload, _ := json.Marshal(group)
	_, err := NewHandler(nil).Execute(context.Background(), domain.Spec{Resources: domain.Resources{MemoryBytes: (4 << 20) + 8}, Payload: payload})
	if err == nil || err.Error() != "room_source_changed" || writes.Load() != 0 {
		t.Fatalf("err=%v uploads=%d", err, writes.Load())
	}
}

func TestSourceGroupCancellationInterruptsDestinationAndRetainedBuffer(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("abcdefgh"))
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	group := contracts.MultipartTransfer{TransferID: "transfer", SourceGroupID: "range", DestinationConcurrency: 1, Parts: []contracts.TransferPart{{TaskID: "task", OfferID: "offer", Length: 8, Source: contracts.HTTPEndpoint{URL: server.URL}, Destination: contracts.HTTPEndpoint{URL: server.URL}}}}
	payload, _ := json.Marshal(group)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := NewHandler(nil).Execute(ctx, domain.Spec{Resources: domain.Resources{MemoryBytes: (4 << 20) + 8}, Payload: payload})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("destination never started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled source group succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation retained active provider I/O")
	}
}
