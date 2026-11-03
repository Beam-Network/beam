package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func makeFastRetryHandler() *Handler {
	return NewHandler(
		http.DefaultClient,
		WithRetryConfig(
			3,                   // max 3 attempts
			5*time.Millisecond,  // initial backoff
			20*time.Millisecond, // max backoff
			50*time.Millisecond, // min deadline budget
		),
	)
}

func makeTransferSpec(sourceURL, destURL string, length int64, expectedSHA string) domain.Spec {
	transfer := contracts.MultipartTransfer{
		Parts: []contracts.TransferPart{
			{
				Index:          0,
				Length:         length,
				ExpectedSHA256: expectedSHA,
				Source: contracts.HTTPEndpoint{
					URL: sourceURL,
				},
				Destination: contracts.HTTPEndpoint{
					URL: destURL,
				},
			},
		},
	}
	payload, _ := json.Marshal(transfer)
	return domain.Spec{Payload: payload}
}

// 1. Transient destination 503 after partial PUT followed by successful retry
// 2. Final payload bytes and integrity hash match the original source
func TestPhase3H_Transient503PartialPutRecoveryAndIntegrity(t *testing.T) {
	const payloadSize = 64 * 1024 // 64 KB
	payload := make([]byte, payloadSize)
	_, _ = rand.Read(payload)
	hasher := sha256.New()
	hasher.Write(payload)
	expectedSHA := hex.EncodeToString(hasher.Sum(nil))

	var sourceReads atomic.Int32
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceReads.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer sourceServer.Close()

	var destAttempts atomic.Int32
	var receivedBody bytes.Buffer
	var mu sync.Mutex

	destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := destAttempts.Add(1)
		if attempt == 1 {
			// Read partial body (16 KB) and fail with HTTP 503
			buf := make([]byte, 16*1024)
			_, _ = io.ReadFull(r.Body, buf)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("temporary overload"))
			return
		}

		// Attempt 2: complete stream read and success
		mu.Lock()
		receivedBody.Reset()
		_, _ = io.Copy(&receivedBody, r.Body)
		mu.Unlock()

		w.Header().Set("ETag", `"etag-recovered-123"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer destServer.Close()

	spec := makeTransferSpec(sourceServer.URL+"/src", destServer.URL+"/dst", payloadSize, expectedSHA)
	h := makeFastRetryHandler()

	result, err := h.Execute(context.Background(), spec)
	if err != nil {
		t.Fatalf("expected successful recovery, got error: %v", err)
	}

	if destAttempts.Load() != 2 {
		t.Errorf("expected exactly 2 destination attempts, got %d", destAttempts.Load())
	}
	if sourceReads.Load() != 2 {
		t.Errorf("expected source GET to be restarted from byte zero on retry (2 reads), got %d", sourceReads.Load())
	}
	if result.BytesProcessed != payloadSize {
		t.Errorf("expected %d bytes processed, got %d", payloadSize, result.BytesProcessed)
	}
	if result.Outputs["part.0.sha256"] != expectedSHA {
		t.Errorf("expected hash %s, got %s", expectedSHA, result.Outputs["part.0.sha256"])
	}
	if result.Outputs["part.0.etag"] != "etag-recovered-123" {
		t.Errorf("expected etag 'etag-recovered-123', got %s", result.Outputs["part.0.etag"])
	}

	// Byte-for-byte integrity check of final destination storage
	mu.Lock()
	finalBytes := receivedBody.Bytes()
	mu.Unlock()
	if !bytes.Equal(finalBytes, payload) {
		t.Errorf("final stored bytes do not match original source payload byte-for-byte")
	}
}

// 3. Permanent 403 does not retry
func TestPhase3H_Permanent403NoRetry(t *testing.T) {
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data"))
	}))
	defer sourceServer.Close()

	var destAttempts atomic.Int32
	destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destAttempts.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("AccessDenied"))
	}))
	defer destServer.Close()

	spec := makeTransferSpec(sourceServer.URL, destServer.URL, 4, "")
	h := makeFastRetryHandler()

	_, err := h.Execute(context.Background(), spec)
	if err == nil {
		t.Fatal("expected failure on 403, got nil")
	}
	if !strings.Contains(err.Error(), "destination returned HTTP 403") {
		t.Errorf("expected 403 error message, got: %v", err)
	}
	if destAttempts.Load() != 1 {
		t.Errorf("expected permanent 403 NOT to retry (exactly 1 attempt), got %d", destAttempts.Load())
	}
}

// 4. Retry exhaustion returns a clear terminal error
func TestPhase3H_RetryExhaustion(t *testing.T) {
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("payload"))
	}))
	defer sourceServer.Close()

	var destAttempts atomic.Int32
	destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destAttempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("exhaustion-test"))
	}))
	defer destServer.Close()

	spec := makeTransferSpec(sourceServer.URL, destServer.URL, 7, "")
	h := makeFastRetryHandler()

	_, err := h.Execute(context.Background(), spec)
	if err == nil {
		t.Fatal("expected terminal failure on retry exhaustion, got nil")
	}
	if !strings.Contains(err.Error(), "destination returned HTTP 503") {
		t.Errorf("expected destination 503 in error, got: %v", err)
	}
	if destAttempts.Load() != 3 {
		t.Errorf("expected exactly 3 attempts before exhaustion, got %d", destAttempts.Load())
	}
}

// 5. Cancellation during backoff prevents the next attempt
func TestPhase3H_CancellationDuringBackoff(t *testing.T) {
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("payload"))
	}))
	defer sourceServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var destAttempts atomic.Int32

	destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destAttempts.Add(1)
		// Cancel context immediately during attempt 1 failure
		cancel()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer destServer.Close()

	spec := makeTransferSpec(sourceServer.URL, destServer.URL, 7, "")
	// Use longer backoff to ensure cancellation triggers during timer
	h := NewHandler(http.DefaultClient, WithRetryConfig(3, 200*time.Millisecond, 500*time.Millisecond, 10*time.Millisecond))

	_, err := h.Execute(ctx, spec)
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("expected context canceled, got: %v", err)
	}
	if destAttempts.Load() != 1 {
		t.Errorf("expected attempt 2 to be aborted by cancellation (exactly 1 attempt), got %d", destAttempts.Load())
	}
}

// 6. Insufficient deadline budget prevents starting another attempt
func TestPhase3H_InsufficientDeadlineBudgetPreventsAttempt(t *testing.T) {
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("payload"))
	}))
	defer sourceServer.Close()

	var destAttempts atomic.Int32
	destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destAttempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer destServer.Close()

	spec := makeTransferSpec(sourceServer.URL, destServer.URL, 7, "")
	// Handler requires 500ms min deadline budget
	h := NewHandler(http.DefaultClient, WithRetryConfig(3, 50*time.Millisecond, 100*time.Millisecond, 500*time.Millisecond))

	// Provide context with only 100ms deadline budget (less than 500ms required)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := h.Execute(ctx, spec)
	if err == nil {
		t.Fatal("expected error due to insufficient deadline, got nil")
	}
	if !strings.Contains(err.Error(), "insufficient deadline budget") {
		t.Errorf("expected deadline budget error, got: %v", err)
	}
	if destAttempts.Load() != 0 {
		t.Errorf("expected 0 attempts started due to immediate budget check, got %d", destAttempts.Load())
	}
}

// 7. Source GET failure and destination network failure follow intended retry policy
func TestPhase3H_SourceAndNetworkFailureRetryPolicy(t *testing.T) {
	t.Run("SourceTransient503Recovery", func(t *testing.T) {
		var sourceAttempts atomic.Int32
		sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			att := sourceAttempts.Add(1)
			if att == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("source transient error"))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("recovered source data"))
		}))
		defer sourceServer.Close()

		var destAttempts atomic.Int32
		destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			destAttempts.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer destServer.Close()

		spec := makeTransferSpec(sourceServer.URL, destServer.URL, int64(len("recovered source data")), "")
		h := makeFastRetryHandler()

		_, err := h.Execute(context.Background(), spec)
		if err != nil {
			t.Fatalf("expected successful recovery from source 503, got: %v", err)
		}
		if sourceAttempts.Load() != 2 {
			t.Errorf("expected 2 source attempts, got %d", sourceAttempts.Load())
		}
		if destAttempts.Load() != 1 {
			t.Errorf("expected 1 dest attempt (after source recovery), got %d", destAttempts.Load())
		}
	})

	t.Run("SourcePermanent404NoRetry", func(t *testing.T) {
		var sourceAttempts atomic.Int32
		sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sourceAttempts.Add(1)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("NoSuchKey"))
		}))
		defer sourceServer.Close()

		destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer destServer.Close()

		spec := makeTransferSpec(sourceServer.URL, destServer.URL, 10, "")
		h := makeFastRetryHandler()

		_, err := h.Execute(context.Background(), spec)
		if err == nil {
			t.Fatal("expected failure on source 404, got nil")
		}
		if sourceAttempts.Load() != 1 {
			t.Errorf("expected permanent 404 NOT to retry (1 attempt), got %d", sourceAttempts.Load())
		}
	})

	t.Run("DestinationNetworkResetRecovery", func(t *testing.T) {
		sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("valid payload"))
		}))
		defer sourceServer.Close()

		var destAttempts atomic.Int32
		// Custom listener simulating TCP connection reset on attempt 1
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		destURL := "http://" + listener.Addr().String()

		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				att := destAttempts.Add(1)
				if att == 1 {
					// Force immediate TCP close/reset
					_ = conn.Close()
					continue
				}
				// Attempt 2: serve clean HTTP 200
				buf := make([]byte, 1024)
				_, _ = conn.Read(buf)
				resp := "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nETag: \"net-ok\"\r\n\r\n"
				_, _ = conn.Write([]byte(resp))
				_ = conn.Close()
			}
		}()
		defer listener.Close()

		spec := makeTransferSpec(sourceServer.URL, destURL, int64(len("valid payload")), "")
		h := makeFastRetryHandler()

		res, err := h.Execute(context.Background(), spec)
		if err != nil {
			t.Fatalf("expected network reset to trigger retry and succeed, got: %v", err)
		}
		if res.Outputs["part.0.etag"] != "net-ok" {
			t.Errorf("expected etag 'net-ok', got: %s", res.Outputs["part.0.etag"])
		}
		if destAttempts.Load() != 2 {
			t.Errorf("expected 2 destination attempts (1 reset + 1 success), got %d", destAttempts.Load())
		}
	})
}

// 8. Success path remains unchanged
func TestPhase3H_SuccessPathUnchanged(t *testing.T) {
	const payload = "unmodified-transfer-payload"
	hasher := sha256.New()
	hasher.Write([]byte(payload))
	expectedSHA := hex.EncodeToString(hasher.Sum(nil))

	var sourceAttempts, destAttempts atomic.Int32
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceAttempts.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	}))
	defer sourceServer.Close()

	destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destAttempts.Add(1)
		w.Header().Set("ETag", `"single-shot-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer destServer.Close()

	spec := makeTransferSpec(sourceServer.URL, destServer.URL, int64(len(payload)), expectedSHA)
	h := NewHandler(http.DefaultClient) // uses loopback-capable client for test server

	start := time.Now()
	res, err := h.Execute(context.Background(), spec)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error on success path: %v", err)
	}
	if sourceAttempts.Load() != 1 || destAttempts.Load() != 1 {
		t.Errorf("expected exactly 1 attempt each on success, got source=%d dest=%d", sourceAttempts.Load(), destAttempts.Load())
	}
	if res.BytesProcessed != int64(len(payload)) {
		t.Errorf("expected %d bytes, got %d", len(payload), res.BytesProcessed)
	}
	if res.Outputs["part.0.sha256"] != expectedSHA {
		t.Errorf("expected hash %s, got %s", expectedSHA, res.Outputs["part.0.sha256"])
	}
	if res.Outputs["part.0.etag"] != "single-shot-etag" {
		t.Errorf("expected etag 'single-shot-etag', got %s", res.Outputs["part.0.etag"])
	}
	// Success path should execute in milliseconds without retry delays
	if elapsed > 500*time.Millisecond {
		t.Errorf("success path unexpectedly delayed: %v", elapsed)
	}
}

