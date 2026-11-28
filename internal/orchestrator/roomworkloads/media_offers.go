package roomworkloads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/roomworkload"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type MediaOfferSource interface {
	RequestMediaNodeOffer(ctx context.Context, request mediacontracts.MediaNodeOfferRequest) (json.RawMessage, error)
}

func (m *Manager) RegisterMediaOffers(source MediaOfferSource) {
	m.mediaNodes.mu.Lock()
	m.mediaNodes.offers = source
	m.mediaNodes.mu.Unlock()
}

func (n *mediaNodes) offerSource() MediaOfferSource {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.offers
}

func (s MediaStrategy) NeedsRefresh(definition genericDefinition[contracts.MediaUnitDetails]) bool {
	return contracts.IsRoomMediaNodeProfile(definition.Workload.Details.Profile)
}

func (s MediaStrategy) Refresh(ctx context.Context, definition genericDefinition[contracts.MediaUnitDetails], workerID,
	reason string) (contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails], error) {
	var none contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails]
	if s.media == nil {
		return none, fmt.Errorf("%w: media offers are not configured", roomworkload.ErrRefreshLater)
	}
	source := s.media.offerSource()
	if source == nil {
		return none, fmt.Errorf("%w: the Runtime's media control is not connected", roomworkload.ErrRefreshLater)
	}
	held := definition.Workload
	identity := held.Identity
	request := mediacontracts.MediaNodeOfferRequest{Type: mediacontracts.NodeOfferRequestType, SchemaVersion: contracts.RoomWorkloadSchema,
		Kind: string(domain.KindRoomMedia), WorkloadID: identity.WorkloadID, UnitID: identity.UnitID, Epoch: int64(identity.Epoch),
		Attempt: int64(identity.Attempt), Reason: reason, RequestedAt: s.media.now().UTC().Format(time.RFC3339Nano)}
	if workerID != "" {
		request.WorkerID = &workerID
	}
	if err := mediacontracts.ValidateMediaNodeOfferRequest(request); err != nil {
		return none, fmt.Errorf("media_node_offer_request for %s: %w", identity.WorkloadID, err)
	}
	encoded, err := source.RequestMediaNodeOffer(ctx, request)
	var refusal *MediaRefusal
	switch {
	case errors.As(err, &refusal) && refusal.RetryLater():
		return none, fmt.Errorf("%w: %v", roomworkload.ErrRefreshLater, err)
	case errors.As(err, &refusal):
		log.Printf("media node %s of %s attempt %d: fresh offer refused (%s): not delivered again", held.Details.NodeID,
			identity.WorkloadID, identity.Attempt, refusal.Reason)
		return none, &roomworkload.RefreshRefused{Reason: refusal.Reason, KeepRunning: refusal.Reason == "workload_retired"}
	case err != nil:
		return none, fmt.Errorf("%w: %v", roomworkload.ErrRefreshLater, err)
	}
	var ack mediacontracts.MediaAck
	if err := json.Unmarshal(encoded, &ack); err != nil {
		return none, fmt.Errorf("%w: invalid media_node_offer_request_ack: %v", roomworkload.ErrRefreshLater, err)
	}
	if err := mediacontracts.ValidateMediaNodeOfferAck(ack, request); err != nil || !ack.Accepted {
		return none, fmt.Errorf("%w: media_node_offer_request_ack does not answer the request", roomworkload.ErrRefreshLater)
	}
	fresh, err := s.media.decodeFreshOffer(ack.Offer, held)
	if err != nil {
		return none, fmt.Errorf("fresh offer of %s attempt %d: %w", identity.WorkloadID, identity.Attempt, err)
	}
	s.media.forgetShape(definition.ID)
	return fresh, nil
}

