package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/storagehttp"
)

const defaultResponseBodyLimit = 64 << 10
const multipartCheckpointSchema = "beam.transfer.multipart/1"
const defaultMaxPartAttempts = 3
const defaultInitialBackoff = 100 * time.Millisecond
const defaultMaxBackoff = 2 * time.Second
const defaultMinDeadlineBudget = 1 * time.Second

type multipartCheckpoint struct {
	Parts map[string]partCheckpoint `json:"parts"`
	Bytes int64                     `json:"bytes"`
}

type partCheckpoint struct {
	Bytes          int64  `json:"bytes"`
	SHA256         string `json:"sha256"`
	ETag           string `json:"etag,omitempty"`
	SHA1           string `json:"sha1,omitempty"`
	RequestCount   int    `json:"request_count,omitempty"`
	ProviderPartID string `json:"provider_part_id,omitempty"`
}

type Handler struct {
	client            *http.Client
	maxAttempts       int
	initialBackoff    time.Duration
	maxBackoff        time.Duration
	minDeadlineBudget time.Duration
	randDuration      func(time.Duration) time.Duration
	// wait pauses between authenticated upload attempts.
	wait func(context.Context, time.Duration) error
}

type HandlerOption func(*Handler)

func WithRetryConfig(maxAttempts int, initialBackoff, maxBackoff, minDeadlineBudget time.Duration) HandlerOption {
	return func(h *Handler) {
		h.maxAttempts = maxAttempts
		h.initialBackoff = initialBackoff
		h.maxBackoff = maxBackoff
		h.minDeadlineBudget = minDeadlineBudget
	}
}

func WithRandDuration(fn func(time.Duration) time.Duration) HandlerOption {
	return func(h *Handler) {
		h.randDuration = fn
	}
}

func NewHandler(client *http.Client, opts ...HandlerOption) *Handler {
	if client == nil {
		client = storagehttp.NewClient(30 * time.Minute)
	}
	h := &Handler{client: client, wait: sleepContext}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *Handler) maxAttemptsCount() int {
	if h.maxAttempts > 0 {
		return h.maxAttempts
	}
	return defaultMaxPartAttempts
}

func (h *Handler) initialBackoffDuration() time.Duration {
	if h.initialBackoff > 0 {
		return h.initialBackoff
	}
	return defaultInitialBackoff
}

func (h *Handler) maxBackoffDuration() time.Duration {
	if h.maxBackoff > 0 {
		return h.maxBackoff
	}
	return defaultMaxBackoff
}

func (h *Handler) minDeadlineBudgetDuration() time.Duration {
	if h.minDeadlineBudget > 0 {
		return h.minDeadlineBudget
	}
	return defaultMinDeadlineBudget
}

func (h *Handler) rand(n time.Duration) time.Duration {
	if n <= 0 {
		return 0
	}
	var r time.Duration
	if h != nil && h.randDuration != nil {
		r = h.randDuration(n)
	} else {
		r = defaultRandDuration(n)
	}
	if r < 0 {
		return 0
	}
	if r > n {
		return n
	}
	return r
}

func defaultRandDuration(n time.Duration) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(n)))
}

func (h *Handler) calculateBackoff(attempt int, retryAfter time.Duration) time.Duration {
	maxBackoff := h.maxBackoffDuration()
	if retryAfter > 0 {
		if retryAfter > maxBackoff {
			return maxBackoff
		}
		return retryAfter
	}
	base := h.initialBackoffDuration()
	for i := 1; i < attempt; i++ {
		base *= 2
		if base >= maxBackoff {
			base = maxBackoff
			break
		}
	}
	if base > maxBackoff {
		base = maxBackoff
	}
	if base <= 0 {
		return 0
	}
	half := base / 2
	jitterRange := base - half
	return half + h.rand(jitterRange)
}

func (h *Handler) Kind() domain.Kind { return domain.KindTransferMultipart }