// TestPhase3H2_MultiPartRetryRegression verifies a 3-part multipart transfer fixture where:
// - Part 0 succeeds on the first attempt.
// - Part 1 fails transiently on attempt 1 with HTTP 503 after a partial write, then recovers on attempt 2.
// - Part 2 succeeds on the first attempt.
// Invariants verified:
// - Final payload bytes and integrity hashes are 100% correct across all 3 parts.
// - Attempt counts: Part 0 (1 GET, 1 PUT), Part 1 (2 GET, 2 PUT), Part 2 (1 GET, 1 PUT).
// - Completed parts (Part 0) are NOT retransmitted when Part 1 retries.
func TestPhase3H2_MultiPartRetryRegression(t *testing.T) {
	const (
		part0Size = 16 * 1024 // 16 KB
		part1Size = 32 * 1024 // 32 KB
		part2Size = 48 * 1024 // 48 KB
	)

	payload0 := make([]byte, part0Size)
	_, _ = rand.Read(payload0)
	h0 := sha256.Sum256(payload0)
	sha0 := hex.EncodeToString(h0[:])

	payload1 := make([]byte, part1Size)
	_, _ = rand.Read(payload1)
	h1 := sha256.Sum256(payload1)
	sha1 := hex.EncodeToString(h1[:])

	payload2 := make([]byte, part2Size)
	_, _ = rand.Read(payload2)
	h2 := sha256.Sum256(payload2)
	sha2 := hex.EncodeToString(h2[:])

	var sourceAttempts [3]atomic.Int32
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		switch r.URL.Path {
		case "/src/0":
			sourceAttempts[0].Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload0)
		case "/src/1":
			sourceAttempts[1].Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload1)
		case "/src/2":
			sourceAttempts[2].Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload2)
		default:
			http.NotFound(w, r)
		}
	}))
	defer sourceServer.Close()

	var destAttempts [3]atomic.Int32
	var destBodies [3]bytes.Buffer
	var mu sync.Mutex

	destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dst/0":
			destAttempts[0].Add(1)
			mu.Lock()
			destBodies[0].Reset()
			_, _ = io.Copy(&destBodies[0], r.Body)
			mu.Unlock()
			w.Header().Set("ETag", `"etag-part-0"`)
			w.WriteHeader(http.StatusOK)

		case "/dst/1":
			att := destAttempts[1].Add(1)
			if att == 1 {
				// Partial write (read 8KB) then transient 503 failure
				buf := make([]byte, 8*1024)
				_, _ = io.ReadFull(r.Body, buf)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("transient 503 on part 1"))
				return
			}
			// Attempt 2 succeeds
			mu.Lock()
			destBodies[1].Reset()
			_, _ = io.Copy(&destBodies[1], r.Body)
			mu.Unlock()
			w.Header().Set("ETag", `"etag-part-1"`)
			w.WriteHeader(http.StatusOK)

		case "/dst/2":
			destAttempts[2].Add(1)
			mu.Lock()
			destBodies[2].Reset()
			_, _ = io.Copy(&destBodies[2], r.Body)
			mu.Unlock()
			w.Header().Set("ETag", `"etag-part-2"`)
			w.WriteHeader(http.StatusOK)

		default:
			http.NotFound(w, r)
		}
	}))
	defer destServer.Close()

	transfer := contracts.MultipartTransfer{
		Parts: []contracts.TransferPart{
			{
				Index:          0,
				Length:         part0Size,
				ExpectedSHA256: sha0,
				Source:         contracts.HTTPEndpoint{URL: sourceServer.URL + "/src/0"},
				Destination:    contracts.HTTPEndpoint{URL: destServer.URL + "/dst/0"},
			},
			{
				Index:          1,
				Length:         part1Size,
				ExpectedSHA256: sha1,
				Source:         contracts.HTTPEndpoint{URL: sourceServer.URL + "/src/1"},
				Destination:    contracts.HTTPEndpoint{URL: destServer.URL + "/dst/1"},
			},
			{
				Index:          2,
				Length:         part2Size,
				ExpectedSHA256: sha2,
				Source:         contracts.HTTPEndpoint{URL: sourceServer.URL + "/src/2"},
				Destination:    contracts.HTTPEndpoint{URL: destServer.URL + "/dst/2"},
			},
		},
	}

	payloadBytes, _ := json.Marshal(transfer)
	spec := domain.Spec{Payload: payloadBytes}
	h := makeFastRetryHandler()

	result, err := h.Execute(context.Background(), spec)
	if err != nil {
		t.Fatalf("expected successful multi-part transfer with retry, got error: %v", err)
	}

	// 1. Verify Attempt Counts per Part
	if sourceAttempts[0].Load() != 1 || destAttempts[0].Load() != 1 {
		t.Errorf("Part 0 attempt count mismatch: source=%d dest=%d (want 1, 1)", sourceAttempts[0].Load(), destAttempts[0].Load())
	}
	if sourceAttempts[1].Load() != 2 || destAttempts[1].Load() != 2 {
		t.Errorf("Part 1 attempt count mismatch: source=%d dest=%d (want 2, 2)", sourceAttempts[1].Load(), destAttempts[1].Load())
	}
	if sourceAttempts[2].Load() != 1 || destAttempts[2].Load() != 1 {
		t.Errorf("Part 2 attempt count mismatch: source=%d dest=%d (want 1, 1)", sourceAttempts[2].Load(), destAttempts[2].Load())
	}

	// 2. Total Bytes Processed
	const expectedTotalBytes = part0Size + part1Size + part2Size
	if result.BytesProcessed != expectedTotalBytes {
		t.Errorf("total bytes processed mismatch: got %d, want %d", result.BytesProcessed, expectedTotalBytes)
	}

	// 3. Verify Output Hashes and ETags
	if result.Outputs["part.0.sha256"] != sha0 || result.Outputs["part.0.etag"] != "etag-part-0" {
		t.Errorf("Part 0 outputs mismatch: sha=%s etag=%s", result.Outputs["part.0.sha256"], result.Outputs["part.0.etag"])
	}
	if result.Outputs["part.1.sha256"] != sha1 || result.Outputs["part.1.etag"] != "etag-part-1" {
		t.Errorf("Part 1 outputs mismatch: sha=%s etag=%s", result.Outputs["part.1.sha256"], result.Outputs["part.1.etag"])
	}
	if result.Outputs["part.2.sha256"] != sha2 || result.Outputs["part.2.etag"] != "etag-part-2" {
		t.Errorf("Part 2 outputs mismatch: sha=%s etag=%s", result.Outputs["part.2.sha256"], result.Outputs["part.2.etag"])
	}

	// 4. Verify Payload Byte-for-Byte Integrity at Destination
	mu.Lock()
	body0 := destBodies[0].Bytes()
	body1 := destBodies[1].Bytes()
	body2 := destBodies[2].Bytes()
	mu.Unlock()

	if !bytes.Equal(body0, payload0) {
		t.Errorf("Part 0 destination payload does not match source byte-for-byte")
	}
	if !bytes.Equal(body1, payload1) {
		t.Errorf("Part 1 destination payload does not match source byte-for-byte")
	}
	if !bytes.Equal(body2, payload2) {
		t.Errorf("Part 2 destination payload does not match source byte-for-byte")
	}
}

