package beamcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type TaskOffer struct {
	SourceGroup              *SourceGroup      `json:"source_group,omitempty"`
	TaskID                   string            `json:"task_id"`
	OfferID                  string            `json:"offer_id"`
	DeadlineUS               int64             `json:"deadline_us"`
	AssignmentTimeoutMS      int64             `json:"assignment_timeout_ms,omitempty"`
	WorkerExecutionTimeoutMS int64             `json:"worker_execution_timeout_ms,omitempty"`
	TransferID               string            `json:"transfer_id,omitempty"`
	ChunkSize                int64             `json:"chunk_size,omitempty"`
	ChunkHash                string            `json:"chunk_hash,omitempty"`
	ChunkHashes              map[string]string `json:"chunk_hashes,omitempty"`
	SourceURL                string            `json:"source_url,omitempty"`
	DestinationURL           string            `json:"dest_url,omitempty"`
	SourceHeaders            map[string]string `json:"source_headers,omitempty"`
	DestinationHeaders       map[string]string `json:"dest_headers,omitempty"`
	URLsExpireAt             string            `json:"urls_expires_at,omitempty"`
	ETagRequired             bool              `json:"etag_required,omitempty"`
	ExecutionContext         ExecutionContext  `json:"execution_context"`
}

// A source group is indivisible at worker dispatch. Results retain each original
// task/offer identity so successful destinations survive unrelated failures.
type SourceGroup struct {
	ID          string `json:"id"`
	Index       int    `json:"index"`
	Count       int    `json:"count"`
	Concurrency int    `json:"concurrency"`
}

type ExecutionContext struct {
	TransferID        string            `json:"transfer_id"`
	GatewayURL        string            `json:"gateway_url,omitempty"`
	ObjectID          string            `json:"object_id,omitempty"`
	AuthToken         string            `json:"auth_token,omitempty"`
	ChunkIndices      []int             `json:"chunk_indices"`
	ChunkOffset       *int64            `json:"chunk_offset,omitempty"`
	ChunkSize         int64             `json:"chunk_size,omitempty"`
	TotalSize         int64             `json:"total_size,omitempty"`
	SourceURLs        map[string]string `json:"source_urls"`
	DestinationURLs   map[string]string `json:"dest_urls"`
	DestinationURL    string            `json:"destination_url,omitempty"`
	MultipartMetadata map[string]any    `json:"multipart_metadata,omitempty"`
}

func MultipartTransferResources() domain.Resources {
	return domain.Resources{MemoryBytes: 4 << 20, BandwidthMbps: 1, Connections: 2, Streams: 2}
}

func ToWorkload(offer TaskOffer, identity domain.Identity, receivedAt time.Time) (domain.Spec, error) {
	if offer.SourceGroup != nil {
		return domain.Spec{}, errors.New("source groups require atomic grouped dispatch")
	}
	if offer.TaskID == "" {
		return domain.Spec{}, errors.New("BeamCore task_id is required")
	}
	if offer.OfferID == "" {
		return domain.Spec{}, errors.New("BeamCore offer_id is required")
	}
	if offer.SourceURL != "" || offer.DestinationURL != "" {
		return directSignedURLWorkload(offer, identity, receivedAt)
	}
	context := offer.ExecutionContext
	indices := context.ChunkIndices
	if len(indices) == 0 {
		indices = []int{0}
	}
	chunkSize := context.ChunkSize
	if chunkSize <= 0 {
		chunkSize = offer.ChunkSize
	}
	parts := make([]contracts.TransferPart, 0, len(indices))
	for _, index := range indices {
		key := strconv.Itoa(index)
		sourceURL := context.SourceURLs[key]
		sourceRange := sourceURL != ""
		if sourceURL == "" && context.ObjectID != "" {
			sourceURL = joinGatewayURL(context.GatewayURL, "objects", context.ObjectID)
			sourceRange = true
		}
		if sourceURL == "" {
			sourceURL = joinGatewayURL(context.GatewayURL, "chunks", context.TransferID, key)
		}
		if sourceURL == "" {
			return domain.Spec{}, fmt.Errorf("BeamCore source URL or gateway_url for chunk %d is required", index)
		}
		destinationURL := context.DestinationURLs[key]
		if destinationURL == "" {
			destinationURL = context.DestinationURL
		}
		if destinationURL == "" {
			return domain.Spec{}, fmt.Errorf("BeamCore destination URL for chunk %d is required", index)
		}
		offset := int64(index) * chunkSize
		if context.ChunkOffset != nil {
			offset = *context.ChunkOffset
		}
		length := chunkSize
		if context.TotalSize > 0 && length > context.TotalSize-offset {
			length = context.TotalSize - offset
		}
		if length < 0 {
			return domain.Spec{}, fmt.Errorf("invalid chunk %d offset", index)
		}
		expectedHash := offer.ChunkHashes[key]
		if expectedHash == "" && len(indices) == 1 {
			expectedHash = offer.ChunkHash
		}
		destinationMethod := destinationMethod(destinationURL)
		destinationHeaders := make(map[string]string)
		if destinationMethod == http.MethodPost {
			destinationHeaders["X-Transfer-ID"] = context.TransferID
			destinationHeaders["X-Chunk-ID"] = "chunk_" + key
			destinationHeaders["X-Offset"] = strconv.FormatInt(offset, 10)
			destinationHeaders["X-Length"] = strconv.FormatInt(length, 10)
			destinationHeaders["X-Total-Size"] = strconv.FormatInt(context.TotalSize, 10)
			if context.AuthToken != "" {
				destinationHeaders["Authorization"] = "Bearer " + context.AuthToken
			}
		}
		parts = append(parts, contracts.TransferPart{
			Index: index, Offset: offset, Length: length, SourceRange: sourceRange, ExpectedSHA256: expectedHash,
			Source:      contracts.HTTPEndpoint{URL: sourceURL},
			Destination: contracts.HTTPEndpoint{URL: destinationURL, Method: destinationMethod, Headers: destinationHeaders},
		})
	}
	payload, err := json.Marshal(contracts.MultipartTransfer{TransferID: context.TransferID, Parts: parts})
	if err != nil {
		return domain.Spec{}, err
	}
	offerExpiration := time.UnixMicro(offer.DeadlineUS)
	if offer.DeadlineUS <= 0 {
		offerExpiration = receivedAt.Add(30 * time.Second)
	}
	return domain.Spec{
		WorkloadID: offer.TaskID, AttemptID: offer.OfferID, Identity: identity,
		Kind: domain.KindTransferMultipart, Class: domain.ClassJob,
		Source:               domain.Source{System: "beamcore", Reference: offer.TaskID},
		RequiredCapabilities: []string{"transfer.multipart"},
		Resources:            MultipartTransferResources(),
		Lease:                domain.Lease{OfferExpiresAt: offerExpiration},
		Evidence:             domain.EvidencePolicy{ReceiptRequired: true, Commitments: []string{"sha256", "etag"}},
		Payload:              payload,
	}, nil
}

