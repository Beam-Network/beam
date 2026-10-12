package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

// digestSource serves one object at /source. By default it declares the object's
// Content-Length; chunked streams it without one, and declared overrides it.
type digestSource struct {
	server   *httptest.Server
	object   []byte
	chunked  bool
	declared int
	status   int
	mu       sync.Mutex
	requests []*http.Request
}

func newDigestSource(t *testing.T, object []byte) *digestSource {
	t.Helper()
	source := &digestSource{object: object, declared: -1}
	source.server = httptest.NewServer(http.HandlerFunc(source.serve))
	t.Cleanup(source.server.Close)
	return source
}

func (source *digestSource) serve(w http.ResponseWriter, r *http.Request) {
	source.mu.Lock()
	source.requests = append(source.requests, r.Clone(context.Background()))
	source.mu.Unlock()
	switch {
	case r.URL.Path == "/redirect":
		http.Redirect(w, r, "/source", http.StatusFound)
		return
	case r.URL.Path != "/source":
		w.WriteHeader(http.StatusNotFound)
		return
	case source.status != 0:
		w.WriteHeader(source.status)
		return
	}
	if source.chunked {
		// Flushing before the body forces chunked encoding without a Content-Length.
		w.(http.Flusher).Flush()
		for offset := 0; offset < len(source.object); offset += 64 << 10 {
			_, _ = w.Write(source.object[offset:min(offset+64<<10, len(source.object))])
			w.(http.Flusher).Flush()
		}
		return
	}
	declared := len(source.object)
	if source.declared >= 0 {
		declared = source.declared
	}
	w.Header().Set("Content-Length", strconv.Itoa(declared))
	_, _ = w.Write(source.object[:min(declared, len(source.object))])
	if declared > len(source.object) {
		w.(http.Flusher).Flush()
		dropConnection(w)
	}
}

func (source *digestSource) received() []*http.Request {
	source.mu.Lock()
	defer source.mu.Unlock()
	return append([]*http.Request(nil), source.requests...)
}

func (source *digestSource) paths() []string {
	var paths []string
	for _, request := range source.received() {
		paths = append(paths, request.URL.Path)
	}
	return paths
}

func digestSpec(t *testing.T, digest contracts.SourceDigest) domain.Spec {
	t.Helper()
	payload, err := json.Marshal(digest)
	if err != nil {
		t.Fatal(err)
	}
	return domain.Spec{Kind: domain.KindTransferSourceDigest, Payload: payload}
}

func runDigest(t *testing.T, ctx context.Context, digest contracts.SourceDigest) (domain.Result, error) {
	t.Helper()
	handler := NewSourceDigestHandler(http.DefaultClient)
	spec := digestSpec(t, digest)
	if err := handler.Validate(spec); err != nil {
		t.Fatal(err)
	}
	return handler.Execute(ctx, spec)
}

func (source *digestSource) digest(size int) contracts.SourceDigest {
	return contracts.SourceDigest{Size: int64(size), Source: contracts.HTTPEndpoint{URL: source.server.URL + "/source",
		Headers: map[string]string{"x-amz-security-token": "source-token"}}}
}

func digestObject(size int) []byte {
	object := make([]byte, size)
	_, _ = rand.Read(object)
	return object
}

func TestSourceDigestHashesAWholeMultiMiBSourceInOneRead(t *testing.T) {
	object := digestObject(5<<20 + 12345)
	wantSHA1, wantSHA256 := sha1.Sum(object), sha256.Sum256(object)
	for _, chunked := range []bool{false, true} {
		source := newDigestSource(t, object)
		source.chunked = chunked
		result, err := runDigest(t, context.Background(), source.digest(len(object)))
		if err != nil {
			t.Fatalf("chunked=%v: %v", chunked, err)
		}
		want := map[string]string{"sha1": hex.EncodeToString(wantSHA1[:]), "sha256": hex.EncodeToString(wantSHA256[:]),
			"bytes": strconv.Itoa(len(object))}
		if len(result.Outputs) != len(want) || result.BytesProcessed != int64(len(object)) {
			t.Fatalf("chunked=%v: outputs=%+v bytes=%d", chunked, result.Outputs, result.BytesProcessed)
		}
		for key, value := range want {
			if result.Outputs[key] != value {
				t.Fatalf("chunked=%v: %s = %q, want %q", chunked, key, result.Outputs[key], value)
			}
		}
		requests := source.received()
		request := requests[0]
		if len(requests) != 1 || request.Method != http.MethodGet || request.Header.Get("Range") != "" ||
			request.Header.Get("x-amz-security-token") != "source-token" || request.Header.Get("Accept-Encoding") != "identity" {
			t.Fatalf("chunked=%v: unexpected source requests: %d %+v", chunked, len(requests), request.Header)
		}
	}
}

