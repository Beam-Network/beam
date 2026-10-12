package transfer

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/storagehttp"
)

// Bounded failure classes reported as a destination's error.
const (
	classProviderThrottled         = "provider_throttled"
	classAuthorizationLost         = "authorization_lost"
	classProviderDigestRejected    = "provider_digest_rejected"
	classProviderError             = "provider_error"
	classProviderConnectionDropped = "provider_connection_dropped"
	classProviderReceiptInvalid    = "provider_receipt_invalid"
	classProviderReceiptMismatch   = "provider_receipt_mismatch"
	classDeliveryCancelled         = "delivery_cancelled"
	classSourceReadFailed          = "source_read_failed"
	classSourceLengthMismatch      = "source_length_mismatch"
	classSourceChanged             = "room_source_changed"
)

const authenticatedRetryBackoff = 250 * time.Millisecond

// uploadProfile holds what a provider defines beyond the offer: the receipt field
// that identifies the stored part and the error codes that reject the digest.
type uploadProfile struct {
	partIDField      string
	formIDField      string
	digestRejections []string
}

var uploadProfiles = map[string]uploadProfile{
	contracts.UploadProfileBox: {partIDField: "part_id", formIDField: "id",
		digestRejections: []string{"digest_mismatch", "bad_digest"}},
}

func (profile uploadProfile) idField(protocol string) string {
	if protocol == contracts.UploadProtocolAuthenticatedForm {
		return profile.formIDField
	}
	return profile.partIDField
}

// uploadError carries a bounded class. Its message never includes a URL, header or body.
type uploadError struct {
	class     string
	retryable bool
	delay     time.Duration
	cause     error
}

func (e *uploadError) Error() string { return e.class }
func (e *uploadError) Unwrap() error { return e.cause }

// recordThrottle writes the provider's Retry-After of a throttled delivery, in
// milliseconds, so BeamCore can pause that provider account for every participant.
func recordThrottle(outputs map[string]string, prefix string, err error) {
	var failure *uploadError
	if errors.As(err, &failure) && failure.class == classProviderThrottled && failure.delay > 0 {
		outputs[prefix+"retry_after_ms"] = strconv.FormatInt(failure.delay.Milliseconds(), 10)
	}
}

func uploadFailureClass(err error) string {
	var failure *uploadError
	if errors.As(err, &failure) {
		return failure.class
	}
	return classProviderError
}

// providerDelivery is the evidence of one authenticated destination. requests
// counts every provider HTTP request, including failed and retried ones.
type providerDelivery struct {
	requests       int
	providerPartID string
}

type rangePayload struct {
	bytes  []byte
	sha256 string
	sha1   []byte
}

// readRangePayload reads exactly length bytes and hashes them in the same pass.
func readRangePayload(reader io.Reader, length int64, withSHA1 bool) (rangePayload, error) {
	sha256Hash := sha256.New()
	var sha1Hash hash.Hash
	var hashes io.Writer = sha256Hash
	if withSHA1 {
		sha1Hash = sha1.New()
		hashes = io.MultiWriter(sha1Hash, sha256Hash)
	}
	payload, err := storagehttp.ReadExact(io.TeeReader(reader, hashes), length)
	if err != nil {
		return rangePayload{}, err
	}
	result := rangePayload{bytes: payload, sha256: hex.EncodeToString(sha256Hash.Sum(nil))}
	if sha1Hash != nil {
		result.sha1 = sha1Hash.Sum(nil)
	}
	return result, nil
}

func validateAuthenticatedPart(part contracts.TransferPart, memoryBytes int64) error {
	if _, ok := uploadProfiles[part.Profile]; !ok {
		return fmt.Errorf("unsupported upload profile %q", part.Profile)
	}
	if method := part.Destination.Method; method != http.MethodPut && method != http.MethodPost {
		return errors.New("authenticated destination requires method PUT or POST")
	}
	if part.Length <= 0 || part.Length > memoryBytes-contracts.TransferBufferOverheadBytes {
		return errors.New("authenticated part buffer exceeds reserved memory")
	}
	if part.ETagRequired {
		return errors.New("authenticated destinations report a provider part id, not an etag")
	}
	if part.MaxAttempts < 1 {
		return errors.New("authenticated destination requires max_attempts")
	}
	digest := part.Digest
	if digest == nil || digest.Algorithm != "sha1" || (digest.Format != "sha=base64" && digest.Format != "hex") ||
		digest.Header == "" || strings.ContainsAny(digest.Header, " \t\r\n:") {
		return errors.New("authenticated destination requires a sha1 digest header in sha=base64 or hex format")
	}
	if part.Receipt == nil {
		return errors.New("authenticated destination requires a receipt")
	}
	parsed, err := url.Parse(part.Destination.URL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("invalid authenticated destination endpoint")
	}
	if ip := net.ParseIP(parsed.Hostname()); parsed.Scheme != "https" && !(parsed.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return errors.New("authenticated destinations require TLS outside loopback")
	}
	if part.Protocol == contracts.UploadProtocolAuthenticatedForm {
		form := part.Form
		if form == nil || form.FileField == "" || form.Filename == "" || !json.Valid([]byte(form.Attributes)) {
			return errors.New("authenticated form requires JSON attributes, a file field and a filename")
		}
		return nil
	}
	if part.Form != nil {
		return errors.New("authenticated part must not declare a form")
	}
	for key, value := range part.Destination.Headers {
		if strings.EqualFold(key, "Content-Range") {
			if span, ok := contentRangeSpan(value); !ok || span != part.Length {
				return errors.New("authenticated part Content-Range does not match its length")
			}
		}
	}
	return nil
}

