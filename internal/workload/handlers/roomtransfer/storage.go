package roomtransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

// A redirect could forward an assignment token or a signed provider request to
// a different authority. Errors deliberately omit URLs, headers and bodies.
func storageHTTPClient() *http.Client {
	return &http.Client{Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("storage redirects are not permitted")
	}}
}

func httpsEndpoint(endpoint contracts.HTTPEndpoint, method string) error {
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || endpoint.Method != method {
		return errors.New("invalid storage HTTPS endpoint")
	}
	return nil
}

func resolveStorageRoute(ctx context.Context, client *http.Client, transfer contracts.RoomTransfer,
	workerID string, lease contracts.TunnelLease, index int64, contentMD5 string, now time.Time) (contracts.StorageRoute, error) {
	var route contracts.StorageRoute
	if lease.Storage == nil || !now.Before(lease.ExpiresAt) || index < transfer.ChunkStart || index > transfer.ChunkEnd {
		return route, errors.New("storage assignment is expired or outside its range")
	}
	if err := lease.Validate(lease.Role, lease.TargetMemberID, now); err != nil {
		return route, err
	}
	endpoint := lease.Endpoints[0]
	payload, _ := json.Marshal(contracts.StorageRouteRequest{SchemaVersion: contracts.RoomStorageSchemaVersion,
		LeaseID: lease.LeaseID, TransferID: transfer.TransferID, LaneID: transfer.LaneID,
		Attempt: transfer.Attempt, WorkerID: workerID, ChunkIndex: index, ContentMD5: contentMD5})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL, bytes.NewReader(payload))
	if err != nil {
		return route, errors.New("invalid storage route request")
	}
	for key, value := range endpoint.Headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return route, errors.New("storage route control unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return route, fmt.Errorf("storage route control rejected assignment: HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 128<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&route); err != nil {
		return route, errors.New("invalid storage route response")
	}
	method := http.MethodPut
	if lease.Role == contracts.TunnelLeaseRoleSourceRead {
		method = http.MethodGet
	}
	offset, length := storageRange(transfer, index)
	if route.ChunkIndex != index || route.Offset != offset || route.Length != length ||
		!route.ExpiresAt.After(now) || route.ExpiresAt.After(lease.ExpiresAt) || httpsEndpoint(route.Endpoint, method) != nil {
		return contracts.StorageRoute{}, errors.New("storage route does not match the assignment")
	}
	if method == http.MethodPut && (route.PartNumber != contracts.MultipartAttemptPartNumber(index, transfer.Attempt) || route.UploadID == "") {
		return contracts.StorageRoute{}, errors.New("storage route lacks its multipart identity")
	}
	if method == http.MethodPut {
		headers := http.Header{}
		for key, value := range route.Endpoint.Headers {
			headers.Set(key, value)
		}
		if contentMD5 == "" || headers.Get("Content-MD5") != contentMD5 {
			return contracts.StorageRoute{}, errors.New("storage route checksum binding mismatch")
		}
	}
	return route, nil
}

func storageRange(transfer contracts.RoomTransfer, index int64) (int64, int64) {
	offset := index * transfer.ChunkSizeBytes
	return offset, min(transfer.ChunkSizeBytes, transfer.FileSizeBytes-offset)
}

