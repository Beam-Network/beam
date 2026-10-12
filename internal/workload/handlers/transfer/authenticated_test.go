package transfer

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

const boxObject = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// boxEmulator serves a ranged source object and emulates Box upload-session
// parts and small-file form uploads.
type boxEmulator struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	// respond overrides the Box reply for the given 1-based upload request.
	respond      func(attempt int, w http.ResponseWriter, r *http.Request, body []byte) bool
	uploads      [][]byte
	sourceReads  int
	sourceExtra  string
	sourceLength int
}

func newBoxEmulator(t *testing.T) *boxEmulator {
	t.Helper()
	box := &boxEmulator{t: t}
	box.server = httptest.NewServer(http.HandlerFunc(box.serve))
	t.Cleanup(box.server.Close)
	return box
}

func (box *boxEmulator) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/source" {
		box.mu.Lock()
		box.sourceReads++
		extra, length := box.sourceExtra, box.sourceLength
		box.mu.Unlock()
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		content := boxObject[start:end+1] + extra
		if length > 0 {
			content = content[:length]
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(boxObject)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, content)
		return
	}
	body, _ := io.ReadAll(r.Body)
	box.mu.Lock()
	box.uploads = append(box.uploads, body)
	attempt := len(box.uploads)
	respond := box.respond
	box.mu.Unlock()
	if respond != nil && respond(attempt, w, r, body) {
		return
	}
	if r.Header.Get("Authorization") != "Bearer worker-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/upload_sessions/"):
		box.servePart(w, r, body)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/files/"):
		box.serveForm(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (box *boxEmulator) servePart(w http.ResponseWriter, r *http.Request, body []byte) {
	var start, end, total int
	if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil ||
		end-start+1 != len(body) || r.Header.Get("Digest") == "" {
		dropConnection(w)
		return
	}
	digest := sha1.Sum(body)
	if r.Header.Get("Digest") != "sha="+base64.StdEncoding.EncodeToString(digest[:]) {
		writeBoxError(w, http.StatusBadRequest, "digest_mismatch")
		return
	}
	writeJSON(w, map[string]any{"part": map[string]any{"part_id": fmt.Sprintf("PART%d", start),
		"offset": start, "size": len(body), "sha1": hex.EncodeToString(digest[:]), "sha512": "ignored"}})
}

func (box *boxEmulator) serveForm(w http.ResponseWriter, r *http.Request) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	box.mu.Lock()
	body := box.uploads[len(box.uploads)-1]
	box.mu.Unlock()
	reader := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
	first, err := reader.NextPart()
	if err != nil || first.FormName() != "attributes" {
		box.t.Errorf("form does not start with attributes: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	attributes, _ := io.ReadAll(first)
	file, err := reader.NextPart()
	if err != nil || file.FormName() != "file" || file.FileName() != "small.bin" {
		box.t.Errorf("form lacks the file part: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	content, _ := io.ReadAll(file)
	if _, err := reader.NextPart(); err != io.EOF {
		box.t.Errorf("form has trailing parts: %v", err)
	}
	if string(attributes) != `{"name":"small.bin","parent":{"id":"0"}}` {
		box.t.Errorf("attributes = %s", attributes)
	}
	digest := sha1.Sum(content)
	if r.Header.Get("Content-MD5") != hex.EncodeToString(digest[:]) {
		writeBoxError(w, http.StatusBadRequest, "bad_digest")
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"total_count": 1, "entries": []map[string]any{{"id": "file-42", "type": "file",
		"sha1": hex.EncodeToString(digest[:]), "size": len(content)}}})
}

func dropConnection(w http.ResponseWriter) {
	connection, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = connection.Close()
	}
}

func writeBoxError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "status": status, "code": code})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func (box *boxEmulator) uploadCount() int {
	box.mu.Lock()
	defer box.mu.Unlock()
	return len(box.uploads)
}