func (n *mediaNodes) decodeFreshOffer(encoded json.RawMessage,
	held contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails]) (contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails], error) {
	var none contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails]
	now := n.now()
	if err := contracts.ValidateRoomMediaMessage(contracts.RoomWorkloadOffer, encoded, now); err != nil {
		return none, err
	}
	var value contracts.RoomWorkloadOfferWire[contracts.MediaUnitDetails]
	if err := decodeCanonical(encoded, &value); err != nil {
		return none, err
	}
	if !contracts.IsRoomMediaNodeProfile(value.Details.Profile) {
		return none, errors.New("a fresh offer is a node workload offer")
	}
	if err := contracts.ValidateMediaNodeEnvelope(value.Details.Role, value.RequiredCapacity, len(value.DestinationSnapshot.Targets),
		&value.PathAuthorizations.Source); err != nil {
		return none, err
	}
	if err := value.ValidateProtocol(now, true, contracts.RoomMediaNodePathProtocol); err != nil {
		return none, err
	}
	fresh := fromOffer(value)
	identity, heldIdentity := fresh.Identity, held.Identity
	if identity.WorkloadID != heldIdentity.WorkloadID || identity.UnitID != heldIdentity.UnitID || identity.Epoch != heldIdentity.Epoch ||
		identity.Attempt != heldIdentity.Attempt || identity.RoomID != heldIdentity.RoomID ||
		fresh.Details.SessionID != held.Details.SessionID || fresh.Details.NodeID != held.Details.NodeID ||
		fresh.Details.Role != held.Details.Role || fresh.Source.PathID != held.Source.PathID {
		return none, errors.New("the fresh offer is not the held attempt's")
	}
	if !sameNodeRecording(fresh.Details.Recording, held.Details.Recording) {
		return none, errors.New("the fresh offer changes the attempt's recording")
	}
	return fresh, nil
}

func sameNodeRecording(fresh, held *mediacontracts.NodeRecording) bool {
	if fresh == nil || held == nil {
		return fresh == nil && held == nil
	}
	sameCount := func(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	return fresh.RecordingID == held.RecordingID && fresh.BucketPrefix == held.BucketPrefix &&
		sameCount(fresh.Generation, held.Generation)
}

func (n *mediaNodes) forgetShape(definitionID string) {
	life := n.current()
	life.shapesMu.Lock()
	defer life.shapesMu.Unlock()
	if shape := life.shapes[definitionID]; shape != nil {
		shape.computed = false
	}
}

var _ roomworkload.Refresher[contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails]] = MediaStrategy{}

const mediaResultHoldLimit = time.Minute

func (m *Manager) awaitMediaEvents(_ context.Context, value contracts.RoomGenericResult) error {
	if value.Identity.Kind != domain.KindRoomMedia || !mediaStateLostResult(value) {
		return nil
	}
	nodes, life := m.mediaNodes, m.mediaNodes.current()
	key := attemptKey(value.Identity.WorkloadID, value.Identity.UnitID, value.Identity.Epoch, value.Identity.Attempt)
	life.holdsMu.Lock()
	defer life.holdsMu.Unlock()
	if !nodes.upstream.pendingEvents(value.Identity.WorkloadID) {
		delete(life.resultHolds, key)
		return nil
	}
	if life.stopped() {
		return fmt.Errorf("room.media result of %s attempt %d waits for its session events: %w", value.Identity.WorkloadID,
			value.Identity.Attempt, ErrMediaStopped)
	}
	now := time.Now()
	first, held := life.resultHolds[key]
	if !held {
		life.resultHolds[key] = now
		first = now
	}
	if now.Sub(first) >= mediaResultHoldLimit {
		log.Printf("room.media result of %s attempt %d sent before its session events were acked (held %s)",
			value.Identity.WorkloadID, value.Identity.Attempt, mediaResultHoldLimit)
		delete(life.resultHolds, key)
		return nil
	}
	return fmt.Errorf("room.media result of %s attempt %d waits for its session events", value.Identity.WorkloadID,
		value.Identity.Attempt)
}

func mediaStateLostResult(value contracts.RoomGenericResult) bool {
	for _, failure := range value.Failures {
		if failure.Code == contracts.MediaPackagerStateLost || failure.Code == contracts.MediaRecordingStateLost {
			return true
		}
	}
	return false
}
