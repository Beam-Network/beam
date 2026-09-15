package beamcore

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func TestSourceGroupDispatchIsAtomic(t *testing.T) {
	offers := []TaskOffer{
		{TaskID: "t0", OfferID: "a0", TransferID: "transfer", ChunkSize: 8, SourceURL: "https://source.invalid/range", DestinationURL: "https://destination.invalid/one", SourceGroup: &SourceGroup{ID: "range", Index: 0, Count: 2, Concurrency: 2}},
		{TaskID: "t1", OfferID: "a1", TransferID: "transfer", ChunkSize: 8, SourceURL: "https://source.invalid/range", DestinationURL: "https://destination.invalid/two", SourceGroup: &SourceGroup{ID: "range", Index: 1, Count: 2, Concurrency: 2}},
	}
	specs, err := GroupWorkloads(offers, time.Now())
	if err != nil || len(specs) != 1 {
		t.Fatalf("group dispatch=%v %v", specs, err)
	}
	var payload contracts.MultipartTransfer
	if err := json.Unmarshal(specs[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Parts) != 2 || payload.Parts[1].OfferID != "a1" || specs[0].Resources.MemoryBytes != 8+(4<<20) {
		t.Fatalf("payload=%+v spec=%+v", payload, specs[0])
	}
	if _, err := GroupWorkloads(offers[:1], time.Now()); err == nil {
		t.Fatal("incomplete group dispatched")
	}
	if _, err := ToWorkload(offers[0], domain.Identity{}, time.Now()); err == nil {
		t.Fatal("source group dispatched as scalar task")
	}
	offers[1].SourceURL = "https://different.invalid/range"
	if _, err := GroupWorkloads(offers, time.Now()); err == nil {
		t.Fatal("mismatched sources dispatched")
	}
}
