package beamcore

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

func singleDestinationOffer() TaskOffer {
	return TaskOffer{
		OfferID: "offer-1",
		Source: OfferSource{URL: "https://source.test/chunk", ChunkSize: 8,
			Headers: map[string]string{"Range": "bytes=8-15", "x-source": "one"}},
		Destinations: []OfferDestination{{URL: "https://bucket.test/chunk?X-Amz-Signature=sig",
			Headers: map[string]string{"x-dest": "two"}, ETagRequired: true}},
	}
}

func boxPartOffer() TaskOffer {
	return TaskOffer{
		OfferID: "offer-box",
		Source: OfferSource{URL: "https://source.test/object", ChunkSize: 8 << 20,
			Headers: map[string]string{"Range": "bytes=8388608-16777215"}},
		Destinations: []OfferDestination{{
			URL:      "https://upload.box.com/api/2.0/files/upload_sessions/session-1",
			Headers:  map[string]string{"Authorization": "Bearer worker-token", "Content-Range": "bytes 8388608-16777215/20971520"},
			Protocol: contracts.UploadProtocolAuthenticatedPart, Profile: contracts.UploadProfileBox, Method: "put",
			Digest:      &contracts.UploadDigest{Algorithm: "sha1", Header: "Digest", Format: "sha=base64"},
			Receipt:     &contracts.UploadReceipt{Path: "part", Fields: []string{"part_id", "offset", "size", "sha1"}},
			MaxAttempts: 3,
		}},
	}
}

