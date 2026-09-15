package beamcore

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

// GroupWorkloads composes existing signed multipart offers into one worker
// workload per immutable source range. Incomplete groups are never dispatched.
func GroupWorkloads(offers []TaskOffer, receivedAt time.Time) ([]domain.Spec, error) {
	groups := map[string][]TaskOffer{}
	order := []string{}
	for _, offer := range offers {
		key := "task:" + offer.OfferID
		if offer.SourceGroup != nil {
			key = "group:" + offer.SourceGroup.ID
			if offer.SourceGroup.ID == "" {
				return nil, errors.New("source group identity is required")
			}
		}
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], offer)
	}
	specs := make([]domain.Spec, 0, len(order))
	for _, key := range order {
		group := groups[key]
		if group[0].SourceGroup == nil {
			if len(group) != 1 {
				return nil, errors.New("duplicate task offer identity")
			}
			spec, err := ToWorkload(group[0], domain.Identity{}, receivedAt)
			if err != nil {
				return nil, err
			}
			specs = append(specs, spec)
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].SourceGroup.Index < group[j].SourceGroup.Index })
		first := group[0]
		if len(group) != first.SourceGroup.Count || first.SourceGroup.Concurrency < 1 || first.SourceGroup.Concurrency > 8 {
			return nil, errors.New("source group is incomplete or has invalid concurrency")
		}
		payload := contracts.MultipartTransfer{TransferID: first.TransferID, SourceGroupID: first.SourceGroup.ID, DestinationConcurrency: first.SourceGroup.Concurrency}
		var spec domain.Spec
		seenTasks, seenOffers := map[string]bool{}, map[string]bool{}
		for index, offer := range group {
			if offer.SourceGroup == nil || offer.SourceGroup.Index != index || offer.SourceGroup.Count != len(group) ||
				offer.SourceGroup.Concurrency != first.SourceGroup.Concurrency || offer.TransferID != first.TransferID ||
				offer.ChunkSize != first.ChunkSize || offer.SourceURL != first.SourceURL || !reflect.DeepEqual(offer.SourceHeaders, first.SourceHeaders) ||
				seenTasks[offer.TaskID] || seenOffers[offer.OfferID] {
				return nil, errors.New("source group offers disagree")
			}
			seenTasks[offer.TaskID], seenOffers[offer.OfferID] = true, true
			child, err := directSignedURLWorkload(offer, domain.Identity{}, receivedAt)
			if err != nil {
				return nil, err
			}
			var transfer contracts.MultipartTransfer
			if err := json.Unmarshal(child.Payload, &transfer); err != nil {
				return nil, err
			}
			part := transfer.Parts[0]
			part.Index, part.TaskID, part.OfferID, part.ETagRequired = index, offer.TaskID, offer.OfferID, offer.ETagRequired
			payload.Parts = append(payload.Parts, part)
			if index == 0 {
				spec = child
			} else if child.Lease.OfferExpiresAt.Before(spec.Lease.OfferExpiresAt) {
				spec.Lease.OfferExpiresAt = child.Lease.OfferExpiresAt
			}
		}
		spec.RequiredCapabilities = []string{contracts.TransferMultipartCapability, contracts.TransferMultipartFanoutCapability}
		spec.Resources.MemoryBytes = first.ChunkSize + (4 << 20)
		spec.Resources.Connections = int64(1 + first.SourceGroup.Concurrency)
		spec.Resources.Streams = spec.Resources.Connections
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		spec.Payload = encoded
		specs = append(specs, spec)
	}
	return specs, nil
}