// boxPart is the authenticated_part_v1 destination BeamCore offers for bytes 8-23.
func (box *boxEmulator) boxPart(index int) contracts.TransferPart {
	return contracts.TransferPart{
		Index: index, Length: 16,
		Source: contracts.HTTPEndpoint{URL: box.server.URL + "/source", Headers: map[string]string{"Range": "bytes=8-23"}},
		Destination: contracts.HTTPEndpoint{URL: box.server.URL + "/upload_sessions/session-1", Method: http.MethodPut,
			Headers: map[string]string{"Authorization": "Bearer worker-token", "Content-Range": "bytes 8-23/62"}},
		Protocol: contracts.UploadProtocolAuthenticatedPart, Profile: contracts.UploadProfileBox, MaxAttempts: 3,
		Digest:  &contracts.UploadDigest{Algorithm: "sha1", Header: "Digest", Format: "sha=base64"},
		Receipt: &contracts.UploadReceipt{Path: "part", Fields: []string{"part_id", "offset", "size", "sha1"}},
	}
}

func (box *boxEmulator) boxForm() contracts.TransferPart {
	return contracts.TransferPart{
		Length: 16,
		Source: contracts.HTTPEndpoint{URL: box.server.URL + "/source", Headers: map[string]string{"Range": "bytes=8-23"}},
		Destination: contracts.HTTPEndpoint{URL: box.server.URL + "/files/file-42/content", Method: http.MethodPost,
			Headers: map[string]string{"Authorization": "Bearer worker-token"}},
		Protocol: contracts.UploadProtocolAuthenticatedForm, Profile: contracts.UploadProfileBox, MaxAttempts: 3,
		Digest:  &contracts.UploadDigest{Algorithm: "sha1", Header: "Content-MD5", Format: "hex"},
		Receipt: &contracts.UploadReceipt{Path: "entries.0", Fields: []string{"id", "sha1", "size"}},
		Form:    &contracts.UploadForm{Attributes: `{"name":"small.bin","parent":{"id":"0"}}`, FileField: "file", Filename: "small.bin"},
	}
}

func bufferedSpec(t *testing.T, transfer contracts.MultipartTransfer) domain.Spec {
	t.Helper()
	payload, err := json.Marshal(transfer)
	if err != nil {
		t.Fatal(err)
	}
	return domain.Spec{Kind: domain.KindTransferMultipart, Payload: payload,
		Resources: domain.Resources{MemoryBytes: contracts.TransferBufferOverheadBytes + 16}}
}

type recordedWaits struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (w *recordedWaits) wait(ctx context.Context, delay time.Duration) error {
	w.mu.Lock()
	w.delays = append(w.delays, delay)
	w.mu.Unlock()
	return ctx.Err()
}

func runAuthenticated(t *testing.T, part contracts.TransferPart) (domain.Result, error, *recordedWaits) {
	t.Helper()
	handler := NewHandler(http.DefaultClient)
	waits := &recordedWaits{}
	handler.wait = waits.wait
	spec := bufferedSpec(t, contracts.MultipartTransfer{Parts: []contracts.TransferPart{part}})
	if err := handler.Validate(spec); err != nil {
		t.Fatal(err)
	}
	result, err := handler.Execute(context.Background(), spec)
	return result, err, waits
}

func rangeDigests() (string, string) {
	sha1Digest := sha1.Sum([]byte(boxObject[8:24]))
	sha256Digest := sha256.Sum256([]byte(boxObject[8:24]))
	return hex.EncodeToString(sha1Digest[:]), hex.EncodeToString(sha256Digest[:])
}

func TestAuthenticatedPartSendsDigestAndReportsReceipt(t *testing.T) {
	box := newBoxEmulator(t)
	result, err, waits := runAuthenticated(t, box.boxPart(0))
	if err != nil {
		t.Fatal(err)
	}
	wantSHA1, wantSHA256 := rangeDigests()
	if string(box.uploads[0]) != boxObject[8:24] || box.uploadCount() != 1 || len(waits.delays) != 0 {
		t.Fatalf("uploads=%q waits=%v", box.uploads, waits.delays)
	}
	want := map[string]string{"part.0.sha1": wantSHA1, "part.0.sha256": wantSHA256,
		"part.0.request_count": "1", "part.0.provider_part_id": "PART8"}
	for key, value := range want {
		if result.Outputs[key] != value {
			t.Fatalf("%s = %q, want %q (outputs %+v)", key, result.Outputs[key], value, result.Outputs)
		}
	}
	if result.BytesProcessed != 16 || result.Outputs["part.0.error"] != "" || result.Outputs["part.0.etag"] != "" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestAuthenticatedPartReportsRejectedDigestWithoutRetry(t *testing.T) {
	box := newBoxEmulator(t)
	box.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeBoxError(w, http.StatusBadRequest, "digest_mismatch")
		return true
	}
	result, err, _ := runAuthenticated(t, box.boxPart(0))
	wantSHA1, _ := rangeDigests()
	if err == nil || result.Outputs["part.0.error"] != classProviderDigestRejected || result.Outputs["part.0.request_count"] != "1" ||
		result.Outputs["part.0.sha1"] != wantSHA1 || result.Outputs["part.0.sha256"] != "" || box.uploadCount() != 1 {
		t.Fatalf("err=%v outputs=%+v uploads=%d", err, result.Outputs, box.uploadCount())
	}
}