// ---------------------------------------------------------------------------
// Phase 4A.31 Reliability-First Retry Candidate Validation Tests
// ---------------------------------------------------------------------------

// TestPhase4A31_Permanent4xx_NoInappropriateRetries verifies that client errors
// (400, 401, 403, 404, 409) are treated as permanently non-retryable and
// never trigger retry attempts.
func TestPhase4A31_Permanent4xx_NoInappropriateRetries(t *testing.T) {
	statusCodes := []int{
		http.StatusBadRequest,         // 400
		http.StatusUnauthorized,       // 401
		http.StatusForbidden,          // 403
		http.StatusNotFound,           // 404
		http.StatusConflict,           // 409
		http.StatusPreconditionFailed, // 412
	}

	for _, code := range statusCodes {
		t.Run(http.StatusText(code), func(t *testing.T) {
			sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("test-payload"))
			}))
			defer sourceServer.Close()

			var destAttempts atomic.Int32
			destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				destAttempts.Add(1)
				w.WriteHeader(code)
				_, _ = w.Write([]byte("client error"))
			}))
			defer destServer.Close()

			spec := makeTransferSpec(sourceServer.URL, destServer.URL, 12, "")
			h := makeFastRetryHandler()

			_, err := h.Execute(context.Background(), spec)
			if err == nil {
				t.Fatalf("expected error for HTTP %d, got nil", code)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("destination returned HTTP %d", code)) {
				t.Errorf("expected error containing HTTP %d, got: %v", code, err)
			}
			if destAttempts.Load() != 1 {
				t.Errorf("expected exactly 1 attempt (0 retries) for HTTP %d, got %d", code, destAttempts.Load())
			}
		})
	}
}