// Allocate exactly the admitted payload rather than growing and copying it.
func readExactPayload(reader io.Reader, length int64) ([]byte, error) {
	if length <= 0 {
		return nil, errors.New("invalid source payload length")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	var extra [1]byte
	if _, err := io.ReadFull(reader, extra[:]); err != io.EOF {
		return nil, errors.New("source payload exceeds admitted length")
	}
	return payload, nil
}

func readStorageChunk(ctx context.Context, client *http.Client, transfer contracts.RoomTransfer,
	workerID string, index int64, now func() time.Time) ([]byte, contracts.StorageRangeResult, error) {
	lease := transfer.SourceLease
	route, err := resolveStorageRoute(ctx, client, transfer, workerID, lease, index, "", now())
	if err != nil {
		return nil, contracts.StorageRangeResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, route.Endpoint.URL, nil)
	if err != nil {
		return nil, contracts.StorageRangeResult{}, errors.New("invalid storage source request")
	}
	for key, value := range route.Endpoint.Headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", route.Offset, route.Offset+route.Length-1))
	if lease.Storage.ETag != "" {
		condition := request.Header.Get("If-Match")
		if condition != "" && strings.Trim(condition, `"`) != strings.Trim(lease.Storage.ETag, `"`) {
			return nil, contracts.StorageRangeResult{}, errors.New("storage source condition does not match its frozen identity")
		}
		if condition == "" {
			request.Header.Set("If-Match", `"`+strings.Trim(lease.Storage.ETag, `"`)+`"`)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, contracts.StorageRangeResult{}, errors.New("storage source read failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPreconditionFailed {
		return nil, contracts.StorageRangeResult{}, errors.New("room_storage_source_mutated")
	}
	expectedRange := fmt.Sprintf("bytes %d-%d/%d", route.Offset, route.Offset+route.Length-1, transfer.FileSizeBytes)
	if response.StatusCode != http.StatusPartialContent || response.Header.Get("Content-Range") != expectedRange {
		return nil, contracts.StorageRangeResult{}, errors.New("storage source did not return the assigned range")
	}
	etag, version := response.Header.Get("ETag"), response.Header.Get("x-amz-version-id")
	if (lease.Storage.ETag != "" && strings.Trim(etag, `"`) != strings.Trim(lease.Storage.ETag, `"`)) ||
		(lease.Storage.VersionID != "" && version != lease.Storage.VersionID) {
		return nil, contracts.StorageRangeResult{}, errors.New("room_storage_source_mutated")
	}
	payload, err := readExactPayload(response.Body, route.Length)
	if err != nil || int64(len(payload)) != route.Length {
		return nil, contracts.StorageRangeResult{}, errors.New("storage source range length mismatch")
	}
	digest := sha256.Sum256(payload)
	return payload, contracts.StorageRangeResult{LeaseID: lease.LeaseID, MemberID: lease.Storage.MemberID,
		Role: lease.Role, ChunkIndex: index, Offset: route.Offset, Length: route.Length, RangeSHA256: hex.EncodeToString(digest[:]),
		ETag: etag, VersionID: version, CompletedAt: now().UTC()}, nil
}

func writeStorageChunk(ctx context.Context, client *http.Client, transfer contracts.RoomTransfer,
	workerID string, target contracts.RoomTransferDestination, index int64, payload []byte, rangeSHA256, contentMD5 string,
	now func() time.Time) (contracts.StorageRangeResult, error) {
	// Route refresh and HTTP retries retain this exact payload. Neither operation
	// can invoke the source reader or alter successful destination coverage.
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return contracts.StorageRangeResult{}, err
		}
		route, err := resolveStorageRoute(ctx, client, transfer, workerID, target.Lease, index, contentMD5, now())
		if err != nil {
			return contracts.StorageRangeResult{}, err
		}
		if int64(len(payload)) != route.Length {
			return contracts.StorageRangeResult{}, errors.New("storage destination range length mismatch")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, route.Endpoint.URL, bytes.NewReader(payload))
		if err != nil {
			return contracts.StorageRangeResult{}, errors.New("invalid storage destination request")
		}
		for key, value := range route.Endpoint.Headers {
			request.Header.Set(key, value)
		}
		if request.Header.Get("Content-Type") == "" {
			request.Header.Set("Content-Type", "application/octet-stream")
		}
		response, err := client.Do(request)
		if err != nil {
			last = errors.New("storage destination write failed")
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxReceiptBytes))
		_ = response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			etag := response.Header.Get("ETag")
			if etag == "" {
				return contracts.StorageRangeResult{}, errors.New("storage destination returned no part identity")
			}
			return contracts.StorageRangeResult{LeaseID: target.Lease.LeaseID, MemberID: target.MemberID,
				Role: target.Lease.Role, ChunkIndex: index, Offset: route.Offset, Length: route.Length,
				RangeSHA256: rangeSHA256, ETag: etag, UploadID: route.UploadID, PartNumber: route.PartNumber, CompletedAt: now().UTC()}, nil
		}
		last = fmt.Errorf("storage destination returned HTTP %d", response.StatusCode)
		if response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusRequestTimeout && response.StatusCode != http.StatusTooManyRequests && response.StatusCode < 500 {
			break
		}
	}
	return contracts.StorageRangeResult{}, last
}
