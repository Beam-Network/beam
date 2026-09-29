package roomworkloads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

func TestDirectMessageHandlerUploadsCiphertextOnceAndWaitsForEveryTargetAck(t *testing.T) {
	handler := NewDirectMessageHandler(DirectMessageConfig{ListenAddress: "127.0.0.1:0",
		AdvertiseURL: "https://127.0.0.1:{port}"})
	t.Cleanup(func() { _ = handler.Close() })
	now := time.Now().UTC()
	record, _ := json.Marshal(messageRecord{MessageID: "message-1", Ciphertext: []byte("ciphertext")})
	payload, _ := json.Marshal(contracts.RoomWorkerSpec[contracts.MessageUnitDetails]{Schema: contracts.RoomWorkloadSchema,
		Identity: contracts.RoomWorkloadIdentity{WorkloadID: "workload-1", Kind: domain.KindRoomMessage,
			RoomID: "room-1", ChannelID: "channel-1", SourceMemberID: "source", TargetSnapshot: 1,
			AuthorizationEpoch: 1, PlanEpoch: 1, UnitID: "unit-1", Epoch: 1, Attempt: 1,
			WorkerID: "worker-1", ExpiresAt: now.Add(time.Minute)},
		Source: contracts.RoomPathLease{PathID: "source", Role: "source", Protocol: "http",
			Endpoints: []contracts.HTTPEndpoint{{URL: "https://worker.invalid/source"}}, ExpiresAt: now.Add(time.Minute)},
		Targets: []contracts.RoomWorkerTarget{
			{MemberID: "target-a", Path: contracts.RoomPathLease{PathID: "target-a", Role: "target", TargetMemberID: "target-a", Protocol: "http", Endpoints: []contracts.HTTPEndpoint{{URL: "https://worker.invalid/a"}}, ExpiresAt: now.Add(time.Minute)}},
			{MemberID: "target-b", Path: contracts.RoomPathLease{PathID: "target-b", Role: "target", TargetMemberID: "target-b", Protocol: "http", Endpoints: []contracts.HTTPEndpoint{{URL: "https://worker.invalid/b"}}, ExpiresAt: now.Add(time.Minute)}},
		}, Details: contracts.MessageUnitDetails{MessageID: "message-1", ContentType: "application/json", SizeBytes: int64(len("ciphertext"))}})
	spec := domain.Spec{WorkloadID: "workload-1", AttemptID: "attempt-1", Identity: domain.Identity{WorkerID: "worker-1"},
		Kind: domain.KindRoomMessage, Class: domain.ClassJob, Payload: payload}
	progress := make(chan map[string]string, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = workloadprogress.WithReporter(ctx, func(value map[string]string) { progress <- value })
	resultCh := make(chan domain.Result, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := handler.Execute(ctx, spec)
		resultCh <- result
		errCh <- err
	}()
	var initial contracts.MessageProgressDetails
	if err := json.Unmarshal([]byte((<-progress)["room_progress_details"]), &initial); err != nil || initial.Runtime == nil {
		t.Fatalf("missing private runtime progress: %v", err)
	}
	client := pinnedMessageClient(t, initial.Runtime.TLSCertificateSHA256)
	call := func(method, suffix string, body []byte) *http.Response {
		request, _ := http.NewRequestWithContext(ctx, method, initial.Runtime.BaseURL+suffix, bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+initial.Runtime.AccessToken)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	legacy, _ := json.Marshal(map[string]any{"message_id": "message-1", "payload": []byte("ciphertext")})
	response := call(http.MethodPost, "/source", legacy)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("legacy source status=%d", response.StatusCode)
	}
	type targetResult struct {
		member   string
		response *http.Response
		err      error
	}
	targetResults := make(chan targetResult, 2)
	targetStarted := make(chan struct{}, 2)
	for _, member := range []string{"target-a", "target-b"} {
		go func() {
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet,
				initial.Runtime.BaseURL+"/targets/"+member, nil)
			request.Header.Set("Authorization", "Bearer "+initial.Runtime.AccessToken)
			targetStarted <- struct{}{}
			response, err := client.Do(request)
			targetResults <- targetResult{member: member, response: response, err: err}
		}()
	}
	<-targetStarted
	<-targetStarted
	select {
	case result := <-targetResults:
		t.Fatalf("target %s completed before source upload: %v", result.member, result.err)
	case <-time.After(100 * time.Millisecond):
	}
	response = call(http.MethodPost, "/source", record)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("source status=%d", response.StatusCode)
	}
	response = call(http.MethodPost, "/source", record)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("idempotent source status=%d", response.StatusCode)
	}
	conflicting, _ := json.Marshal(messageRecord{MessageID: "message-1", Ciphertext: []byte("different!")})
	response = call(http.MethodPost, "/source", conflicting)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting source status=%d", response.StatusCode)
	}
	for range 2 {
		var result targetResult
		select {
		case result = <-targetResults:
		case <-time.After(2 * time.Second):
			t.Fatal("waiting target did not receive source ciphertext")
		}
		if result.err != nil {
			t.Fatalf("target %s download failed: %v", result.member, result.err)
		}
		got, _ := io.ReadAll(result.response.Body)
		result.response.Body.Close()
		if result.response.StatusCode != http.StatusOK || !bytes.Equal(got, record) {
			t.Fatalf("target %s did not receive exact ciphertext", result.member)
		}
		ack, _ := json.Marshal(contracts.MessageDelivery{TargetMemberID: result.member, State: "delivered"})
		response = call(http.MethodPost, "/targets/"+result.member+"/ack", ack)
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("target %s ack status=%d", result.member, response.StatusCode)
		}
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	result := <-resultCh
	if result.BytesProcessed != int64(len(record)) {
		t.Fatalf("source bytes=%d want=%d", result.BytesProcessed, len(record))
	}
}

func pinnedMessageClient(t *testing.T, fingerprint string) *http.Client {
	t.Helper()
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13,
		ServerName: fingerprint[:32] + "." + fingerprint[32:] + ".room-worker.invalid", InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			digest := sha256.Sum256(state.PeerCertificates[0].Raw)
			if hex.EncodeToString(digest[:]) != fingerprint {
				t.Fatalf("certificate pin mismatch")
			}
			return nil
		}}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}
