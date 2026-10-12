package transfer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/storagehttp"
)

func validateFanout(transfer contracts.MultipartTransfer, spec domain.Spec) error {
	if len(transfer.Parts) == 0 {
		return errors.New("fan-out transfer has no destinations")
	}
	first := transfer.Parts[0]
	if first.Length <= 0 || first.Length > spec.Resources.MemoryBytes-contracts.TransferBufferOverheadBytes {
		return errors.New("fan-out buffer exceeds reserved memory")
	}
	for index, part := range transfer.Parts {
		if part.Index != index || part.Length != first.Length ||
			part.Offset != first.Offset || part.SourceRange != first.SourceRange || !reflect.DeepEqual(part.Source, first.Source) ||
			part.ExpectedSHA256 != first.ExpectedSHA256 {
			return errors.New("fan-out transfer does not describe one immutable range")
		}
		for _, endpoint := range []contracts.HTTPEndpoint{part.Source, part.Destination} {
			parsed, err := url.Parse(endpoint.URL)
			if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
				return errors.New("invalid fan-out endpoint")
			}
			ip := net.ParseIP(parsed.Hostname())
			if parsed.Scheme != "https" && !(parsed.Scheme == "http" && ip != nil && ip.IsLoopback()) {
				return errors.New("fan-out endpoints require TLS outside loopback")
			}
		}
	}
	return nil
}

// The buffer belongs to a logical range, never to a destination. A slow or
// retrying destination holds this one buffer while other bounded slots advance.
func (h *Handler) executeFanout(ctx context.Context, transfer contracts.MultipartTransfer) (domain.Result, error) {
	var resumed contracts.FanoutCheckpoint
	if _, ok, err := workloadcheckpoint.Current(ctx, contracts.FanoutCheckpointSchema, &resumed); err != nil {
		return domain.Result{}, err
	} else if ok {
		result := domain.Result{BytesProcessed: resumed.Bytes, Outputs: resumed.Outputs}
		for _, part := range transfer.Parts {
			if resumed.Outputs[fmt.Sprintf("part.%d.state", part.Index)] != "completed" {
				// The old buffer is gone. Return proven deliveries and let Runtime
				// assign only missing cells under a new fenced attempt.
				return result, errors.New("fanout_worker_restarted")
			}
		}
		return result, nil
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
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
	response, err := storagehttp.Get(&client, request)
	if err != nil {
		return domain.Result{}, errors.New(classSourceReadFailed)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPreconditionFailed {
		return domain.Result{}, errors.New(classSourceChanged)
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
	authenticated := slices.ContainsFunc(transfer.Parts, contracts.TransferPart.Authenticated)
	payload, err := readRangePayload(response.Body, first.Length, authenticated)
	outputs := map[string]string{}
	if err != nil {
		return domain.Result{Outputs: outputs}, errors.New(classSourceLengthMismatch)
	}
	checksum := payload.sha256
	if first.ExpectedSHA256 != "" && !strings.EqualFold(first.ExpectedSHA256, checksum) {
		return domain.Result{Outputs: outputs}, errors.New(classSourceChanged)
	}
	checkpoint := func(total int64) error {
		err := workloadcheckpoint.Save(ctx, contracts.FanoutCheckpointSchema, nil,
			contracts.FanoutCheckpoint{Bytes: total, Outputs: outputs})
		if errors.Is(err, workloadcheckpoint.ErrUnavailable) {
			return nil
		}
		return err
	}
	if err := checkpoint(0); err != nil {
		return domain.Result{Outputs: outputs}, err
	}
	queue := make(chan contracts.TransferPart)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var total int64
	failed := false
	var checkpointErr error
	for slot := 0; slot < min(len(transfer.Parts), contracts.FanoutDestinationConcurrency); slot++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for part := range queue {
				var etag string
				var delivery providerDelivery
				var writeErr error
				if part.Authenticated() {
					delivery, writeErr = h.uploadAuthenticated(ctx, &client, part, payload)
				} else {
					etag, writeErr = writeFanoutDestination(ctx, &client, part, payload.bytes)
				}
				mu.Lock()
				prefix := fmt.Sprintf("part.%d.", part.Index)
				if part.Authenticated() {
					outputs[prefix+"sha1"] = hex.EncodeToString(payload.sha1)
					outputs[prefix+"request_count"] = strconv.Itoa(delivery.requests)
				}
				if writeErr != nil {
					failed = true
					outputs[prefix+"error"] = writeErr.Error()
					outputs[prefix+"state"] = "failed"
					recordThrottle(outputs, prefix, writeErr)
				} else {
					total += part.Length
					outputs[prefix+"sha256"] = checksum
					if part.Authenticated() {
						outputs[prefix+"provider_part_id"] = delivery.providerPartID
					} else {
						outputs[prefix+"etag"] = etag
					}
					outputs[prefix+"bytes"], outputs[prefix+"state"] = strconv.FormatInt(part.Length, 10), "completed"
				}
				if checkpointErr == nil {
					if err := checkpoint(total); err != nil {
						checkpointErr = err
						stop()
					}
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
	if checkpointErr != nil {
		return result, checkpointErr
	}
	if failed {
		return result, errors.New("fanout_delivery_failed")
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