func TestAuthenticatedPartHonoursRetryAfterWithTheSameBuffer(t *testing.T) {
	box := newBoxEmulator(t)
	box.respond = func(attempt int, w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		if attempt > 1 {
			return false
		}
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	}
	result, err, waits := runAuthenticated(t, box.boxPart(0))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outputs["part.0.request_count"] != "2" || result.Outputs["part.0.provider_part_id"] != "PART8" ||
		len(waits.delays) != 1 || waits.delays[0] != 7*time.Second {
		t.Fatalf("outputs=%+v waits=%v", result.Outputs, waits.delays)
	}
	if box.sourceReads != 1 || string(box.uploads[0]) != string(box.uploads[1]) {
		t.Fatalf("retry did not reuse the buffer: reads=%d uploads=%q", box.sourceReads, box.uploads)
	}
}

func TestAuthenticatedPartThrottlingExhaustsTheAttemptBudget(t *testing.T) {
	box := newBoxEmulator(t)
	box.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	}
	result, err, waits := runAuthenticated(t, box.boxPart(0))
	if err == nil || result.Outputs["part.0.error"] != classProviderThrottled || result.Outputs["part.0.request_count"] != "3" ||
		len(waits.delays) != 2 || result.Outputs["part.0.retry_after_ms"] != "" {
		t.Fatalf("err=%v outputs=%+v waits=%v", err, result.Outputs, waits.delays)
	}
}

func TestAuthenticatedPartStopsWhenRetryAfterOutlastsTheLease(t *testing.T) {
	box := newBoxEmulator(t)
	box.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	}
	handler := NewHandler(http.DefaultClient)
	waits := &recordedWaits{}
	handler.wait = waits.wait
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := handler.Execute(ctx, bufferedSpec(t, contracts.MultipartTransfer{Parts: []contracts.TransferPart{box.boxPart(0)}}))
	if err == nil || result.Outputs["part.0.error"] != classProviderThrottled || result.Outputs["part.0.request_count"] != "1" ||
		len(waits.delays) != 0 || result.Outputs["part.0.retry_after_ms"] != "3600000" {
		t.Fatalf("err=%v outputs=%+v waits=%v", err, result.Outputs, waits.delays)
	}
}

func TestAuthenticatedPartReportsLostAuthorizationWithoutRetry(t *testing.T) {
	box := newBoxEmulator(t)
	part := box.boxPart(0)
	part.Destination.Headers["Authorization"] = "Bearer revoked"
	result, err, _ := runAuthenticated(t, part)
	if err == nil || result.Outputs["part.0.error"] != classAuthorizationLost || result.Outputs["part.0.request_count"] != "1" {
		t.Fatalf("err=%v outputs=%+v", err, result.Outputs)
	}
}

func TestAuthenticatedPartCountsDroppedConnections(t *testing.T) {
	box := newBoxEmulator(t)
	box.respond = func(attempt int, w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		dropConnection(w)
		return true
	}
	part := box.boxPart(0)
	part.MaxAttempts = 2
	result, err, waits := runAuthenticated(t, part)
	if err == nil || result.Outputs["part.0.error"] != classProviderConnectionDropped ||
		result.Outputs["part.0.request_count"] != "2" || box.uploadCount() != 2 || len(waits.delays) != 1 {
		t.Fatalf("err=%v outputs=%+v uploads=%d waits=%v", err, result.Outputs, box.uploadCount(), waits.delays)
	}
}

