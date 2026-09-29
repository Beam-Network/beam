package roommedia

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/handlers/workertls"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

func TestWorkerMediaRejectsPlaintextDowngrade(t *testing.T) {
	spec := mediaSpec(t, "work", "session")
	var worker contracts.RoomWorkerSpec[contracts.MediaUnitDetails]
	if err := json.Unmarshal(spec.Payload, &worker); err != nil {
		t.Fatal(err)
	}
	worker.Details.Protection = contracts.MediaProtection{}
	spec.Payload, _ = json.Marshal(worker)
	if err := NewHandler(testMediaConfig()).Validate(spec); err == nil {
		t.Fatal("Worker accepted an unprotected WebRTC workload")
	}
}

func TestWorkerMediaRequiresPinnedHTTPSAdvertiseURL(t *testing.T) {
	spec := mediaSpec(t, "work", "session")
	for _, advertiseURL := range []string{"", "http://127.0.0.1:{port}", "https://user@127.0.0.1:{port}",
		"https://127.0.0.1:{port}?token=value", "https://127.0.0.1:{port}#fragment", "/v1/media"} {
		handler := NewHandler(Config{ListenAddress: "127.0.0.1:0", AdvertiseURL: advertiseURL})
		if err := handler.Validate(spec); err == nil {
			t.Fatalf("Worker accepted media advertise URL %q", advertiseURL)
		}
		if err := handler.PrepareListener(); err == nil {
			_ = handler.Close()
			t.Fatalf("Worker bound media listener for advertise URL %q", advertiseURL)
		}
	}
	handler := NewHandler(testMediaConfig())
	t.Cleanup(func() { _ = handler.Close() })
	if err := handler.PrepareListener(); err != nil {
		t.Fatal(err)
	}
	if err := handler.Validate(spec); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerMediaAdmissionContract(t *testing.T) {
	cases := map[string]func(*contracts.RoomWorkerSpec[contracts.MediaUnitDetails]){
		"lifetime over 23 hours": func(worker *contracts.RoomWorkerSpec[contracts.MediaUnitDetails]) {
			worker.Identity.ExpiresAt = time.Now().Add(24 * time.Hour).UTC()
		},
		"data track": func(worker *contracts.RoomWorkerSpec[contracts.MediaUnitDetails]) {
			worker.Details.Tracks = append(worker.Details.Tracks, contracts.MediaTrack{TrackID: "data-main", Kind: "data"})
		},
		"duplicate track id": func(worker *contracts.RoomWorkerSpec[contracts.MediaUnitDetails]) {
			worker.Details.Tracks = append(worker.Details.Tracks, contracts.MediaTrack{TrackID: "video-main", Kind: "audio"})
		},
		"source as target": func(worker *contracts.RoomWorkerSpec[contracts.MediaUnitDetails]) {
			worker.Targets = []contracts.RoomWorkerTarget{{MemberID: worker.Identity.SourceMemberID}}
		},
		"replay": func(worker *contracts.RoomWorkerSpec[contracts.MediaUnitDetails]) { worker.Details.Replay = true },
	}
	handler := NewHandler(testMediaConfig())
	if err := handler.Validate(mediaSpec(t, "work", "session")); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := mediaSpec(t, "work", "session")
			var worker contracts.RoomWorkerSpec[contracts.MediaUnitDetails]
			if err := json.Unmarshal(spec.Payload, &worker); err != nil {
				t.Fatal(err)
			}
			mutate(&worker)
			spec.Payload, _ = json.Marshal(worker)
			if err := handler.Validate(spec); err == nil {
				t.Fatal("Worker admitted media outside the admission contract")
			}
		})
	}
}

func TestWorkerMediaRuntimePublishesProtectedSFUEndpoint(t *testing.T) {
	spec := mediaSpec(t, "work", "session")
	progress := make(chan map[string]string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	ctx = workloadprogress.WithReporter(ctx, func(value map[string]string) { progress <- value })
	done := make(chan error, 1)
	handler := NewHandler(testMediaConfig())
	t.Cleanup(func() { _ = handler.Close() })
	go func() {
		_, executeErr := handler.Execute(ctx, spec)
		done <- executeErr
	}()

	var details contracts.MediaProgressDetails
	select {
	case value := <-progress:
		if err := json.Unmarshal([]byte(value["room_progress_details"]), &details); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker media runtime did not report readiness")
	}
	if details.Runtime == nil || details.Runtime.Transport != "worker_sfu" ||
		!strings.HasPrefix(details.Runtime.BaseURL, "https://127.0.0.1:") || len(details.Runtime.TLSCertificateSHA256) != 64 {
		t.Fatalf("runtime = %#v", details.Runtime)
	}
	request, _ := http.NewRequest(http.MethodGet, details.Runtime.BaseURL, nil)
	if response, err := http.DefaultClient.Do(request); err == nil {
		response.Body.Close()
		t.Fatal("unpinned client trusted the worker media certificate")
	}
	client := pinnedMediaClient(t, details.Runtime.TLSCertificateSHA256)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodGet, details.Runtime.BaseURL, nil)
	request.Header.Set("Authorization", "Bearer "+details.Runtime.AccessToken)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authorized status = %d", response.StatusCode)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker media runtime did not stop")
	}
}