func (h *Handler) Validate(spec domain.Spec) error {
	var transfer contracts.MultipartTransfer
	if err := json.Unmarshal(spec.Payload, &transfer); err != nil {
		return fmt.Errorf("decode multipart transfer: %w", err)
	}
	if len(transfer.Parts) == 0 {
		return errors.New("at least one transfer part is required")
	}
	if transfer.Fanout {
		if err := validateFanout(transfer, spec); err != nil {
			return err
		}
	}
	seen := make(map[int]struct{}, len(transfer.Parts))
	for _, part := range transfer.Parts {
		if _, exists := seen[part.Index]; exists {
			return fmt.Errorf("duplicate transfer part index %d", part.Index)
		}
		seen[part.Index] = struct{}{}
		if err := validateEndpoint("source", part.Source); err != nil {
			return fmt.Errorf("part %d: %w", part.Index, err)
		}
		if err := validateEndpoint("destination", part.Destination); err != nil {
			return fmt.Errorf("part %d: %w", part.Index, err)
		}
		if part.Offset < 0 || part.Length < 0 {
			return fmt.Errorf("part %d: offset and length cannot be negative", part.Index)
		}
		if part.ExpectedSHA256 != "" {
			digest, err := hex.DecodeString(part.ExpectedSHA256)
			if err != nil || len(digest) != sha256.Size {
				return fmt.Errorf("part %d: expected_sha256 must be a hexadecimal SHA-256 digest", part.Index)
			}
		}
		switch {
		case part.Authenticated():
			if err := validateAuthenticatedPart(part, spec.Resources.MemoryBytes); err != nil {
				return fmt.Errorf("part %d: %w", part.Index, err)
			}
		case part.Protocol != "" && part.Protocol != contracts.UploadProtocolPresignedPut:
			return fmt.Errorf("part %d: unsupported destination protocol %q", part.Index, part.Protocol)
		}
	}
	return nil
}

func (h *Handler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	if err := h.Validate(spec); err != nil {
		return domain.Result{}, err
	}
	var transfer contracts.MultipartTransfer
	if err := json.Unmarshal(spec.Payload, &transfer); err != nil {
		return domain.Result{}, err
	}
	if transfer.Fanout {
		return h.executeFanout(ctx, transfer)
	}
	resume := multipartCheckpoint{Parts: make(map[string]partCheckpoint)}
	if _, _, err := workloadcheckpoint.Current(ctx, multipartCheckpointSchema, &resume); err != nil {
		return domain.Result{}, fmt.Errorf("restore multipart checkpoint: %w", err)
	}
	if resume.Parts == nil {
		resume.Parts = make(map[string]partCheckpoint)
	}
	outputs := make(map[string]string, len(transfer.Parts)*2)
	total := resume.Bytes

	remainingParts := 0
	for _, p := range transfer.Parts {
		if _, ok := resume.Parts[strconv.Itoa(p.Index)]; !ok {
			remainingParts++
		}
	}

	for _, part := range transfer.Parts {
		partKey := strconv.Itoa(part.Index)
		prefix := "part." + partKey + "."
		if completed, ok := resume.Parts[partKey]; ok {
			partResult{sha256: completed.SHA256, etag: completed.ETag, sha1: completed.SHA1,
				requests: completed.RequestCount, providerPartID: completed.ProviderPartID}.record(outputs, prefix, part)
			continue
		}
		partResult, err := h.executePart(ctx, part, remainingParts)
		if err != nil {
			if part.Authenticated() {
				partResult.record(outputs, prefix, part)
				outputs[prefix+"error"] = uploadFailureClass(err)
				recordThrottle(outputs, prefix, err)
			}
			return domain.Result{BytesProcessed: total, Outputs: outputs}, fmt.Errorf("part %d: %w", part.Index, err)
		}
		remainingParts--
		total += partResult.bytes
		partResult.record(outputs, prefix, part)
		resume.Bytes = total
		resume.Parts[partKey] = partCheckpoint{Bytes: partResult.bytes, SHA256: partResult.sha256, ETag: partResult.etag,
			SHA1: partResult.sha1, RequestCount: partResult.requests, ProviderPartID: partResult.providerPartID}
		if err := workloadcheckpoint.Save(ctx, multipartCheckpointSchema, map[string]string{
			"completed_part": partKey,
		}, resume); err != nil && !errors.Is(err, workloadcheckpoint.ErrUnavailable) {
			return domain.Result{BytesProcessed: total, Outputs: outputs}, fmt.Errorf("save multipart checkpoint: %w", err)
		}
	}
	return domain.Result{BytesProcessed: total, Outputs: outputs}, nil
}

type partResult struct {
	bytes          int64
	sha256         string
	etag           string
	sha1           string
	requests       int
	providerPartID string
}

// record writes a part's evidence outputs. Authenticated destinations always report
// their request count, and their sha1 once the range was read.
func (r partResult) record(outputs map[string]string, prefix string, part contracts.TransferPart) {
	if r.sha256 != "" {
		outputs[prefix+"sha256"] = r.sha256
	}
	if r.etag != "" {
		outputs[prefix+"etag"] = r.etag
	}
	if !part.Authenticated() {
		return
	}
	outputs[prefix+"request_count"] = strconv.Itoa(r.requests)
	if r.sha1 != "" {
		outputs[prefix+"sha1"] = r.sha1
	}
	if r.providerPartID != "" {
		outputs[prefix+"provider_part_id"] = r.providerPartID
	}
}