// TestPhase4A31_DeadlineExpirationDuringBackoff verifies that when a context deadline
// expires while waiting in the backoff timer, the retry is aborted immediately and
// returns DeadlineExceeded without launching attempt 2.
func TestPhase4A31_DeadlineExpirationDuringBackoff(t *testing.T) {
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("test-payload"))
	}))
	defer sourceServer.Close()

	var destAttempts atomic.Int32
	destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destAttempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer destServer.Close()

	spec := makeTransferSpec(sourceServer.URL, destServer.URL, 12, "")
	// Configure backoff: initial 200ms, minDeadlineBudget 10ms
	h := NewHandler(http.DefaultClient, WithRetryConfig(3, 200*time.Millisecond, 500*time.Millisecond, 10*time.Millisecond))

	// Context deadline set to 30ms: enough for Attempt 1 (< 5ms), but expires while waiting 200ms backoff
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := h.Execute(ctx, spec)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected deadline exceeded error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context deadline exceeded") && !strings.Contains(err.Error(), "insufficient deadline budget") {
		t.Errorf("expected deadline exceeded error, got: %v", err)
	}
	if destAttempts.Load() != 1 {
		t.Errorf("expected exactly 1 attempt (attempt 2 aborted by deadline expiration), got %d", destAttempts.Load())
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("backoff did not abort promptly on deadline expiry, elapsed: %v", elapsed)
	}
}