func TestSourceDigestRejectsShortAndLongSources(t *testing.T) {
	object := digestObject(256 << 10)
	for name, test := range map[string]struct {
		object  []byte
		chunked bool
	}{
		"short declared": {object: object[:len(object)-1]},
		"long declared":  {object: append(bytes.Clone(object), 0)},
		"short streamed": {object: object[:len(object)-1], chunked: true},
		"long streamed":  {object: append(bytes.Clone(object), make([]byte, 64<<10)...), chunked: true},
	} {
		t.Run(name, func(t *testing.T) {
			source := newDigestSource(t, test.object)
			source.chunked = test.chunked
			result, err := runDigest(t, context.Background(), source.digest(len(object)))
			if err == nil || err.Error() != classSourceLengthMismatch || result.Outputs["error"] != classSourceLengthMismatch ||
				result.Outputs["sha1"] != "" || result.Outputs["bytes"] != "" {
				t.Fatalf("err=%v outputs=%+v", err, result.Outputs)
			}
		})
	}
}

func TestSourceDigestNeverFollowsRedirects(t *testing.T) {
	object := digestObject(1024)
	source := newDigestSource(t, object)
	digest := source.digest(len(object))
	digest.Source.URL = source.server.URL + "/redirect"
	result, err := runDigest(t, context.Background(), digest)
	if err == nil || result.Outputs["error"] != classSourceReadFailed {
		t.Fatalf("err=%v outputs=%+v", err, result.Outputs)
	}
	if paths := source.paths(); len(paths) != 1 || paths[0] != "/redirect" {
		t.Fatalf("redirect was followed: %v", paths)
	}
}

func TestSourceDigestReportsSourceReadFailures(t *testing.T) {
	object := digestObject(1024)
	for name, configure := range map[string]func(*digestSource){
		"forbidden":          func(source *digestSource) { source.status = http.StatusForbidden },
		"server error":       func(source *digestSource) { source.status = http.StatusInternalServerError },
		"dropped connection": func(source *digestSource) { source.object, source.declared = object[:512], len(object) },
	} {
		t.Run(name, func(t *testing.T) {
			source := newDigestSource(t, object)
			configure(source)
			result, err := runDigest(t, context.Background(), source.digest(len(object)))
			if err == nil || err.Error() != classSourceReadFailed || result.Outputs["error"] != classSourceReadFailed ||
				len(source.paths()) != 1 {
				t.Fatalf("err=%v outputs=%+v requests=%d", err, result.Outputs, len(source.paths()))
			}
		})
	}
}

func TestSourceDigestReportsCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2048")
		_, _ = w.Write(make([]byte, 1024))
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	result, err := runDigest(t, ctx, contracts.SourceDigest{Size: 2048, Source: contracts.HTTPEndpoint{URL: server.URL + "/source"}})
	if err == nil || err.Error() != classDeliveryCancelled || result.Outputs["error"] != classDeliveryCancelled {
		t.Fatalf("err=%v outputs=%+v", err, result.Outputs)
	}
}

func TestSourceDigestValidation(t *testing.T) {
	handler := NewSourceDigestHandler(http.DefaultClient)
	if handler.Kind() != domain.KindTransferSourceDigest {
		t.Fatalf("kind = %s", handler.Kind())
	}
	valid := contracts.SourceDigest{Size: 1, Source: contracts.HTTPEndpoint{URL: "https://bucket.example/object?X-Amz-Signature=sig"}}
	if err := handler.Validate(digestSpec(t, valid)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*contracts.SourceDigest){
		"plain http":     func(digest *contracts.SourceDigest) { digest.Source.URL = "http://bucket.example/object" },
		"relative url":   func(digest *contracts.SourceDigest) { digest.Source.URL = "/object" },
		"credentials":    func(digest *contracts.SourceDigest) { digest.Source.URL = "https://user:pass@bucket.example/object" },
		"range header":   func(digest *contracts.SourceDigest) { digest.Source.Headers = map[string]string{"range": "bytes=0-0"} },
		"zero size":      func(digest *contracts.SourceDigest) { digest.Size = 0 },
		"negative size":  func(digest *contracts.SourceDigest) { digest.Size = -1 },
		"ftp source url": func(digest *contracts.SourceDigest) { digest.Source.URL = "ftp://bucket.example/object" },
	} {
		digest := valid
		mutate(&digest)
		if err := handler.Validate(digestSpec(t, digest)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := handler.Validate(domain.Spec{Kind: domain.KindTransferSourceDigest, Payload: json.RawMessage(`[]`)}); err == nil {
		t.Error("a payload that is not a source digest was accepted")
	}
}