func TestWorkerMediaRuntimeMultiplexesConcurrentSessions(t *testing.T) {
	handler := NewHandler(testMediaConfig())
	t.Cleanup(func() { _ = handler.Close() })
	contexts := make([]context.CancelFunc, 0, 2)
	done := make(chan error, 2)
	runtimes := make([]*contracts.MediaRuntime, 0, 2)
	for index, sessionID := range []string{"session-a", "session-b"} {
		progress := make(chan map[string]string, 1)
		ctx, cancel := context.WithCancel(context.Background())
		contexts = append(contexts, cancel)
		ctx = workloadprogress.WithReporter(ctx, func(value map[string]string) { progress <- value })
		spec := mediaSpec(t, "work-"+sessionID, sessionID)
		go func() {
			_, executeErr := handler.Execute(ctx, spec)
			done <- executeErr
		}()
		select {
		case value := <-progress:
			var details contracts.MediaProgressDetails
			if err := json.Unmarshal([]byte(value["room_progress_details"]), &details); err != nil {
				t.Fatal(err)
			}
			if details.Runtime == nil {
				t.Fatalf("runtime %d is nil", index)
			}
			runtimes = append(runtimes, details.Runtime)
		case <-time.After(5 * time.Second):
			t.Fatalf("media session %d did not report readiness", index)
		}
	}
	if runtimes[0].BaseURL == runtimes[1].BaseURL {
		t.Fatal("concurrent media sessions share a route")
	}
	for _, runtime := range runtimes {
		request, _ := http.NewRequest(http.MethodGet, runtime.BaseURL, nil)
		request.Header.Set("Authorization", "Bearer "+runtime.AccessToken)
		response, err := pinnedMediaClient(t, runtime.TLSCertificateSHA256).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("authorized status = %d", response.StatusCode)
		}
	}
	for _, cancel := range contexts {
		cancel()
	}
	for range contexts {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent media runtime did not stop")
		}
	}
}

func testMediaConfig() Config {
	return Config{ListenAddress: "127.0.0.1:0", AdvertiseURL: "https://127.0.0.1:{port}"}
}

// pinnedMediaClient connects like a room agent: the pin-derived SNI selects
// the worker certificate and the leaf must match the runtime fingerprint.
func pinnedMediaClient(t *testing.T, fingerprint string) *http.Client {
	t.Helper()
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13,
		ServerName: workertls.ServerName(fingerprint), InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			digest := sha256.Sum256(state.PeerCertificates[0].Raw)
			if hex.EncodeToString(digest[:]) != fingerprint {
				return errors.New("certificate pin mismatch")
			}
			return nil
		}}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func mediaSpec(t *testing.T, workloadID, sessionID string) domain.Spec {
	t.Helper()
	expiresAt := time.Now().Add(time.Minute).UTC()
	payload, err := json.Marshal(contracts.RoomWorkerSpec[contracts.MediaUnitDetails]{
		Schema: contracts.RoomWorkloadSchema,
		Identity: contracts.RoomWorkloadIdentity{WorkloadID: workloadID, Kind: domain.KindRoomMedia,
			RoomID: "room", ChannelID: "channel", SourceMemberID: "source", TargetSnapshot: 1,
			AuthorizationEpoch: 1, PlanEpoch: 1, UnitID: workloadID + "/unit", Epoch: 1, Attempt: 1, ExpiresAt: expiresAt},
		Details: contracts.MediaUnitDetails{SessionID: sessionID, Service: "publish",
			Profile: contracts.RoomMediaWebRTCWorkerProfile,
			Tracks:  []contracts.MediaTrack{{TrackID: "video-main", Kind: "video"}}, Layers: []contracts.MediaLayer{},
			Protection: contracts.MediaProtection{Scheme: contracts.RoomMediaProtectionSchemeV1,
				KeyScope: contracts.RoomMediaProtectionChannel, Required: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return domain.Spec{WorkloadID: workloadID, AttemptID: "attempt", Kind: domain.KindRoomMedia, Payload: payload,
		Identity: domain.Identity{WorkerID: "worker", NodeID: "node"}}
}