// TestPhase4A31_CorruptedRetryByteIntegrityFailure verifies that if a retry attempt
// receives corrupted bytes that do not match the expected SHA-256 digest:
// 1. The handler rejects the transfer with a SHA mismatch error.
// 2. The error is non-retryable (does not loop).
// 3. The handler never falsely reports success or records corrupted outputs.
func TestPhase4A31_CorruptedRetryByteIntegrityFailure(t *testing.T) {
	goodPayload := []byte("original-authentic-data")
	corruptPayload := []byte("corrupted-tampered-data!")

	hasher := sha256.New()
	hasher.Write(goodPayload)
	expectedSHA := hex.EncodeToString(hasher.Sum(nil))

	var sourceAttempts atomic.Int32
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := sourceAttempts.Add(1)
		w.WriteHeader(http.StatusOK)
		if att == 1 {
			// First read serves authentic data
			_, _ = w.Write(goodPayload)
		} else {
			// Second read (on retry) serves corrupted data!
			_, _ = w.Write(corruptPayload)
		}
	}))
	defer sourceServer.Close()

	var destAttempts atomic.Int32
	destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := destAttempts.Add(1)
		if att == 1 {
			// Attempt 1 fails with transient 503
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// Attempt 2 consumes body and accepts HTTP 200
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer destServer.Close()

	spec := makeTransferSpec(sourceServer.URL, destServer.URL, int64(len(goodPayload)), expectedSHA)
	h := makeFastRetryHandler()

	result, err := h.Execute(context.Background(), spec)
	if err == nil {
		t.Fatal("expected SHA mismatch error on corrupted retry, got nil")
	}
	if !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Errorf("expected SHA-256 mismatch in error, got: %v", err)
	}
	// Exactly 2 attempts: attempt 1 (transient 503), attempt 2 (corrupted, non-retryable terminal exit)
	if destAttempts.Load() != 2 {
		t.Errorf("expected exactly 2 attempts, got %d", destAttempts.Load())
	}
	// Ensure corrupted output was not committed
	if result.Outputs["part.0.sha256"] != "" {
		t.Errorf("corrupted hash should not be returned as valid output, got: %s", result.Outputs["part.0.sha256"])
	}
}

