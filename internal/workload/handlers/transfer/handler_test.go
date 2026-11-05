package transfer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func presignedSpec(sourceURL, destinationURL string) domain.Spec {
	payload, _ := json.Marshal(contracts.MultipartTransfer{Parts: []contracts.TransferPart{{
		Index: 1, Offset: 0, Length: 4, SourceRange: true,
		Source:      contracts.HTTPEndpoint{URL: sourceURL},
		Destination: contracts.HTTPEndpoint{URL: destinationURL, Method: http.MethodPut},
	}}})
	return domain.Spec{Kind: domain.KindTransferMultipart, Payload: payload}
}

func TestHandlerRetriesTransientPresignedAnswers(t *testing.T) {
	var gets, puts atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		if gets.Add(1) == 1 {
			http.Error(response, "ServiceUnavailable", http.StatusServiceUnavailable)
			return
		}
		response.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(response, "abcd")
	}))
	defer source.Close()
	destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		if puts.Add(1) == 1 {
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	handler := NewHandler(http.DefaultClient)
	var waits []time.Duration
	handler.wait = func(_ context.Context, delay time.Duration) error { waits = append(waits, delay); return nil }
	result, err := handler.Execute(context.Background(), presignedSpec(source.URL, destination.URL))
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesProcessed != 4 || gets.Load() != 3 || puts.Load() != 2 ||
		len(waits) != 2 || waits[0] != uploadRetryBackoff || waits[1] != 2*uploadRetryBackoff {
		t.Fatalf("result=%+v gets=%d puts=%d waits=%v", result, gets.Load(), puts.Load(), waits)
	}
}

func TestHandlerStopsPresignedRetries(t *testing.T) {
	for name, test := range map[string]struct {
		status  int
		timeout time.Duration
		puts    int32
		err     string
	}{
		"attempts exhausted": {status: http.StatusInternalServerError, puts: presignedAttempts, err: "part 1: destination returned HTTP 500"},
		"refused":            {status: http.StatusForbidden, puts: 1, err: "part 1: destination returned HTTP 403"},
		"lease ends":         {status: http.StatusServiceUnavailable, timeout: 100 * time.Millisecond, puts: 1, err: "part 1: destination returned HTTP 503"},
	} {
		source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(response, "abcd")
		}))
		var puts atomic.Int32
		destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			puts.Add(1)
			_, _ = io.Copy(io.Discard, request.Body)
			response.WriteHeader(test.status)
		}))
		ctx, cancel := context.WithCancel(context.Background())
		if test.timeout > 0 {
			ctx, cancel = context.WithTimeout(context.Background(), test.timeout)
		}
		handler := NewHandler(http.DefaultClient)
		handler.wait = func(context.Context, time.Duration) error { return nil }
		_, err := handler.Execute(ctx, presignedSpec(source.URL, destination.URL))
		cancel()
		source.Close()
		destination.Close()
		if err == nil || err.Error() != test.err || puts.Load() != test.puts {
			t.Errorf("%s: err=%v puts=%d", name, err, puts.Load())
		}
	}
}