func TestAuthenticatedPartRejectsAReceiptForOtherBytes(t *testing.T) {
	box := newBoxEmulator(t)
	box.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeJSON(w, map[string]any{"part": map[string]any{"part_id": "PART8", "offset": 8, "size": 16,
			"sha1": strings.Repeat("0", 40)}})
		return true
	}
	result, err, _ := runAuthenticated(t, box.boxPart(0))
	if err == nil || result.Outputs["part.0.error"] != classProviderReceiptMismatch || result.Outputs["part.0.provider_part_id"] != "" {
		t.Fatalf("err=%v outputs=%+v", err, result.Outputs)
	}
	box.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeJSON(w, map[string]any{"part": map[string]any{"part_id": "PART8"}})
		return true
	}
	result, err, _ = runAuthenticated(t, box.boxPart(0))
	if err == nil || result.Outputs["part.0.error"] != classProviderReceiptInvalid {
		t.Fatalf("receipt without declared fields: err=%v outputs=%+v", err, result.Outputs)
	}
}

func TestAuthenticatedPartRejectsShortAndLongSourceReads(t *testing.T) {
	for name, configure := range map[string]func(*boxEmulator){
		"short": func(box *boxEmulator) { box.sourceLength = 15 },
		"long":  func(box *boxEmulator) { box.sourceExtra = "!" },
	} {
		t.Run(name, func(t *testing.T) {
			box := newBoxEmulator(t)
			configure(box)
			result, err, _ := runAuthenticated(t, box.boxPart(0))
			if err == nil || result.Outputs["part.0.error"] != classSourceLengthMismatch ||
				result.Outputs["part.0.request_count"] != "0" || box.uploadCount() != 0 {
				t.Fatalf("err=%v outputs=%+v uploads=%d", err, result.Outputs, box.uploadCount())
			}
		})
	}
}

func TestAuthenticatedPartNeverFollowsRedirects(t *testing.T) {
	box := newBoxEmulator(t)
	box.respond = func(attempt int, w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if attempt == 1 {
			http.Redirect(w, r, "/upload_sessions/elsewhere", http.StatusTemporaryRedirect)
			return true
		}
		return false
	}
	result, err, _ := runAuthenticated(t, box.boxPart(0))
	if err == nil || result.Outputs["part.0.error"] != classProviderError || box.uploadCount() != 1 {
		t.Fatalf("err=%v outputs=%+v uploads=%d", err, result.Outputs, box.uploadCount())
	}
}

func TestAuthenticatedFormSendsAttributesFirstWithHexDigest(t *testing.T) {
	box := newBoxEmulator(t)
	result, err, _ := runAuthenticated(t, box.boxForm())
	if err != nil {
		t.Fatal(err)
	}
	wantSHA1, _ := rangeDigests()
	if result.Outputs["part.0.provider_part_id"] != "file-42" || result.Outputs["part.0.sha1"] != wantSHA1 ||
		result.Outputs["part.0.request_count"] != "1" {
		t.Fatalf("outputs=%+v", result.Outputs)
	}
	box.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeBoxError(w, http.StatusBadRequest, "bad_digest")
		return true
	}
	result, err, _ = runAuthenticated(t, box.boxForm())
	if err == nil || result.Outputs["part.0.error"] != classProviderDigestRejected {
		t.Fatalf("err=%v outputs=%+v", err, result.Outputs)
	}
}

