package beamcore

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
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
	spec, err := ToWorkload(boxPartOffer(), time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
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
