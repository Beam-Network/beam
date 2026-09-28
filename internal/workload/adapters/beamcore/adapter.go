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

type OfferDestination struct {
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers,omitempty"`
	ETagRequired bool              `json:"etag_required,omitempty"`
}

func MultipartTransferResources() domain.Resources {
	return domain.Resources{MemoryBytes: 4 << 20, BandwidthMbps: 1, Connections: 2, Streams: 2}
}

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
	etagRequired := false
	for index, destination := range offer.Destinations {
		if destination.URL == "" {
			return domain.Spec{}, fmt.Errorf("BeamCore offer destination %d requires a url", index)
		}
		etagRequired = etagRequired || destination.ETagRequired
		parts = append(parts, contracts.TransferPart{
			Index: index, Length: offer.Source.ChunkSize, ETagRequired: destination.ETagRequired,
			Source: contracts.HTTPEndpoint{URL: offer.Source.URL, Headers: offer.Source.Headers},
			Destination: contracts.HTTPEndpoint{URL: destination.URL, Method: destinationMethod(destination.URL),
				Headers: destination.Headers},
		})
	}
	payload, err := json.Marshal(contracts.MultipartTransfer{Parts: parts, Fanout: fanout})
	if err != nil {
		return domain.Spec{}, err
	}
	commitments := []string{"sha256"}
	if etagRequired {
		commitments = append(commitments, "etag")
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
	if fanout {
		spec.RequiredCapabilities = append(spec.RequiredCapabilities, contracts.TransferMultipartFanoutCapability)
		spec.Resources.MemoryBytes = offer.Source.ChunkSize + (4 << 20)
		spec.Resources.Connections = int64(1 + min(len(parts), contracts.FanoutDestinationConcurrency))
		spec.Resources.Streams = spec.Resources.Connections
	}
	return spec, nil
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