// TestStorageHTTPClientPreserved verifies that NewHandler(nil) uses storagehttp.NewClient
// which preserves the security restriction that non-public IP addresses (e.g. loopback)
// are refused.
func TestStorageHTTPClientPreserved(t *testing.T) {
	h := NewHandler(nil)
	if h.client == nil {
		t.Fatal("expected non-nil client from NewHandler(nil)")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// An attempt by the default client to connect to httptest loopback must fail
	// because storagehttp refuses non-public addresses.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.client.Do(req)
	if err == nil {
		t.Fatal("expected error connecting to private/loopback address with default client, got nil")
	}
	if !strings.Contains(err.Error(), "storage address is not public") {
		t.Logf("Observed rejection for loopback with default client: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Phase 4A.40 Test Suite: Jittered Backoff, Capping, and Multi-Part Headroom
// ---------------------------------------------------------------------------

func TestPhase4A40_JitterBoundsAndDistribution(t *testing.T) {
	h := NewHandler(http.DefaultClient, WithRetryConfig(
		3,
		100*time.Millisecond, // initial backoff
		2*time.Second,        // max backoff
		1*time.Second,        // min budget
	))

	// 1. Verify Attempt 1: Equal Jitter bounds [50ms, 100ms]
	var minObserved1, maxObserved1 time.Duration
	minObserved1 = 10 * time.Second

	for i := 0; i < 1000; i++ {
		d := h.calculateBackoff(1, 0)
		if d < 50*time.Millisecond || d > 100*time.Millisecond {
			t.Fatalf("attempt 1 backoff out of bounds [50ms, 100ms]: got %v", d)
		}
		if d < minObserved1 {
			minObserved1 = d
		}
		if d > maxObserved1 {
			maxObserved1 = d
		}
	}
	if minObserved1 == maxObserved1 {
		t.Errorf("expected jittered variation in attempt 1, got constant %v", minObserved1)
	}

	// 2. Verify Attempt 2: Equal Jitter bounds [100ms, 200ms]
	var minObserved2, maxObserved2 time.Duration
	minObserved2 = 10 * time.Second

	for i := 0; i < 1000; i++ {
		d := h.calculateBackoff(2, 0)
		if d < 100*time.Millisecond || d > 200*time.Millisecond {
			t.Fatalf("attempt 2 backoff out of bounds [100ms, 200ms]: got %v", d)
		}
		if d < minObserved2 {
			minObserved2 = d
		}
		if d > maxObserved2 {
			maxObserved2 = d
		}
	}
	if minObserved2 == maxObserved2 {
		t.Errorf("expected jittered variation in attempt 2, got constant %v", minObserved2)
	}

	// 3. Verify Large Attempt (attempt 10): Capped at maxBackoff [1s, 2s]
	for i := 0; i < 500; i++ {
		d := h.calculateBackoff(10, 0)
		if d < 1*time.Second || d > 2*time.Second {
			t.Fatalf("attempt 10 backoff out of bounds [1s, 2s]: got %v", d)
		}
	}
}

func TestPhase4A40_JitterInjectableDeterminism(t *testing.T) {
	// Inject 0 jitter: returns exact lower bound (half)
	hMin := NewHandler(http.DefaultClient,
		WithRetryConfig(3, 100*time.Millisecond, 2*time.Second, 1*time.Second),
		WithRandDuration(func(n time.Duration) time.Duration { return 0 }),
	)
	if d := hMin.calculateBackoff(1, 0); d != 50*time.Millisecond {
		t.Errorf("expected min attempt 1 backoff 50ms, got %v", d)
	}
	if d := hMin.calculateBackoff(2, 0); d != 100*time.Millisecond {
		t.Errorf("expected min attempt 2 backoff 100ms, got %v", d)
	}

	// Inject max jitter (n): returns exact upper bound (base)
	hMax := NewHandler(http.DefaultClient,
		WithRetryConfig(3, 100*time.Millisecond, 2*time.Second, 1*time.Second),
		WithRandDuration(func(n time.Duration) time.Duration { return n }),
	)
	if d := hMax.calculateBackoff(1, 0); d != 100*time.Millisecond {
		t.Errorf("expected max attempt 1 backoff 100ms, got %v", d)
	}
	if d := hMax.calculateBackoff(2, 0); d != 200*time.Millisecond {
		t.Errorf("expected max attempt 2 backoff 200ms, got %v", d)
	}

	// Inject midpoint (n/2): returns exact midpoint
	hMid := NewHandler(http.DefaultClient,
		WithRetryConfig(3, 100*time.Millisecond, 2*time.Second, 1*time.Second),
		WithRandDuration(func(n time.Duration) time.Duration { return n / 2 }),
	)
	if d := hMid.calculateBackoff(1, 0); d != 75*time.Millisecond {
		t.Errorf("expected mid attempt 1 backoff 75ms, got %v", d)
	}
	if d := hMid.calculateBackoff(2, 0); d != 150*time.Millisecond {
		t.Errorf("expected mid attempt 2 backoff 150ms, got %v", d)
	}
}

func TestPhase4A40_BackoffCappingAndRetryAfter(t *testing.T) {
	h := NewHandler(http.DefaultClient, WithRetryConfig(
		3,
		100*time.Millisecond,
		2*time.Second,
		1*time.Second,
	))

	// Server specifies Retry-After: 500ms (less than maxBackoff) -> exactly 500ms, un-jittered
	if d := h.calculateBackoff(1, 500*time.Millisecond); d != 500*time.Millisecond {
		t.Errorf("expected server-directed 500ms, got %v", d)
	}

	// Server specifies Retry-After: 10s (exceeds maxBackoff 2s) -> clamped strictly to maxBackoff
	if d := h.calculateBackoff(1, 10*time.Second); d != 2*time.Second {
		t.Errorf("expected clamped Retry-After 2s, got %v", d)
	}

	// No Retry-After with massive attempt number -> strictly capped at maxBackoff (2s)
	for i := 0; i < 100; i++ {
		d := h.calculateBackoff(15, 0)
		if d > 2*time.Second {
			t.Fatalf("expected delay <= 2s, got %v", d)
		}
	}
}

func TestPhase4A40_MultiPartRemainingDeadlineBudget(t *testing.T) {
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("part-content"))
	}))
	defer sourceServer.Close()

	var destAttempts atomic.Int32
	destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destAttempts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer destServer.Close()

	// 3 parts in transfer spec
	transfer := contracts.MultipartTransfer{
		Parts: []contracts.TransferPart{
			{Index: 0, Length: 12, Source: contracts.HTTPEndpoint{URL: sourceServer.URL}, Destination: contracts.HTTPEndpoint{URL: destServer.URL}},
			{Index: 1, Length: 12, Source: contracts.HTTPEndpoint{URL: sourceServer.URL}, Destination: contracts.HTTPEndpoint{URL: destServer.URL}},
			{Index: 2, Length: 12, Source: contracts.HTTPEndpoint{URL: sourceServer.URL}, Destination: contracts.HTTPEndpoint{URL: destServer.URL}},
		},
	}
	payload, _ := json.Marshal(transfer)
	spec := domain.Spec{Payload: payload}

	// Handler configured with 100ms minDeadlineBudget per part
	// 3 parts need at least 3 * 100ms = 300ms budget
	h := NewHandler(http.DefaultClient, WithRetryConfig(3, 10*time.Millisecond, 50*time.Millisecond, 100*time.Millisecond))

	// Provide context with only 150ms total timeout (less than 300ms needed for 3 parts)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := h.Execute(ctx, spec)
	if err == nil {
		t.Fatal("expected error due to insufficient deadline for 3 parts, got nil")
	}
	if !strings.Contains(err.Error(), "insufficient deadline budget") {
		t.Errorf("expected insufficient deadline budget error, got: %v", err)
	}
	if destAttempts.Load() != 0 {
		t.Errorf("expected 0 attempts started due to multi-part headroom check, got %d", destAttempts.Load())
	}
}

