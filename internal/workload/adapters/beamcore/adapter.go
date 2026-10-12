package beamcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type TaskOfferBatch struct {
	BatchID             string      `json:"batch_id"`
	TransferID          string      `json:"transfer_id"`
	AssignmentTimeoutMS int64       `json:"assignment_timeout_ms"`
	Offers              []TaskOffer `json:"offers"`
}

type TaskOffer struct {
	OfferID      string             `json:"offer_id"`
	Source       OfferSource        `json:"source"`
	Destinations []OfferDestination `json:"destinations"`
}

type OfferSource struct {
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	ChunkSize int64             `json:"chunk_size"`
}

// OfferDestination is one TaskOfferDestination. Protocol defaults to presigned_put;
// the authenticated protocols declare their method, digest, receipt and attempt budget.
type OfferDestination struct {
	URL          string                   `json:"url"`
	Headers      map[string]string        `json:"headers,omitempty"`
	ETagRequired bool                     `json:"etag_required,omitempty"`
	Protocol     string                   `json:"protocol,omitempty"`
	Profile      string                   `json:"profile,omitempty"`
	Method       string                   `json:"method,omitempty"`
	Digest       *contracts.UploadDigest  `json:"digest,omitempty"`
	Receipt      *contracts.UploadReceipt `json:"receipt,omitempty"`
	MaxAttempts  int                      `json:"max_attempts,omitempty"`
	Form         *contracts.UploadForm    `json:"form,omitempty"`
}

func MultipartTransferResources() domain.Resources {
	return domain.Resources{MemoryBytes: 4 << 20, BandwidthMbps: 1, Connections: 2, Streams: 2}
}

// BufferedTransferResources reserves one buffered range plus transport overhead.
func BufferedTransferResources(chunkSize int64) domain.Resources {
	resources := MultipartTransferResources()
	resources.MemoryBytes = chunkSize + contracts.TransferBufferOverheadBytes
	return resources
}

// AuthenticatedEvidence names the outputs an authenticated destination commits to.
var AuthenticatedEvidence = []string{"sha1", "request_count", "provider_part_id"}

// ToWorkload maps one offer to one worker workload. Several destinations become a
// fan-out workload that reads the range once and uploads it to each destination.
func ToWorkload(offer TaskOffer, assignmentTimeout time.Duration, receivedAt time.Time) (domain.Spec, error) {
	if offer.OfferID == "" {
		return domain.Spec{}, errors.New("BeamCore offer_id is required")
	}
	if offer.Source.URL == "" || offer.Source.ChunkSize <= 0 || len(offer.Destinations) == 0 {
		return domain.Spec{}, errors.New("BeamCore offer requires a source range and at least one destination")
	}
	if assignmentTimeout <= 0 {
		return domain.Spec{}, errors.New("BeamCore offer batch requires assignment_timeout_ms")
	}
	fanout := len(offer.Destinations) > 1
	parts := make([]contracts.TransferPart, 0, len(offer.Destinations))
	etagRequired, authenticated := false, false
	for index, destination := range offer.Destinations {
		if destination.URL == "" {
			return domain.Spec{}, fmt.Errorf("BeamCore offer destination %d requires a url", index)
		}
		method, err := offerDestinationMethod(destination)
		if err != nil {
			return domain.Spec{}, fmt.Errorf("BeamCore offer destination %d: %w", index, err)
		}
		etagRequired = etagRequired || destination.ETagRequired
		part := contracts.TransferPart{
			Index: index, Length: offer.Source.ChunkSize, ETagRequired: destination.ETagRequired,
			Source: contracts.HTTPEndpoint{URL: offer.Source.URL, Headers: offer.Source.Headers},
			Destination: contracts.HTTPEndpoint{URL: destination.URL, Method: method,
				Headers: destination.Headers},
		}
		if destination.Protocol != "" && destination.Protocol != contracts.UploadProtocolPresignedPut {
			part.Protocol, part.Profile, part.MaxAttempts = destination.Protocol, destination.Profile, destination.MaxAttempts
			part.Digest, part.Receipt, part.Form = destination.Digest, destination.Receipt, destination.Form
			authenticated = true
		}
		parts = append(parts, part)
	}
	payload, err := json.Marshal(contracts.MultipartTransfer{Parts: parts, Fanout: fanout})
	if err != nil {
		return domain.Spec{}, err
	}
	commitments := []string{"sha256"}
	if etagRequired {
		commitments = append(commitments, "etag")
	}
	if authenticated {
		commitments = append(commitments, AuthenticatedEvidence...)
	}
	spec := domain.Spec{
		WorkloadID: offer.OfferID, AttemptID: offer.OfferID,
		Kind: domain.KindTransferMultipart, Class: domain.ClassJob,
		Source:               domain.Source{System: "beamcore", Reference: offer.OfferID},
		RequiredCapabilities: []string{contracts.TransferMultipartCapability},
		Resources:            MultipartTransferResources(),
		Lease:                domain.Lease{OfferExpiresAt: receivedAt.Add(assignmentTimeout)},
		Evidence:             domain.EvidencePolicy{ReceiptRequired: true, Commitments: commitments},
		Payload:              payload,
	}
	if fanout || authenticated {
		// The range is buffered once so every destination and retry reuses the same bytes.
		spec.Resources = BufferedTransferResources(offer.Source.ChunkSize)
	}
	if fanout {
		spec.RequiredCapabilities = append(spec.RequiredCapabilities, contracts.TransferMultipartFanoutCapability)
		spec.Resources.Connections = int64(1 + min(len(parts), contracts.FanoutDestinationConcurrency))
		spec.Resources.Streams = spec.Resources.Connections
	}
	if authenticated {
		spec.RequiredCapabilities = append(spec.RequiredCapabilities, contracts.TransferAuthenticatedPartCapability)
	}
	return spec, nil
}

// offerDestinationMethod derives a presigned destination's method from its URL and
// ignores any offered method; authenticated protocols always declare theirs.
func offerDestinationMethod(destination OfferDestination) (string, error) {
	switch destination.Protocol {
	case "", contracts.UploadProtocolPresignedPut:
		return destinationMethod(destination.URL), nil
	case contracts.UploadProtocolAuthenticatedPart, contracts.UploadProtocolAuthenticatedForm:
		method := strings.ToUpper(strings.TrimSpace(destination.Method))
		if method == "" {
			return "", fmt.Errorf("protocol %s requires a method", destination.Protocol)
		}
		return method, nil
	default:
		return "", fmt.Errorf("unsupported destination protocol %q", destination.Protocol)
	}
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