func directSignedURLWorkload(offer TaskOffer, identity domain.Identity, receivedAt time.Time) (domain.Spec, error) {
	if offer.SourceURL == "" || offer.DestinationURL == "" || offer.TransferID == "" || offer.ChunkSize <= 0 {
		return domain.Spec{}, errors.New("BeamCore signed URL offer requires transfer_id, chunk_size, source_url, and dest_url")
	}
	expiresAt := receivedAt.Add(30 * time.Second)
	if offer.AssignmentTimeoutMS > 0 {
		expiresAt = receivedAt.Add(time.Duration(offer.AssignmentTimeoutMS) * time.Millisecond)
	}
	if offer.URLsExpireAt != "" {
		urlsExpireAt, err := time.Parse(time.RFC3339Nano, offer.URLsExpireAt)
		if err != nil {
			return domain.Spec{}, fmt.Errorf("invalid BeamCore signed URL expiry: %w", err)
		}
		if urlsExpireAt.Before(expiresAt) {
			expiresAt = urlsExpireAt
		}
	}
	payload, err := json.Marshal(contracts.MultipartTransfer{TransferID: offer.TransferID, Parts: []contracts.TransferPart{{
		Index: 0, Length: offer.ChunkSize, ExpectedSHA256: offer.ChunkHash,
		Source: contracts.HTTPEndpoint{URL: offer.SourceURL, Headers: offer.SourceHeaders},
		Destination: contracts.HTTPEndpoint{URL: offer.DestinationURL, Method: destinationMethod(offer.DestinationURL),
			Headers: offer.DestinationHeaders},
	}}})
	if err != nil {
		return domain.Spec{}, err
	}
	commitments := []string{"sha256"}
	if offer.ETagRequired {
		commitments = append(commitments, "etag")
	}
	return domain.Spec{
		WorkloadID: offer.TaskID, AttemptID: offer.OfferID, Identity: identity,
		Kind: domain.KindTransferMultipart, Class: domain.ClassJob,
		Source:               domain.Source{System: "beamcore", Reference: offer.TaskID},
		RequiredCapabilities: []string{"transfer.multipart"},
		Resources:            MultipartTransferResources(),
		Lease:                domain.Lease{OfferExpiresAt: expiresAt},
		Evidence:             domain.EvidencePolicy{ReceiptRequired: true, Commitments: commitments},
		Payload:              payload,
	}, nil
}

func joinGatewayURL(base string, segments ...string) string {
	if strings.TrimSpace(base) == "" {
		return ""
	}
	joined := strings.TrimRight(base, "/")
	for _, segment := range segments {
		joined += "/" + url.PathEscape(segment)
	}
	return joined
}

func destinationMethod(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return http.MethodPut
	}
	query := parsed.Query()
	if query.Has("X-Amz-Signature") || query.Has("X-Goog-Signature") ||
		strings.Contains(parsed.Host, "cloudflarestorage.com") || strings.Contains(parsed.Host, "storage.googleapis.com") {
		return http.MethodPut
	}
	return http.MethodPost
}