// ---------------------------------------------------------------------------
// Phase 4A.49 Test Suite: Edge-Case Failure Dynamics
// ---------------------------------------------------------------------------

// TestPhase4A49_TransientFailureThen403TerminalAbort verifies that if attempt 1 encounters
// a transient network drop (simulated via TCP hijack/drop), but the subsequent retry attempt 2
// encounters HTTP 403 Forbidden (e.g. presigned URL expired or invalidated upon reuse),
// the retry loop terminates immediately at attempt 2 and does NOT make a third attempt.
func TestPhase4A49_TransientFailureThen403TerminalAbort(t *testing.T) {
	payload := []byte("payload-for-transient-then-403")
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer sourceServer.Close()

	var destAttempts atomic.Int32
	destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := destAttempts.Add(1)
		if att == 1 {
			// Attempt 1: Transient network failure (TCP connection drop)
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// Attempt 2: Terminal HTTP 403 Forbidden (e.g. single-use presigned URL expired)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("presigned URL expired or invalid"))
	}))
	defer destServer.Close()

	spec := makeTransferSpec(sourceServer.URL, destServer.URL, int64(len(payload)), "")
	h := makeFastRetryHandler()

	result, err := h.Execute(context.Background(), spec)
	if err == nil {
		t.Fatal("expected terminal failure on HTTP 403, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected error message to reference HTTP 403, got: %v", err)
	}
	if destAttempts.Load() != 2 {
		t.Errorf("expected exactly 2 destination attempts (1 drop + 1 403, no attempt 3), got %d", destAttempts.Load())
	}
	if result.BytesProcessed != 0 {
		t.Errorf("expected 0 bytes processed on terminal failure, got %d", result.BytesProcessed)
	}
}

