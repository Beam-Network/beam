package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func validateSourceGroup(transfer contracts.MultipartTransfer, spec domain.Spec) error {
	if len(transfer.Parts) == 0 || transfer.DestinationConcurrency < 1 || transfer.DestinationConcurrency > 8 {
		return errors.New("invalid source group concurrency")
	}
	first := transfer.Parts[0]
	if first.Length <= 0 || first.Length > spec.Resources.MemoryBytes-(4<<20) {
		return errors.New("source group buffer exceeds reserved memory")
	}
	for index, part := range transfer.Parts {
		if part.Index != index || part.TaskID == "" || part.OfferID == "" || part.Length != first.Length ||
			part.Offset != first.Offset || part.SourceRange != first.SourceRange || !reflect.DeepEqual(part.Source, first.Source) ||
			part.ExpectedSHA256 != first.ExpectedSHA256 {
			return errors.New("source group does not describe one immutable range")
		}
		for _, endpoint := range []contracts.HTTPEndpoint{part.Source, part.Destination} {
			parsed, err := url.Parse(endpoint.URL)
			if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
				return errors.New("invalid source group endpoint")
			}
			ip := net.ParseIP(parsed.Hostname())
			if parsed.Scheme != "https" && !(parsed.Scheme == "http" && ip != nil && ip.IsLoopback()) {
				return errors.New("source group endpoints require TLS outside loopback")
			}
		}
	}
	return nil
}

// The buffer belongs to a logical range, never to a destination. A slow or
// retrying destination holds this one buffer while other bounded slots advance.
func (h *Handler) executeSourceGroup(ctx context.Context, spec domain.Spec, transfer contracts.MultipartTransfer) (domain.Result, error) {
	if err := validateSourceGroup(transfer, spec); err != nil {
		return domain.Result{}, err
	}
	client := *h.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	first := transfer.Parts[0]
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, first.Source.URL, nil)
	if err != nil {
		return domain.Result{}, errors.New("source_request_invalid")
	}
	copyHeaders(request.Header, first.Source.Headers)
	if first.SourceRange {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", first.Offset, first.Offset+first.Length-1))
	}
	response, err := client.Do(request)
	if err != nil {
		return domain.Result{}, errors.New("source_read_failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPreconditionFailed {
		return domain.Result{}, errors.New("room_source_changed")
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return domain.Result{}, fmt.Errorf("source_http_%d", response.StatusCode)
	}
	if requested := request.Header.Get("Range"); requested != "" {
		var start, end int64
		if _, err := fmt.Sscanf(requested, "bytes=%d-%d", &start, &end); err != nil || response.StatusCode != http.StatusPartialContent ||
			!strings.HasPrefix(response.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-%d/", start, end)) || end-start+1 != first.Length {
			return domain.Result{}, errors.New("source_range_mismatch")
		}
	}
	payload := make([]byte, first.Length)
	read, err := io.ReadFull(response.Body, payload)
	outputs := map[string]string{"source.read_count": "1", "source.payload_bytes": strconv.Itoa(read), "source.group_id": transfer.SourceGroupID}
	var extra [1]byte
	if err == nil {
		var count int
		count, err = response.Body.Read(extra[:])
		outputs["source.payload_bytes"] = strconv.Itoa(read + count)
		if count > 0 {
			err = errors.New("excess source bytes")
		} else if errors.Is(err, io.EOF) {
			err = nil
		}
	}
	if err != nil || int64(read) != first.Length {
		return domain.Result{Outputs: outputs}, errors.New("source_length_mismatch")
	}
	digest := sha256.Sum256(payload)
	checksum := hex.EncodeToString(digest[:])
	if first.ExpectedSHA256 != "" && !strings.EqualFold(first.ExpectedSHA256, checksum) {
		return domain.Result{Outputs: outputs}, errors.New("room_source_changed")
	}
	queue := make(chan contracts.TransferPart)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var total int64
	failed := false
	for slot := 0; slot < transfer.DestinationConcurrency; slot++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for part := range queue {
				etag, writeErr := writeFanoutDestination(ctx, &client, part, payload)
				mu.Lock()
				prefix := fmt.Sprintf("part.%d.", part.Index)
				if writeErr != nil {
					failed = true
					outputs[prefix+"error"] = writeErr.Error()
				} else {
					total += part.Length
					outputs[prefix+"sha256"], outputs[prefix+"etag"] = checksum, etag
					outputs[prefix+"bytes"], outputs[prefix+"state"] = strconv.FormatInt(part.Length, 10), "completed"
				}
				mu.Unlock()
			}
		}()
	}
	for _, part := range transfer.Parts {
		queue <- part
	}
	close(queue)
	wg.Wait()
	result := domain.Result{BytesProcessed: total, Outputs: outputs}
	if failed {
		return result, errors.New("source_group_delivery_failed")
	}
	return result, nil
}

func writeFanoutDestination(ctx context.Context, client *http.Client, part contracts.TransferPart, payload []byte) (string, error) {
	method := part.Destination.Method
	if method == "" {
		method = http.MethodPut
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", errors.New("delivery_cancelled")
		}
		request, err := http.NewRequestWithContext(ctx, method, part.Destination.URL, bytes.NewReader(payload))
		if err != nil {
			return "", errors.New("destination_request_invalid")
		}
		copyHeaders(request.Header, part.Destination.Headers)
		response, err := client.Do(request)
		code := 0
		if err == nil {
			code = response.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, defaultResponseBodyLimit))
			_ = response.Body.Close()
			if code >= 200 && code < 300 {
				etag := strings.Trim(response.Header.Get("ETag"), `"`)
				if part.ETagRequired && etag == "" {
					return "", errors.New("destination_etag_missing")
				}
				return etag, nil
			}
			if code != 408 && code != 429 && code < 500 {
				return "", fmt.Errorf("destination_http_%d", code)
			}
		}
		if attempt == 2 {
			return "", fmt.Errorf("destination_delivery_failed_http_%d", code)
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", errors.New("delivery_cancelled")
		case <-timer.C:
		}
	}
	return "", errors.New("destination_delivery_failed")
}
