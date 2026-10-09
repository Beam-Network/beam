package roomtransfer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestStorageFailureClassIsBoundedAndNeverCarriesTheURL(t *testing.T) {
	signed := "https://bucket.example/object?X-Amz-Signature=secret"
	for _, test := range []struct {
		err   error
		class string
	}{
		{storageFault("route_expired", "storage assignment is expired or outside its range"), "route_expired"},
		{httpFault("", 503, "storage destination returned HTTP 503"), "http_503"},
		{httpFault("route_", 403, "storage route control rejected assignment: HTTP 403"), "route_http_403"},
		{transportFault("transport", "storage destination write failed", timeoutError{}), "transport_timeout"},
		{transportFault("transport", "storage destination write failed", &net.OpError{Op: "dial", Err: errors.New("refused " + signed)}), "transport_dial"},
		{transportFault("source_transport", "storage source read failed", context.DeadlineExceeded), "source_transport_timeout"},
		{transportFault("transport", "storage destination write failed", errors.New("unexpected " + signed)), "transport_other"},
		{context.Canceled, "cancelled"},
		{errors.New("unclassified " + signed), "other"},
	} {
		if got := storageFailureClass(test.err); got != test.class {
			t.Fatalf("storageFailureClass(%q) = %q, want %q", test.err, got, test.class)
		}
		if strings.Contains(test.err.Error(), "X-Amz-Signature") && test.class != "other" {
			t.Fatalf("classified storage error message leaked its URL: %q", test.err)
		}
	}
}

func TestStorageRedirectIsClassifiedThroughTheTransportError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
	}))
	defer server.Close()
	client := storageHTTPClient()
	client.Transport = server.Client().Transport
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPut, server.URL+"/part?X-Amz-Signature=secret", nil)
	_, err := client.Do(request)
	failure := transportFault("transport", "storage destination write failed", err)
	if got := storageFailureClass(failure); got != "transport_redirect" {
		t.Fatalf("redirect class = %q, want transport_redirect", got)
	}
	if strings.Contains(failure.Error(), "X-Amz-Signature") {
		t.Fatalf("storage error leaked the request URL: %q", failure)
	}
}