// TestPhase4A49_DestinationStallUntilContextDeadline verifies that if the destination
// stalls during transmission, the worker respects the request context deadline,
// aborts promptly, performs cleanup, bounds total execution time, and records zero retries.
func TestPhase4A49_DestinationStallUntilContextDeadline(t *testing.T) {
	payload := []byte("payload-for-destination-stall-test")
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer sourceServer.Close()

	var destAttempts atomic.Int32

	destServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destAttempts.Add(1)
		// Stall destination response longer than client timeout
		select {
		case <-r.Context().Done():
		case <-time.After(1 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer destServer.Close()

	spec := makeTransferSpec(sourceServer.URL, destServer.URL, int64(len(payload)), "")
	h := makeFastRetryHandler()

	// Short context deadline: 100ms
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	result, err := h.Execute(ctx, spec)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error due to context expiration during stall, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("expected deadline exceeded error, got: %v", err)
	}
	// Verify bounded execution: should not exceed 500ms
	if elapsed > 500*time.Millisecond {
		t.Errorf("execution took %v, expected bounded completion near 100ms", elapsed)
	}
	// Verify no retries: when deadline expires, attempt count must remain 1
	if destAttempts.Load() != 1 {
		t.Errorf("expected exactly 1 attempt before deadline expiration, got %d", destAttempts.Load())
	}
	if result.BytesProcessed != 0 {
		t.Errorf("expected 0 bytes processed on canceled transfer, got %d", result.BytesProcessed)
	}
}