func TestToWorkloadMapsAuthenticatedBoxPart(t *testing.T) {
	now := time.Now()
	spec, err := ToWorkload(boxPartOffer(), time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Lease.OfferExpiresAt.Equal(now.Add(time.Minute)) || !spec.Lease.AssignmentExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("lease does not end at the Core deadline: %+v", spec.Lease)
	}
	var payload contracts.MultipartTransfer
	if err := json.Unmarshal(spec.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	part := payload.Parts[0]
	if part.Destination.Method != "PUT" || part.Protocol != contracts.UploadProtocolAuthenticatedPart ||
		part.Profile != "box" || part.MaxAttempts != 3 || part.Digest == nil || part.Digest.Format != "sha=base64" ||
		part.Receipt == nil || part.Receipt.Path != "part" || len(part.Receipt.Fields) != 4 || part.Form != nil ||
		part.Destination.Headers["Content-Range"] != "bytes 8388608-16777215/20971520" || !part.Authenticated() {
		t.Fatalf("authenticated destination was not mapped: %+v", part)
	}
	if len(spec.RequiredCapabilities) != 2 || spec.RequiredCapabilities[1] != contracts.TransferAuthenticatedPartCapability {
		t.Fatalf("authenticated part must require its capability: %+v", spec.RequiredCapabilities)
	}
	if spec.Resources.MemoryBytes != (8<<20)+contracts.TransferBufferOverheadBytes {
		t.Fatalf("authenticated part must reserve its buffer: %+v", spec.Resources)
	}
	if strings.Join(spec.Evidence.Commitments, ",") != "sha256,sha1,request_count,provider_part_id" {
		t.Fatalf("authenticated evidence is not committed: %+v", spec.Evidence.Commitments)
	}
}

func TestToWorkloadMapsAuthenticatedBoxForm(t *testing.T) {
	offer := boxPartOffer()
	offer.Destinations[0] = OfferDestination{URL: "https://upload.box.com/api/2.0/files/42/content",
		Headers:  map[string]string{"Authorization": "Bearer worker-token"},
		Protocol: contracts.UploadProtocolAuthenticatedForm, Profile: contracts.UploadProfileBox, Method: "POST",
		Digest:      &contracts.UploadDigest{Algorithm: "sha1", Header: "Content-MD5", Format: "hex"},
		Receipt:     &contracts.UploadReceipt{Path: "entries.0", Fields: []string{"id", "sha1", "size"}},
		MaxAttempts: 3, Form: &contracts.UploadForm{Attributes: `{"name":"a.bin"}`, FileField: "file", Filename: "a.bin"}}
	spec, err := ToWorkload(offer, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var payload contracts.MultipartTransfer
	if err := json.Unmarshal(spec.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	part := payload.Parts[0]
	if part.Destination.Method != "POST" || part.Protocol != contracts.UploadProtocolAuthenticatedForm ||
		part.Form == nil || part.Form.FileField != "file" || part.Form.Filename != "a.bin" || part.Digest.Header != "Content-MD5" {
		t.Fatalf("authenticated form was not mapped: %+v", part)
	}
}

func TestToWorkloadKeepsPresignedMethodsAndRejectsUnknownProtocols(t *testing.T) {
	offer := singleDestinationOffer()
	offer.Destinations[0].URL = "https://upload.example/part"
	offer.Destinations[0].Protocol = contracts.UploadProtocolPresignedPut
	// A presigned destination's method comes from its URL; an offered one is ignored.
	offer.Destinations[0].Method = http.MethodPut
	spec, err := ToWorkload(offer, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var payload contracts.MultipartTransfer
	if err := json.Unmarshal(spec.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if part := payload.Parts[0]; part.Destination.Method != "POST" || part.Protocol != "" || part.Authenticated() ||
		len(spec.RequiredCapabilities) != 1 || spec.Resources != MultipartTransferResources() {
		t.Fatalf("presigned destination changed: %+v %+v", part, spec)
	}
	for name, mutate := range map[string]func(*OfferDestination){
		"unknown protocol":         func(destination *OfferDestination) { destination.Protocol = "signed_post_v9" },
		"authenticated, no method": func(destination *OfferDestination) { destination.Method = "" },
	} {
		offer := boxPartOffer()
		mutate(&offer.Destinations[0])
		if _, err := ToWorkload(offer, time.Minute, time.Now()); err == nil {
			t.Fatalf("offer with %s was accepted", name)
		}
	}
}

func TestToWorkloadRequiresAuthenticatedCapabilityForMixedFanout(t *testing.T) {
	offer := boxPartOffer()
	offer.Destinations = append(offer.Destinations, OfferDestination{URL: "https://bucket.test/part?X-Amz-Signature=sig"})
	spec, err := ToWorkload(offer, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var payload contracts.MultipartTransfer
	if err := json.Unmarshal(spec.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Fanout || !payload.Parts[0].Authenticated() || payload.Parts[1].Authenticated() ||
		payload.Parts[1].Destination.Method != "PUT" || len(spec.RequiredCapabilities) != 3 ||
		spec.RequiredCapabilities[2] != contracts.TransferAuthenticatedPartCapability ||
		spec.Resources.MemoryBytes != (8<<20)+contracts.TransferBufferOverheadBytes || spec.Resources.Connections != 3 {
		t.Fatalf("mixed fan-out has unexpected requirements: %+v %+v", spec.RequiredCapabilities, spec.Resources)
	}
}

func sourceDigestOffer() TaskOffer {
	return TaskOffer{
		OfferID: "offer-digest", Kind: OfferKindSourceDigest,
		Source: OfferSource{URL: "https://bucket.test/object?X-Amz-Signature=sig", Size: 1610612736,
			Headers: map[string]string{"x-amz-security-token": "source-token"}},
		Digest: &OfferDigest{Algorithms: []string{"sha1", "sha256"}},
	}
}

func TestToWorkloadMapsSourceDigestOffer(t *testing.T) {
	now := time.Now()
	spec, err := ToWorkload(sourceDigestOffer(), 40*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Kind != domain.KindTransferSourceDigest || spec.WorkloadID != "offer-digest" || spec.AttemptID != "offer-digest" ||
		!spec.Lease.OfferExpiresAt.Equal(now.Add(40*time.Minute)) || !spec.Lease.AssignmentExpiresAt.Equal(now.Add(40*time.Minute)) {
		t.Fatalf("unexpected digest workload: %+v", spec)
	}
	if len(spec.RequiredCapabilities) != 1 || spec.RequiredCapabilities[0] != contracts.TransferSourceDigestCapability {
		t.Fatalf("digest workload must require only its own capability: %+v", spec.RequiredCapabilities)
	}
	// A streaming digest reserves the same small footprint whatever the object size.
	if spec.Resources != SourceDigestResources() || spec.Resources.MemoryBytes != contracts.TransferBufferOverheadBytes {
		t.Fatalf("digest workload must not reserve the object: %+v", spec.Resources)
	}
	if strings.Join(spec.Evidence.Commitments, ",") != "sha1,sha256,bytes" || !spec.Evidence.ReceiptRequired {
		t.Fatalf("digest evidence is not committed: %+v", spec.Evidence)
	}
	var payload contracts.SourceDigest
	if err := json.Unmarshal(spec.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Size != 1610612736 || payload.Source.URL != "https://bucket.test/object?X-Amz-Signature=sig" ||
		payload.Source.Headers["x-amz-security-token"] != "source-token" {
		t.Fatalf("digest payload was not mapped: %+v", payload)
	}
	if strings.Contains(string(spec.Payload), "destination") || strings.Contains(string(spec.Payload), "offer_id") {
		t.Fatalf("digest payload carries more than its source: %s", spec.Payload)
	}
	offer := sourceDigestOffer()
	offer.Digest.Algorithms = []string{"sha256", "sha1"}
	offer.Source.URL = "http://127.0.0.1:8080/object"
	if _, err := ToWorkload(offer, time.Minute, now); err != nil {
		t.Fatalf("reordered algorithms or a loopback test source were rejected: %v", err)
	}
}

func TestToWorkloadRejectsInvalidSourceDigestOffers(t *testing.T) {
	for name, mutate := range map[string]func(*TaskOffer){
		"destinations": func(offer *TaskOffer) {
			offer.Destinations = []OfferDestination{{URL: "https://bucket.test/part?X-Amz-Signature=sig"}}
		},
		"no digest":           func(offer *TaskOffer) { offer.Digest = nil },
		"sha1 only":           func(offer *TaskOffer) { offer.Digest.Algorithms = []string{"sha1"} },
		"duplicate algorithm": func(offer *TaskOffer) { offer.Digest.Algorithms = []string{"sha1", "sha1"} },
		"extra algorithm":     func(offer *TaskOffer) { offer.Digest.Algorithms = []string{"sha1", "sha256", "md5"} },
		"other algorithm":     func(offer *TaskOffer) { offer.Digest.Algorithms = []string{"sha1", "md5"} },
		"no size":             func(offer *TaskOffer) { offer.Source.Size = 0 },
		"no source url":       func(offer *TaskOffer) { offer.Source.URL = "" },
		"plain http source":   func(offer *TaskOffer) { offer.Source.URL = "http://bucket.test/object" },
		"range header":        func(offer *TaskOffer) { offer.Source.Headers["Range"] = "bytes=0-1023" },
	} {
		offer := sourceDigestOffer()
		mutate(&offer)
		if _, err := ToWorkload(offer, time.Minute, time.Now()); err == nil {
			t.Errorf("digest offer with %s was accepted", name)
		}
	}
	offer := sourceDigestOffer()
	offer.Kind = "source_hash"
	if _, err := ToWorkload(offer, time.Minute, time.Now()); err == nil || !strings.Contains(err.Error(), `unsupported BeamCore offer kind "source_hash"`) {
		t.Fatalf("unknown offer kind was not rejected clearly: %v", err)
	}
}