type presignedAttemptResult struct {
	result     partResult
	retryable  bool
	retryAfter time.Duration
	err        error
}

func (h *Handler) executePart(ctx context.Context, part contracts.TransferPart, remainingParts int) (partResult, error) {
	if part.Authenticated() {
		return h.executeAuthenticatedPart(ctx, part)
	}

	maxAttempts := h.maxAttemptsCount()
	minBudget := h.minDeadlineBudgetDuration()
	if remainingParts <= 0 {
		remainingParts = 1
	}
	budgetNeeded := time.Duration(remainingParts) * minBudget

	var lastErr error
	var nextRetryAfter time.Duration
	var lastBytes int64

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Context cancellation check
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return partResult{bytes: lastBytes}, fmt.Errorf("%w: %v", err, lastErr)
			}
			return partResult{bytes: lastBytes}, err
		}

		// Deadline budget check
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining < budgetNeeded {
				if lastErr != nil {
					return partResult{bytes: lastBytes}, fmt.Errorf("insufficient deadline budget (%v remaining < %v required): %w", remaining, budgetNeeded, lastErr)
				}
				return partResult{bytes: lastBytes}, fmt.Errorf("insufficient deadline budget (%v remaining < %v required)", remaining, budgetNeeded)
			}
		}

		// Backoff delay for retries
		if attempt > 0 {
			delay := h.calculateBackoff(attempt, nextRetryAfter)
			nextRetryAfter = 0

			if deadline, ok := ctx.Deadline(); ok {
				if time.Until(deadline)-delay < budgetNeeded {
					return partResult{bytes: lastBytes}, fmt.Errorf("insufficient deadline budget for retry backoff: %w", lastErr)
				}
			}

			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return partResult{bytes: lastBytes}, ctx.Err()
			case <-timer.C:
			}
		}

		// Attempt execution
		att := h.transferPresignedAttempt(ctx, part)
		if att.err == nil {
			return att.result, nil
		}

		lastErr = att.err
		lastBytes = att.result.bytes
		if !att.retryable || attempt == maxAttempts-1 {
			return att.result, att.err
		}
		nextRetryAfter = att.retryAfter
	}

	return partResult{bytes: lastBytes}, lastErr
}

func (h *Handler) transferPresignedAttempt(ctx context.Context, part contracts.TransferPart) presignedAttemptResult {
	sourceRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, part.Source.URL, nil)
	if err != nil {
		return presignedAttemptResult{err: fmt.Errorf("create source request: %w", err), retryable: false}
	}
	copyHeaders(sourceRequest.Header, part.Source.Headers)
	if part.SourceRange && part.Length > 0 {
		sourceRequest.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", part.Offset, part.Offset+part.Length-1))
	}
	sourceResponse, err := h.client.Do(sourceRequest)
	if err != nil {
		return presignedAttemptResult{err: fmt.Errorf("fetch source: %w", err), retryable: isTransientNetworkError(err)}
	}
	if sourceResponse.StatusCode != http.StatusOK && sourceResponse.StatusCode != http.StatusPartialContent {
		defer sourceResponse.Body.Close()
		retryable := isRetryableHTTPStatus(sourceResponse.StatusCode)
		retryAfter := parseRetryAfter(sourceResponse.Header, h.maxBackoffDuration())
		return presignedAttemptResult{
			err:        responseError("source", sourceResponse),
			retryable:  retryable,
			retryAfter: retryAfter,
		}
	}
	if part.SourceRange && part.Length > 0 && sourceResponse.StatusCode != http.StatusPartialContent {
		_ = sourceResponse.Body.Close()
		return presignedAttemptResult{err: fmt.Errorf("source ignored required byte range: HTTP %d", sourceResponse.StatusCode), retryable: false}
	}
	defer sourceResponse.Body.Close()

	hash := sha256.New()
	var source io.Reader = sourceResponse.Body
	if part.Length > 0 {
		source = io.LimitReader(source, part.Length)
	}
	counter := &countingReader{reader: io.TeeReader(source, hash)}
	destinationMethod := strings.ToUpper(strings.TrimSpace(part.Destination.Method))
	if destinationMethod == "" {
		destinationMethod = http.MethodPut
	}
	destinationRequest, err := http.NewRequestWithContext(ctx, destinationMethod, part.Destination.URL, counter)
	if err != nil {
		return presignedAttemptResult{err: fmt.Errorf("create destination request: %w", err), retryable: false}
	}
	copyHeaders(destinationRequest.Header, part.Destination.Headers)
	if destinationRequest.Header.Get("Content-Type") == "" {
		destinationRequest.Header.Set("Content-Type", "application/octet-stream")
	}
	if part.Length > 0 {
		destinationRequest.ContentLength = part.Length
	} else if sourceResponse.ContentLength >= 0 {
		destinationRequest.ContentLength = sourceResponse.ContentLength
	}
	destinationResponse, err := h.client.Do(destinationRequest)
	if err != nil {
		return presignedAttemptResult{
			result:    partResult{bytes: counter.count},
			err:       fmt.Errorf("write destination: %w", err),
			retryable: isTransientNetworkError(err),
		}
	}
	defer destinationResponse.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(destinationResponse.Body, defaultResponseBodyLimit))
	if destinationResponse.StatusCode < 200 || destinationResponse.StatusCode >= 300 {
		retryable := isRetryableHTTPStatus(destinationResponse.StatusCode)
		retryAfter := parseRetryAfter(destinationResponse.Header, h.maxBackoffDuration())
		return presignedAttemptResult{
			result:     partResult{bytes: counter.count},
			err:        fmt.Errorf("destination returned HTTP %d", destinationResponse.StatusCode),
			retryable:  retryable,
			retryAfter: retryAfter,
		}
	}
	if part.Length > 0 && counter.count != part.Length {
		return presignedAttemptResult{
			result:    partResult{bytes: counter.count},
			err:       fmt.Errorf("source returned %d bytes, expected %d", counter.count, part.Length),
			retryable: false,
		}
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if part.ExpectedSHA256 != "" && !strings.EqualFold(digest, part.ExpectedSHA256) {
		return presignedAttemptResult{
			result:    partResult{bytes: counter.count, sha256: digest},
			err:       fmt.Errorf("SHA-256 mismatch: got %s", digest),
			retryable: false,
		}
	}
	return presignedAttemptResult{
		result: partResult{
			bytes:  counter.count,
			sha256: digest,
			etag:   strings.Trim(destinationResponse.Header.Get("ETag"), `"`),
		},
		retryable: false,
		err:       nil,
	}
}