func TestAuthenticatedFanoutReadsOnceForBoxAndPresignedDestinations(t *testing.T) {
	box := newBoxEmulator(t)
	var presigned []byte
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presigned, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"bucket-etag"`)
	}))
	defer bucket.Close()
	boxPart := box.boxPart(0)
	bucketPart := contracts.TransferPart{Index: 1, Length: 16, ETagRequired: true, Source: boxPart.Source,
		Destination: contracts.HTTPEndpoint{URL: bucket.URL + "/object?X-Amz-Signature=sig", Method: http.MethodPut}}
	handler := NewHandler(http.DefaultClient)
	handler.wait = (&recordedWaits{}).wait
	spec := bufferedSpec(t, contracts.MultipartTransfer{Fanout: true, Parts: []contracts.TransferPart{boxPart, bucketPart}})
	if err := handler.Validate(spec); err != nil {
		t.Fatal(err)
	}
	result, err := handler.Execute(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	wantSHA1, wantSHA256 := rangeDigests()
	if box.sourceReads != 1 || string(presigned) != boxObject[8:24] || string(box.uploads[0]) != boxObject[8:24] {
		t.Fatalf("reads=%d box=%q bucket=%q", box.sourceReads, box.uploads, presigned)
	}
	want := map[string]string{
		"part.0.state": "completed", "part.0.sha1": wantSHA1, "part.0.sha256": wantSHA256,
		"part.0.request_count": "1", "part.0.provider_part_id": "PART8", "part.0.bytes": "16",
		"part.1.state": "completed", "part.1.etag": "bucket-etag", "part.1.sha256": wantSHA256,
	}
	for key, value := range want {
		if result.Outputs[key] != value {
			t.Fatalf("%s = %q, want %q (outputs %+v)", key, result.Outputs[key], value, result.Outputs)
		}
	}
	if result.Outputs["part.1.sha1"] != "" || result.Outputs["part.1.request_count"] != "" || result.Outputs["part.0.etag"] != "" {
		t.Fatalf("presigned destination gained authenticated evidence: %+v", result.Outputs)
	}
}

func TestAuthenticatedFanoutReportsTheThrottleRetryAfter(t *testing.T) {
	box := newBoxEmulator(t)
	box.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	}
	handler := NewHandler(http.DefaultClient)
	handler.wait = (&recordedWaits{}).wait
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := handler.Execute(ctx,
		bufferedSpec(t, contracts.MultipartTransfer{Fanout: true, Parts: []contracts.TransferPart{box.boxPart(0)}}))
	if err == nil || result.Outputs["part.0.error"] != classProviderThrottled ||
		result.Outputs["part.0.retry_after_ms"] != "3600000" {
		t.Fatalf("err=%v outputs=%+v", err, result.Outputs)
	}
}

func TestAuthenticatedFanoutReportsPerDestinationFailure(t *testing.T) {
	box := newBoxEmulator(t)
	box.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	handler := NewHandler(http.DefaultClient)
	handler.wait = (&recordedWaits{}).wait
	result, err := handler.Execute(context.Background(),
		bufferedSpec(t, contracts.MultipartTransfer{Fanout: true, Parts: []contracts.TransferPart{box.boxPart(0)}}))
	if err == nil || err.Error() != "fanout_delivery_failed" || result.Outputs["part.0.state"] != "failed" ||
		result.Outputs["part.0.error"] != classAuthorizationLost || result.Outputs["part.0.request_count"] != "1" {
		t.Fatalf("err=%v outputs=%+v", err, result.Outputs)
	}
}

func TestAuthenticatedPartValidation(t *testing.T) {
	box := newBoxEmulator(t)
	for name, mutate := range map[string]func(*contracts.TransferPart){
		"unknown protocol":   func(part *contracts.TransferPart) { part.Protocol = "signed_post_v9" },
		"unknown profile":    func(part *contracts.TransferPart) { part.Profile = "dropbox" },
		"missing attempts":   func(part *contracts.TransferPart) { part.MaxAttempts = 0 },
		"missing digest":     func(part *contracts.TransferPart) { part.Digest = nil },
		"unsupported digest": func(part *contracts.TransferPart) { part.Digest.Algorithm = "md5" },
		"missing receipt":    func(part *contracts.TransferPart) { part.Receipt = nil },
		"buffer over reservation": func(part *contracts.TransferPart) {
			part.Length = 32
			delete(part.Destination.Headers, "Content-Range")
		},
		"misaligned range": func(part *contracts.TransferPart) { part.Destination.Headers["Content-Range"] = "bytes 8-22/62" },
		"plain http":       func(part *contracts.TransferPart) { part.Destination.URL = "http://box.example/upload_sessions/s" },
		"get method":       func(part *contracts.TransferPart) { part.Destination.Method = http.MethodGet },
		"etag required":    func(part *contracts.TransferPart) { part.ETagRequired = true },
		"form on part":     func(part *contracts.TransferPart) { part.Form = &contracts.UploadForm{} },
	} {
		part := box.boxPart(0)
		mutate(&part)
		if err := NewHandler(http.DefaultClient).Validate(bufferedSpec(t, contracts.MultipartTransfer{Parts: []contracts.TransferPart{part}})); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	form := box.boxForm()
	form.Form.Attributes = "{not json"
	if err := NewHandler(http.DefaultClient).Validate(bufferedSpec(t, contracts.MultipartTransfer{Parts: []contracts.TransferPart{form}})); err == nil {
		t.Error("form with invalid attributes was accepted")
	}
}