func contentRangeSpan(value string) (int64, bool) {
	var start, end, total int64
	if _, err := fmt.Sscanf(value, "bytes %d-%d/%d", &start, &end, &total); err != nil ||
		start < 0 || end < start || end >= total {
		return 0, false
	}
	return end - start + 1, true
}

// executeAuthenticatedPart buffers one source range, then uploads it with its digest.
func (h *Handler) executeAuthenticatedPart(ctx context.Context, part contracts.TransferPart) (partResult, error) {
	response, err := h.openSource(ctx, part)
	if err != nil {
		return partResult{}, &uploadError{class: classSourceReadFailed, cause: err}
	}
	payload, err := readRangePayload(response.Body, part.Length, true)
	_ = response.Body.Close()
	if err != nil {
		return partResult{}, &uploadError{class: classSourceLengthMismatch, cause: err}
	}
	if part.ExpectedSHA256 != "" && !strings.EqualFold(part.ExpectedSHA256, payload.sha256) {
		return partResult{}, &uploadError{class: classSourceChanged}
	}
	client := *h.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	delivery, err := h.uploadAuthenticated(ctx, &client, part, payload)
	result := partResult{sha1: hex.EncodeToString(payload.sha1), requests: delivery.requests}
	if err != nil {
		return result, err
	}
	result.bytes, result.sha256, result.providerPartID = part.Length, payload.sha256, delivery.providerPartID
	return result, nil
}

// uploadAuthenticated writes the retained buffer to an authenticated destination.
// Every attempt resends the same bytes and digest; the client must not follow redirects.
func (h *Handler) uploadAuthenticated(ctx context.Context, client *http.Client, part contracts.TransferPart,
	payload rangePayload) (providerDelivery, error) {
	var delivery providerDelivery
	profile := uploadProfiles[part.Profile]
	body, err := newUploadBody(part)
	if err != nil {
		return delivery, &uploadError{class: classProviderError, cause: err}
	}
	digest := hex.EncodeToString(payload.sha1)
	if part.Digest.Format == "sha=base64" {
		digest = "sha=" + base64.StdEncoding.EncodeToString(payload.sha1)
	}
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return delivery, &uploadError{class: classDeliveryCancelled, cause: err}
		}
		request, err := body.request(ctx, part, payload.bytes, digest)
		if err != nil {
			return delivery, &uploadError{class: classProviderError, cause: err}
		}
		delivery.requests++
		response, err := client.Do(request)
		var failure *uploadError
		if err != nil {
			if ctx.Err() != nil {
				return delivery, &uploadError{class: classDeliveryCancelled, cause: ctx.Err()}
			}
			failure = &uploadError{class: classProviderConnectionDropped, retryable: true, cause: err}
		} else {
			delivery.providerPartID, failure = readProviderResponse(response, part, profile, hex.EncodeToString(payload.sha1))
			if failure == nil {
				return delivery, nil
			}
		}
		if !failure.retryable || attempt >= part.MaxAttempts {
			return delivery, failure
		}
		delay := failure.delay
		if delay <= 0 {
			delay = time.Duration(attempt) * authenticatedRetryBackoff
		}
		// A wait that outlasts the lease cannot lead to an accepted attempt.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return delivery, failure
		}
		if err := h.wait(ctx, delay); err != nil {
			return delivery, &uploadError{class: classDeliveryCancelled, cause: err}
		}
	}
}

type uploadBody struct {
	prefix, suffix []byte
	contentType    string
}