func isRetryableHTTPStatus(code int) bool {
	return code == http.StatusTooManyRequests ||
		code == http.StatusInternalServerError ||
		code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

func isTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false
}

func parseRetryAfter(header http.Header, maxDelay time.Duration) time.Duration {
	val := strings.TrimSpace(header.Get("Retry-After"))
	if val == "" {
		return 0
	}
	if secs, err := strconv.ParseUint(val, 10, 32); err == nil {
		d := time.Duration(secs) * time.Second
		if d > maxDelay {
			return maxDelay
		}
		return d
	}
	if t, err := http.ParseTime(val); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		if d > maxDelay {
			return maxDelay
		}
		return d
	}
	return 0
}

// openSource returns a successful source response; the caller closes its body.
func (h *Handler) openSource(ctx context.Context, part contracts.TransferPart) (*http.Response, error) {
	sourceRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, part.Source.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("create source request: %w", err)
	}
	copyHeaders(sourceRequest.Header, part.Source.Headers)
	if part.SourceRange && part.Length > 0 {
		sourceRequest.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", part.Offset, part.Offset+part.Length-1))
	}
	sourceResponse, err := h.client.Do(sourceRequest)
	if err != nil {
		return nil, fmt.Errorf("fetch source: %w", err)
	}
	if sourceResponse.StatusCode != http.StatusOK && sourceResponse.StatusCode != http.StatusPartialContent {
		defer sourceResponse.Body.Close()
		return nil, responseError("source", sourceResponse)
	}
	if part.SourceRange && part.Length > 0 && sourceResponse.StatusCode != http.StatusPartialContent {
		_ = sourceResponse.Body.Close()
		return nil, fmt.Errorf("source ignored required byte range: HTTP %d", sourceResponse.StatusCode)
	}
	return sourceResponse, nil
}

func validateEndpoint(label string, endpoint contracts.HTTPEndpoint) error {
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s endpoint must be an absolute HTTP(S) URL", label)
	}
	return nil
}

func responseError(label string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, defaultResponseBodyLimit))
	message := strings.TrimSpace(string(body))
	if message == "" {
		return fmt.Errorf("%s returned HTTP %d", label, response.StatusCode)
	}
	return fmt.Errorf("%s returned HTTP %d: %s", label, response.StatusCode, message)
}

func copyHeaders(destination http.Header, source map[string]string) {
	for key, value := range source {
		destination.Set(key, value)
	}
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.count += int64(n)
	return n, err
}