// newUploadBody builds the in-memory multipart envelope of a form upload: the
// attributes part first, then the file part. The payload itself is never copied.
func newUploadBody(part contracts.TransferPart) (uploadBody, error) {
	if part.Protocol != contracts.UploadProtocolAuthenticatedForm {
		return uploadBody{}, nil
	}
	var prefix bytes.Buffer
	writer := multipart.NewWriter(&prefix)
	if err := writer.WriteField("attributes", part.Form.Attributes); err != nil {
		return uploadBody{}, err
	}
	if _, err := writer.CreateFormFile(part.Form.FileField, part.Form.Filename); err != nil {
		return uploadBody{}, err
	}
	return uploadBody{prefix: prefix.Bytes(), suffix: []byte("\r\n--" + writer.Boundary() + "--\r\n"),
		contentType: writer.FormDataContentType()}, nil
}

func (body uploadBody) request(ctx context.Context, part contracts.TransferPart, payload []byte, digest string) (*http.Request, error) {
	var reader io.Reader = bytes.NewReader(payload)
	length := int64(len(payload))
	if body.contentType != "" {
		reader = io.MultiReader(bytes.NewReader(body.prefix), reader, bytes.NewReader(body.suffix))
		length += int64(len(body.prefix) + len(body.suffix))
	}
	request, err := http.NewRequestWithContext(ctx, part.Destination.Method, part.Destination.URL, reader)
	if err != nil {
		return nil, err
	}
	// The transport must not replay the body on its own, so request_count stays exact.
	request.GetBody = nil
	request.ContentLength = length
	copyHeaders(request.Header, part.Destination.Headers)
	if body.contentType != "" {
		request.Header.Set("Content-Type", body.contentType)
	} else if request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	request.Header.Set(part.Digest.Header, digest)
	return request, nil
}

// readProviderResponse classifies one provider response and, on success, returns
// the stored part identity from a receipt whose sha1 matches the sent bytes.
func readProviderResponse(response *http.Response, part contracts.TransferPart, profile uploadProfile,
	sha1Hex string) (string, *uploadError) {
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, defaultResponseBodyLimit+1))
	status := response.StatusCode
	switch {
	case status >= 200 && status < 300:
		if readErr != nil {
			return "", &uploadError{class: classProviderConnectionDropped, retryable: true, cause: readErr}
		}
		if len(body) > defaultResponseBodyLimit {
			return "", &uploadError{class: classProviderReceiptInvalid}
		}
		return parseReceipt(body, *part.Receipt, profile.idField(part.Protocol), sha1Hex)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "", &uploadError{class: classAuthorizationLost}
	case status == http.StatusTooManyRequests:
		return "", &uploadError{class: classProviderThrottled, retryable: true, delay: retryAfter(response.Header.Get("Retry-After"))}
	case status == http.StatusRequestTimeout || status >= 500:
		return "", &uploadError{class: classProviderError, retryable: true, delay: retryAfter(response.Header.Get("Retry-After"))}
	case status == http.StatusBadRequest:
		var providerError struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(body, &providerError) == nil && slices.Contains(profile.digestRejections, providerError.Code) {
			return "", &uploadError{class: classProviderDigestRejected}
		}
	}
	return "", &uploadError{class: classProviderError}
}

func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at), 0)
	}
	return 0
}

// parseReceipt reads the receipt object at its dot path. Every declared field must
// be present, a sha1 must equal the sent bytes, and the profile's id field names the part.
func parseReceipt(body []byte, receipt contracts.UploadReceipt, idField, sha1Hex string) (string, *uploadError) {
	invalid := &uploadError{class: classProviderReceiptInvalid}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var node any
	if err := decoder.Decode(&node); err != nil {
		return "", invalid
	}
	if receipt.Path != "" {
		for _, segment := range strings.Split(receipt.Path, ".") {
			switch value := node.(type) {
			case map[string]any:
				node = value[segment]
			case []any:
				index, err := strconv.Atoi(segment)
				if err != nil || index < 0 || index >= len(value) {
					return "", invalid
				}
				node = value[index]
			default:
				return "", invalid
			}
		}
	}
	object, ok := node.(map[string]any)
	if !ok {
		return "", invalid
	}
	for _, field := range receipt.Fields {
		if object[field] == nil {
			return "", invalid
		}
	}
	if value, present := object["sha1"]; present {
		if reported, ok := value.(string); !ok || !strings.EqualFold(reported, sha1Hex) {
			return "", &uploadError{class: classProviderReceiptMismatch}
		}
	}
	var id string
	switch value := object[idField].(type) {
	case string:
		id = value
	case json.Number:
		id = value.String()
	}
	if id == "" {
		return "", invalid
	}
	return id, nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
