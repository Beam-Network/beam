package roomworkloads

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

type streamOptions struct {
	targets     []string
	policy      string
	maxBuffer   int64
	heartbeatMS int64
	base        uint64
	expiresIn   time.Duration
	server      *DirectServer
	tune        func(*DirectStreamHandler)
}

type streamOutcome struct {
	result domain.Result
	err    error
}

type streamHarness struct {
	t        *testing.T
	runtime  contracts.RoomStreamRuntime
	identity contracts.RoomWorkloadIdentity
	targets  []string
	base     uint64
	client   *http.Client
	cancel   context.CancelFunc
	progress chan string
	done     chan streamOutcome
}

type streamErrorBody struct {
	Error        string         `json:"error"`
	Detail       string         `json:"detail"`
	Receipt      *streamReceipt `json:"receipt"`
	BaseSequence *uint64        `json:"base_sequence"`
	Reason       *string        `json:"reason"`
}

type streamReply struct {
	status  int
	header  http.Header
	body    []byte
	failure streamErrorBody
}

type decodedFrame struct {
	seq  uint64
	data []byte
}

func startStream(t *testing.T, options streamOptions) *streamHarness {
	t.Helper()
	if options.targets == nil {
		options.targets = []string{"target-a"}
	}
	if options.policy == "" {
		options.policy = "block"
	}
	if options.maxBuffer == 0 {
		options.maxBuffer = 1 << 20
	}
	if options.heartbeatMS == 0 {
		options.heartbeatMS = 30_000
	}
	if options.base == 0 {
		options.base = 1
	}
	if options.expiresIn == 0 {
		options.expiresIn = time.Minute
	}
	server := options.server
	if server == nil {
		server = NewDirectServer(DirectServerConfig{ListenAddress: "127.0.0.1:0", AdvertiseURL: "https://127.0.0.1:{port}"})
		t.Cleanup(func() { _ = server.Close() })
	}
	handler := NewDirectStreamHandler(server)
	handler.checkInterval = 10 * time.Millisecond
	if options.tune != nil {
		options.tune(handler)
	}
	now := time.Now().UTC()
	identity := contracts.RoomWorkloadIdentity{WorkloadID: "workload-1", Kind: domain.KindRoomStream, RoomID: "room-1",
		ChannelID: "channel-1", SourceMemberID: "source", TargetSnapshot: 1, AuthorizationEpoch: 1, PlanEpoch: 1,
		UnitID: "unit-1", Epoch: 1, Attempt: 1, WorkerID: "worker-1", ExpiresAt: now.Add(options.expiresIn)}
	targets := make([]contracts.RoomWorkerTarget, 0, len(options.targets))
	for _, member := range options.targets {
		targets = append(targets, contracts.RoomWorkerTarget{MemberID: member, Path: contracts.RoomPathLease{PathID: member,
			Role: "target", TargetMemberID: member, Protocol: "btr-stream",
			Endpoints: []contracts.HTTPEndpoint{{URL: "https://worker.invalid/" + member}}, ExpiresAt: identity.ExpiresAt}})
	}
	payload, _ := json.Marshal(contracts.RoomWorkerSpec[contracts.StreamUnitDetails]{Schema: contracts.RoomWorkloadSchema,
		Identity: identity, Source: contracts.RoomPathLease{PathID: "source", Role: "source", Protocol: "btr-stream",
			Endpoints: []contracts.HTTPEndpoint{{URL: "https://worker.invalid/source"}}, ExpiresAt: identity.ExpiresAt},
		Targets: targets, Details: contracts.StreamUnitDetails{SessionID: "workload-1",
			Protocol: contracts.RoomStreamProtectionProtocol, BackpressurePolicy: options.policy, MaxBufferBytes: options.maxBuffer,
			HeartbeatTimeoutMS: options.heartbeatMS}})
	spec := domain.Spec{WorkloadID: "room-workload-1", AttemptID: "epoch-1-attempt-1", Identity: domain.Identity{WorkerID: "worker-1"},
		Kind: domain.KindRoomStream, Class: domain.ClassSession, Payload: payload}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	progress := make(chan string, 512)
	ctx = workloadprogress.WithReporter(ctx, func(values map[string]string) {
		select {
		case progress <- values["room_progress_details"]:
		default:
		}
	})
	done := make(chan streamOutcome, 1)
	go func() {
		result, err := handler.Execute(ctx, spec)
		done <- streamOutcome{result, err}
	}()
	var first string
	select {
	case first = <-progress:
	case outcome := <-done:
		t.Fatalf("stream ended before reporting its runtime: %v", outcome.err)
	case <-time.After(5 * time.Second):
		t.Fatal("stream runtime was not reported")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(first), &keys); err != nil || len(keys) != 1 || keys["runtime"] == nil {
		t.Fatalf("first progress must be runtime-only, got %s", first)
	}
	var details contracts.StreamProgressDetails
	if err := json.Unmarshal([]byte(first), &details); err != nil || details.Runtime == nil {
		t.Fatalf("runtime progress did not decode: %v", err)
	}
	return &streamHarness{t: t, runtime: *details.Runtime, identity: identity, targets: options.targets, base: options.base,
		client: pinnedMessageClient(t, details.Runtime.TLSCertificateSHA256), cancel: cancel, progress: progress, done: done}
}

func (h *streamHarness) targetToken(member string) string {
	for _, target := range h.runtime.TargetAccessTokens {
		if target.TargetMemberID == member {
			return target.AccessToken
		}
	}
	h.t.Fatalf("no token for %s", member)
	return ""
}

func (h *streamHarness) request(method, path, token string, body io.Reader, headers map[string]string) streamReply {
	h.t.Helper()
	request, err := http.NewRequest(method, h.runtime.BaseURL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("X-Beam-Stream-Epoch", "1")
	request.Header.Set("X-Beam-Stream-Attempt", "1")
	request.Header.Set("X-Beam-Stream-Base-Sequence", strconv.FormatUint(h.base, 10))
	for key, value := range headers {
		if value == "" {
			request.Header.Del(key)
		} else {
			request.Header.Set(key, value)
		}
	}
	response, err := h.client.Do(request)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	encoded, _ := io.ReadAll(response.Body)
	reply := streamReply{status: response.StatusCode, header: response.Header, body: encoded}
	if response.StatusCode >= 400 {
		if err := json.Unmarshal(encoded, &reply.failure); err != nil {
			h.t.Fatalf("%s %s returned %d without an error envelope: %s", method, path, response.StatusCode, encoded)
		}
	}
	return reply
}

func (h *streamHarness) source(method, path string, body []byte) streamReply {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	return h.request(method, path, h.runtime.SourceAccessToken, reader, nil)
}

func (h *streamHarness) target(member, method, path string, body []byte) streamReply {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	return h.request(method, "/targets/"+member+path, h.targetToken(member), reader, nil)
}

func (h *streamHarness) postFrame(seq uint64, data []byte) (int, streamReceipt, streamErrorBody) {
	h.t.Helper()
	reply := h.source(http.MethodPost, fmt.Sprintf("/source/frames/%d", seq), data)
	var receipt streamReceipt
	if reply.status == http.StatusOK {
		decodeJSON(h.t, reply.body, &receipt)
	}
	return reply.status, receipt, reply.failure
}

func (h *streamHarness) mustPost(seq uint64, data []byte) streamReceipt {
	h.t.Helper()
	status, receipt, failure := h.postFrame(seq, data)
	if status != http.StatusOK {
		h.t.Fatalf("frame %d status=%d error=%+v", seq, status, failure)
	}
	return receipt
}

func (h *streamHarness) frames(member string, query string) (streamReply, []decodedFrame) {
	h.t.Helper()
	reply := h.target(member, http.MethodGet, "/frames?"+query, nil)
	if reply.status != http.StatusOK {
		return reply, nil
	}
	if reply.header.Get("Content-Type") != streamFramesContentType {
		h.t.Fatalf("frames content type %q", reply.header.Get("Content-Type"))
	}
	var frames []decodedFrame
	body := reply.body
	for len(body) > 0 {
		if len(body) < streamRecordHeaderBytes {
			h.t.Fatalf("truncated frame record header")
		}
		seq, size := binary.BigEndian.Uint64(body[:8]), binary.BigEndian.Uint32(body[8:12])
		if size == 0 || size > contracts.RoomStreamMaxFrameBytes || int(size) > len(body)-streamRecordHeaderBytes {
			h.t.Fatalf("invalid frame record length %d", size)
		}
		frames = append(frames, decodedFrame{seq: seq, data: body[12 : 12+size]})
		body = body[12+size:]
	}
	return reply, frames
}

func (h *streamHarness) ack(member string, delivered uint64) streamReply {
	h.t.Helper()
	return h.target(member, http.MethodPost, "/ack", []byte(fmt.Sprintf(`{"delivered_through":%d}`, delivered)))
}

func (h *streamHarness) end(final uint64, reason string) (streamReply, streamReceipt) {
	h.t.Helper()
	reply := h.source(http.MethodPost, "/source/end", []byte(fmt.Sprintf(`{"final_sequence":%d,"reason":%q}`, final, reason)))
	var receipt streamReceipt
	if reply.status == http.StatusOK {
		decodeJSON(h.t, reply.body, &receipt)
	}
	return reply, receipt
}

func (h *streamHarness) receipt() streamReceipt {
	h.t.Helper()
	reply := h.source(http.MethodPost, "/heartbeat", nil)
	if reply.status != http.StatusOK {
		h.t.Fatalf("source heartbeat status=%d error=%+v", reply.status, reply.failure)
	}
	var receipt streamReceipt
	decodeJSON(h.t, reply.body, &receipt)
	return receipt
}

func (h *streamHarness) finish() (domain.Result, contracts.StreamResultDetails) {
	h.t.Helper()
	select {
	case outcome := <-h.done:
		if outcome.err != nil {
			h.t.Fatalf("stream failed: %v", outcome.err)
		}
		var details contracts.StreamResultDetails
		decodeJSON(h.t, []byte(outcome.result.Outputs["room_result_details"]), &details)
		return outcome.result, details
	case <-time.After(8 * time.Second):
		h.t.Fatal("stream did not finish")
		return domain.Result{}, contracts.StreamResultDetails{}
	}
}

// keepAlive sends a heartbeat for each token every interval until stopped.
func (h *streamHarness) keepAlive(interval time.Duration, tokens ...string) func() {
	stop := make(chan struct{})
	var group sync.WaitGroup
	for _, token := range tokens {
		group.Add(1)
		go func() {
			defer group.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					request, _ := http.NewRequest(http.MethodPost, h.runtime.BaseURL+"/heartbeat", nil)
					request.Header.Set("Authorization", "Bearer "+token)
					request.Header.Set("X-Beam-Stream-Epoch", "1")
					request.Header.Set("X-Beam-Stream-Attempt", "1")
					if response, err := h.client.Do(request); err == nil {
						_, _ = io.Copy(io.Discard, response.Body)
						response.Body.Close()
					}
				}
			}
		}()
	}
	return func() { close(stop); group.Wait() }
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not reached in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func decodeJSON(t *testing.T, encoded []byte, value any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		t.Fatalf("decode %s: %v", encoded, err)
	}
}

func frameData(seq uint64, size int) []byte {
	data := bytes.Repeat([]byte{byte(seq)}, size)
	if seq == 1 {
		copy(data, "BMS1")
	}
	return data
}

func targetState(receipt streamReceipt, member string) contracts.RoomStreamTargetState {
	for _, target := range receipt.Targets {
		if target.TargetMemberID == member {
			return target
		}
	}
	return contracts.RoomStreamTargetState{}
}

func reasonOf(state contracts.RoomStreamTargetState) string {
	if state.Reason == nil {
		return ""
	}
	return *state.Reason
}

func TestDirectStreamReportsRuntimeWithRoleTokens(t *testing.T) {
	h := startStream(t, streamOptions{targets: []string{"target-a", "target-b"}})
	runtime := h.runtime
	if err := runtime.Validate(h.identity, []string{"target-a", "target-b"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(runtime.BaseURL)
	if err != nil || parsed.Path != "/v1/room-streams/workload-1-unit-1-1" || runtime.Capability != contracts.RoomStreamDirectCapability ||
		runtime.Transport != "worker_https" || !runtime.ExpiresAt.Equal(h.identity.ExpiresAt) {
		t.Fatalf("unexpected runtime %+v", runtime)
	}
	tokens := map[string]bool{runtime.SourceAccessToken: true}
	for _, target := range runtime.TargetAccessTokens {
		if tokens[target.AccessToken] || len(target.AccessToken) != 43 {
			t.Fatalf("target tokens must be distinct 43-character tokens: %+v", runtime.TargetAccessTokens)
		}
		tokens[target.AccessToken] = true
	}
	if receipt := h.receipt(); receipt.State != streamOpen || receipt.BaseSequence != 0 || receipt.MaxBufferBytes != 1<<20 ||
		len(receipt.Targets) != 2 || receipt.Targets[0].State != contracts.RoomStreamTargetJoining || receipt.FinalSequence != nil {
		t.Fatalf("unexpected initial receipt %+v", receipt)
	}
	h.cancel()
	select {
	case outcome := <-h.done:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("cancelled stream returned %v", outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled stream did not return")
	}
	if reply := h.source(http.MethodPost, "/heartbeat", nil); reply.status != http.StatusGone || reply.failure.Error != "session_ended" {
		t.Fatalf("lingering cancelled session answered %d %+v", reply.status, reply.failure)
	}
}

func TestDirectStreamAuthorizationMatrix(t *testing.T) {
	h := startStream(t, streamOptions{targets: []string{"target-a", "target-b"}})
	source, targetA := h.runtime.SourceAccessToken, h.targetToken("target-a")
	cases := []struct {
		name    string
		method  string
		path    string
		token   string
		headers map[string]string
		status  int
		code    string
	}{
		{"missing token", http.MethodPost, "/heartbeat", "", nil, http.StatusUnauthorized, "unauthorized"},
		{"unknown token", http.MethodPost, "/heartbeat", strings.Repeat("A", 43), nil, http.StatusUnauthorized, "unauthorized"},
		{"missing epoch", http.MethodPost, "/heartbeat", source, map[string]string{"X-Beam-Stream-Epoch": ""}, http.StatusConflict, "attempt_mismatch"},
		{"other attempt", http.MethodPost, "/heartbeat", targetA, map[string]string{"X-Beam-Stream-Attempt": "2"}, http.StatusConflict, "attempt_mismatch"},
		{"target posts frames", http.MethodPost, "/source/frames/1", targetA, nil, http.StatusForbidden, "forbidden"},
		{"target reads receipt", http.MethodGet, "/source/receipt", targetA, nil, http.StatusForbidden, "forbidden"},
		{"target ends source", http.MethodPost, "/source/end", targetA, nil, http.StatusForbidden, "forbidden"},
		{"source reads target", http.MethodGet, "/targets/target-a/frames?after=0", source, nil, http.StatusForbidden, "forbidden"},
		{"target reads another", http.MethodGet, "/targets/target-b/frames?after=0", targetA, nil, http.StatusForbidden, "forbidden"},
		{"target acks another", http.MethodPost, "/targets/target-b/ack", targetA, nil, http.StatusForbidden, "forbidden"},
		{"target closes another", http.MethodPost, "/targets/target-b/close", targetA, nil, http.StatusForbidden, "forbidden"},
		{"unknown target", http.MethodGet, "/targets/nobody/frames?after=0", targetA, nil, http.StatusNotFound, "target_not_found"},
		{"wrong method", http.MethodGet, "/source/frames/1", source, nil, http.StatusMethodNotAllowed, "method_not_allowed"},
		{"unknown endpoint", http.MethodPost, "/source/rewind", source, nil, http.StatusNotFound, "not_found"},
		{"missing after", http.MethodGet, "/targets/target-a/frames", targetA, nil, http.StatusBadRequest, "invalid_request"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			reply := h.request(test.method, test.path, test.token, nil, test.headers)
			if reply.status != test.status || reply.failure.Error != test.code {
				t.Fatalf("status=%d error=%+v want %d %s", reply.status, reply.failure, test.status, test.code)
			}
		})
	}
	unknown := *h
	unknown.runtime.BaseURL = strings.TrimSuffix(h.runtime.BaseURL, "workload-1-unit-1-1") + "workload-9-unit-1-1"
	if reply := unknown.source(http.MethodPost, "/heartbeat", nil); reply.status != http.StatusNotFound || reply.failure.Error != "session_not_found" {
		t.Fatalf("unknown session status=%d error=%+v", reply.status, reply.failure)
	}
	var heartbeat struct {
		State         string  `json:"state"`
		FinalSequence *uint64 `json:"final_sequence"`
	}
	reply := h.request(http.MethodPost, "/heartbeat", targetA, nil, nil)
	decodeJSON(t, reply.body, &heartbeat)
	if reply.status != http.StatusOK || heartbeat.State != contracts.RoomStreamTargetJoining || heartbeat.FinalSequence != nil {
		t.Fatalf("target heartbeat status=%d body=%s", reply.status, reply.body)
	}
	if receipt := h.receipt(); receipt.State != streamOpen {
		t.Fatalf("source heartbeat receipt %+v", receipt)
	}
}

func TestDirectStreamRejectsInvalidFrames(t *testing.T) {
	h := startStream(t, streamOptions{})
	chunked := h.request(http.MethodPost, "/source/frames/1", h.runtime.SourceAccessToken,
		io.MultiReader(bytes.NewReader([]byte("BMS1chunked"))), nil)
	cases := []struct {
		name  string
		reply streamReply
		want  int
		code  string
	}{
		{"chunked body", chunked, http.StatusLengthRequired, "length_required"},
		{"oversized", h.source(http.MethodPost, "/source/frames/1", frameData(1, contracts.RoomStreamMaxFrameBytes+1)), http.StatusRequestEntityTooLarge, "frame_too_large"},
		{"empty", h.source(http.MethodPost, "/source/frames/1", nil), http.StatusBadRequest, "empty_frame"},
		{"missing header", h.source(http.MethodPost, "/source/frames/1", []byte("ciphertext")), http.StatusBadRequest, "invalid_protected_stream"},
		{"zero sequence", h.source(http.MethodPost, "/source/frames/0", []byte("x")), http.StatusBadRequest, "invalid_request"},
		{"bad sequence", h.source(http.MethodPost, "/source/frames/abc", []byte("x")), http.StatusBadRequest, "invalid_request"},
		{"bad end", h.source(http.MethodPost, "/source/end", []byte(`{"final_sequence":1,"reason":"closed"}`)), http.StatusBadRequest, "invalid_request"},
	}
	for _, test := range cases {
		if test.reply.status != test.want || test.reply.failure.Error != test.code {
			t.Fatalf("%s: status=%d error=%+v want %d %s", test.name, test.reply.status, test.reply.failure, test.want, test.code)
		}
	}
	if receipt := h.receipt(); receipt.AcceptedThrough != 0 || receipt.BaseSequence != 0 {
		t.Fatalf("rejected frames changed the session: %+v", receipt)
	}
}

func TestDirectStreamAcceptsInOrderHoldsGapsAndDetectsConflicts(t *testing.T) {
	h := startStream(t, streamOptions{tune: func(handler *DirectStreamHandler) { handler.holdTimeout = 300 * time.Millisecond }})
	if receipt := h.mustPost(1, frameData(1, 100)); receipt.BaseSequence != 1 || receipt.AcceptedThrough != 1 || receipt.CommittedThrough != 0 ||
		receipt.BufferedBytes != 100 {
		t.Fatalf("first frame receipt %+v", receipt)
	}
	held := make(chan streamReceipt, 1)
	go func() { held <- h.mustPost(3, frameData(3, 100)) }()
	select {
	case receipt := <-held:
		t.Fatalf("frame 3 was accepted before frame 2: %+v", receipt)
	case <-time.After(100 * time.Millisecond):
	}
	h.mustPost(2, frameData(2, 100))
	select {
	case receipt := <-held:
		if receipt.AcceptedThrough != 3 {
			t.Fatalf("held frame receipt %+v", receipt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("held frame was not accepted after its predecessor")
	}
	if receipt := h.mustPost(2, frameData(2, 100)); receipt.AcceptedThrough != 3 {
		t.Fatalf("idempotent duplicate receipt %+v", receipt)
	}
	if status, _, failure := h.postFrame(2, frameData(9, 100)); status != http.StatusConflict || failure.Error != "frame_conflict" {
		t.Fatalf("conflicting duplicate status=%d error=%+v", status, failure)
	}
	if status, _, failure := h.postFrame(12, frameData(12, 100)); status != http.StatusConflict || failure.Error != "sequence_gap" ||
		failure.Receipt == nil || failure.Receipt.AcceptedThrough != 3 {
		t.Fatalf("frame beyond the pipeline window status=%d error=%+v", status, failure)
	}
	started := time.Now()
	if status, _, failure := h.postFrame(5, frameData(5, 100)); status != http.StatusConflict || failure.Error != "sequence_gap" ||
		time.Since(started) < 250*time.Millisecond {
		t.Fatalf("unfilled gap status=%d error=%+v after %s", status, failure, time.Since(started))
	}
	_, frames := h.frames("target-a", "after=0")
	if len(frames) != 3 || frames[0].seq != 1 || frames[2].seq != 3 || !bytes.Equal(frames[1].data, frameData(2, 100)) {
		t.Fatalf("target frames %+v", frames)
	}
	h.ack("target-a", 3)
	if receipt := h.mustPost(2, frameData(9, 100)); receipt.CommittedThrough != 3 || receipt.BufferedBytes != 0 {
		t.Fatalf("a committed and freed duplicate must be acknowledged: %+v", receipt)
	}
}

func TestDirectStreamBaseIsDeclaredBySource(t *testing.T) {
	h := startStream(t, streamOptions{base: 101})
	missing := h.request(http.MethodPost, "/source/frames/101", h.runtime.SourceAccessToken, bytes.NewReader(frameData(101, 64)),
		map[string]string{"X-Beam-Stream-Base-Sequence": ""})
	zero := h.request(http.MethodPost, "/source/end", h.runtime.SourceAccessToken, strings.NewReader(`{"final_sequence":100,"reason":"eof"}`),
		map[string]string{"X-Beam-Stream-Base-Sequence": "0"})
	for name, reply := range map[string]streamReply{"missing base": missing, "zero base": zero} {
		if reply.status != http.StatusBadRequest || reply.failure.Error != "invalid_request" {
			t.Fatalf("%s status=%d error=%+v", name, reply.status, reply.failure)
		}
	}
	if receipt := h.receipt(); receipt.BaseSequence != 0 || receipt.AcceptedThrough != 0 {
		t.Fatalf("a rejected request fixed the base: %+v", receipt)
	}
	results := make(chan streamReceipt, 3)
	go func() { results <- h.mustPost(103, frameData(103, 64)) }()
	time.Sleep(30 * time.Millisecond)
	go func() { results <- h.mustPost(101, frameData(101, 64)) }()
	go func() { results <- h.mustPost(102, frameData(102, 64)) }()
	for range 3 {
		select {
		case receipt := <-results:
			if receipt.BaseSequence != 101 {
				t.Fatalf("base must be the declared base: %+v", receipt)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("pipelined first frames were not accepted")
		}
	}
	other := h.request(http.MethodPost, "/source/frames/104", h.runtime.SourceAccessToken, bytes.NewReader(frameData(104, 64)),
		map[string]string{"X-Beam-Stream-Base-Sequence": "100"})
	if other.status != http.StatusConflict || other.failure.Error != "base_mismatch" || other.failure.Receipt == nil ||
		other.failure.Receipt.BaseSequence != 101 {
		t.Fatalf("frame with another base status=%d error=%+v", other.status, other.failure)
	}
	other = h.request(http.MethodPost, "/source/end", h.runtime.SourceAccessToken, strings.NewReader(`{"final_sequence":103,"reason":"eof"}`),
		map[string]string{"X-Beam-Stream-Base-Sequence": "102"})
	if other.status != http.StatusConflict || other.failure.Error != "base_mismatch" {
		t.Fatalf("end with another base status=%d error=%+v", other.status, other.failure)
	}
	if receipt := h.receipt(); receipt.AcceptedThrough != 103 || receipt.CommittedThrough != 100 {
		t.Fatalf("receipt after failover base %+v", receipt)
	}
	reply, _ := h.frames("target-a", "after=50")
	if reply.status != http.StatusConflict || reply.failure.Error != "before_base" || reply.failure.BaseSequence == nil ||
		*reply.failure.BaseSequence != 101 {
		t.Fatalf("draining target status=%d error=%+v", reply.status, reply.failure)
	}
	reply, frames := h.frames("target-a", "after=100")
	if reply.status != http.StatusOK || len(frames) != 3 || frames[0].seq != 101 ||
		reply.header.Get("X-Beam-Stream-Base-Sequence") != "101" || reply.header.Get("X-Beam-Stream-Accepted-Through") != "103" {
		t.Fatalf("resumed target status=%d frames=%+v headers=%v", reply.status, frames, reply.header)
	}
	if status, _, failure := h.postFrame(99, frameData(99, 64)); status != http.StatusConflict || failure.Error != "sequence_gap" {
		t.Fatalf("frame below the base status=%d error=%+v", status, failure)
	}
}

func TestDirectStreamWindowBlocksAtTheSlowestTarget(t *testing.T) {
	h := startStream(t, streamOptions{targets: []string{"target-a", "target-b"}, maxBuffer: contracts.RoomStreamMinBufferBytes,
		tune: func(handler *DirectStreamHandler) { handler.overrunGrace = 50 * time.Millisecond }})
	h.mustPost(1, frameData(1, contracts.RoomStreamMaxFrameBytes))
	h.mustPost(2, frameData(2, contracts.RoomStreamMaxFrameBytes))
	status, _, failure := h.postFrame(3, frameData(3, 1))
	if status != http.StatusTooManyRequests || failure.Error != "window_full" || failure.Receipt == nil ||
		failure.Receipt.BufferedBytes != contracts.RoomStreamMinBufferBytes {
		t.Fatalf("full window status=%d error=%+v", status, failure)
	}
	h.frames("target-a", "after=0")
	h.ack("target-a", 2)
	stop := h.keepAlive(50*time.Millisecond, h.targetToken("target-b"))
	defer stop()
	time.Sleep(300 * time.Millisecond)
	receipt := h.receipt()
	if targetState(receipt, "target-b").State != contracts.RoomStreamTargetJoining || receipt.CommittedThrough != 0 {
		t.Fatalf("block must keep a live slow target and hold the commit: %+v", receipt)
	}
	if status, _, _ := h.postFrame(3, frameData(3, 1)); status != http.StatusTooManyRequests {
		t.Fatalf("window must stay full while the slowest target lags: %d", status)
	}
	h.frames("target-b", "after=0&max_bytes=1")
	if reply := h.ack("target-b", 1); reply.status != http.StatusOK {
		t.Fatalf("slow target ack status=%d", reply.status)
	}
	if receipt := h.mustPost(3, frameData(3, 1)); receipt.CommittedThrough != 1 {
		t.Fatalf("commit did not follow the slowest target: %+v", receipt)
	}
}

func TestDirectStreamDropOldestEvictsLaggingTargets(t *testing.T) {
	h := startStream(t, streamOptions{targets: []string{"target-a", "target-b"}, policy: "drop_oldest",
		maxBuffer: contracts.RoomStreamMinBufferBytes, tune: func(handler *DirectStreamHandler) { handler.overrunGrace = 100 * time.Millisecond }})
	stop := h.keepAlive(50*time.Millisecond, h.targetToken("target-b"))
	defer stop()
	h.mustPost(1, frameData(1, contracts.RoomStreamMaxFrameBytes))
	h.mustPost(2, frameData(2, contracts.RoomStreamMaxFrameBytes))
	h.frames("target-a", "after=0")
	h.ack("target-a", 2)
	waitFor(t, 3*time.Second, func() bool { return targetState(h.receipt(), "target-b").State == contracts.RoomStreamTargetDropped })
	receipt := h.receipt()
	if reasonOf(targetState(receipt, "target-b")) != contracts.RoomStreamReasonSlowTargetOverrun || receipt.CommittedThrough != 2 ||
		targetState(receipt, "target-a").State != contracts.RoomStreamTargetActive {
		t.Fatalf("lagging target was not evicted cleanly: %+v", receipt)
	}
	h.mustPost(3, frameData(3, 1))
	reply, _ := h.frames("target-b", "after=0")
	if reply.status != http.StatusGone || reply.failure.Error != "target_closed" || reply.failure.Reason == nil ||
		*reply.failure.Reason != contracts.RoomStreamReasonSlowTargetOverrun {
		t.Fatalf("dropped target status=%d error=%+v", reply.status, reply.failure)
	}
}

func TestDirectStreamLongPollBatchesAndFinalSequence(t *testing.T) {
	h := startStream(t, streamOptions{})
	reply, _ := h.frames("target-a", "after=0&wait_ms=50")
	if reply.status != http.StatusNoContent || reply.header.Get("X-Beam-Stream-Base-Sequence") != "0" ||
		reply.header.Get("X-Beam-Stream-Accepted-Through") != "0" || reply.header.Get("X-Beam-Stream-Final-Sequence") != "" {
		t.Fatalf("empty long poll status=%d headers=%v", reply.status, reply.header)
	}
	polled := make(chan []decodedFrame, 1)
	go func() {
		_, frames := h.frames("target-a", "after=0&wait_ms=4000")
		polled <- frames
	}()
	time.Sleep(50 * time.Millisecond)
	h.mustPost(1, frameData(1, 10))
	select {
	case frames := <-polled:
		if len(frames) != 1 || frames[0].seq != 1 || !bytes.Equal(frames[0].data, frameData(1, 10)) {
			t.Fatalf("long poll frames %+v", frames)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll did not return the new frame")
	}
	h.mustPost(2, frameData(2, 1000))
	h.mustPost(3, frameData(3, 1000))
	if _, frames := h.frames("target-a", "after=1&max_bytes=1"); len(frames) != 1 || frames[0].seq != 2 {
		t.Fatalf("max_bytes must still return one frame: %+v", frames)
	}
	if _, frames := h.frames("target-a", "after=1"); len(frames) != 2 || frames[0].seq != 2 || frames[1].seq != 3 ||
		!bytes.Equal(frames[1].data, frameData(3, 1000)) {
		t.Fatalf("batch frames %+v", frames)
	}
	if reply, _ := h.end(2, "eof"); reply.status != http.StatusConflict || reply.failure.Error != "frames_missing" {
		t.Fatalf("end before accepted_through status=%d error=%+v", reply.status, reply.failure)
	}
	if reply, receipt := h.end(3, "eof"); reply.status != http.StatusOK || receipt.State != streamEnding ||
		receipt.FinalSequence == nil || *receipt.FinalSequence != 3 {
		t.Fatalf("end status=%d receipt=%+v", reply.status, receipt)
	}
	if status, _, failure := h.postFrame(4, frameData(4, 1)); status != http.StatusConflict || failure.Error != "frame_conflict" {
		t.Fatalf("frame after the final sequence status=%d error=%+v", status, failure)
	}
	started := time.Now()
	reply, _ = h.frames("target-a", "after=3&wait_ms=4000")
	if reply.status != http.StatusNoContent || reply.header.Get("X-Beam-Stream-Final-Sequence") != "3" || time.Since(started) > time.Second {
		t.Fatalf("final long poll status=%d headers=%v after %s", reply.status, reply.header, time.Since(started))
	}
	result, details := h.finish()
	if details.TerminalReason != contracts.RoomStreamEnded || details.Sequence != 3 || details.Bytes != 2010 || result.BytesProcessed != 2010 ||
		details.Targets[0].State != contracts.RoomStreamTargetCompleted {
		t.Fatalf("result %+v bytes=%d", details, result.BytesProcessed)
	}
}

func TestDirectStreamAcksAreMonotonicAndBoundedByServedFrames(t *testing.T) {
	h := startStream(t, streamOptions{})
	for seq := uint64(1); seq <= 3; seq++ {
		h.mustPost(seq, frameData(seq, 100))
	}
	if _, frames := h.frames("target-a", "after=0&max_bytes=1"); len(frames) != 1 {
		t.Fatalf("expected one served frame, got %d", len(frames))
	}
	if reply := h.ack("target-a", 2); reply.status != http.StatusBadRequest || reply.failure.Error != "ack_beyond_served" {
		t.Fatalf("ack beyond served status=%d error=%+v", reply.status, reply.failure)
	}
	var ack struct {
		DeliveredThrough uint64  `json:"delivered_through"`
		CommittedThrough uint64  `json:"committed_through"`
		FinalSequence    *uint64 `json:"final_sequence"`
		State            string  `json:"state"`
	}
	reply := h.ack("target-a", 1)
	decodeJSON(t, reply.body, &ack)
	if reply.status != http.StatusOK || ack.DeliveredThrough != 1 || ack.CommittedThrough != 1 || ack.FinalSequence != nil || ack.State != "active" {
		t.Fatalf("ack status=%d body=%s", reply.status, reply.body)
	}
	if reply := h.ack("target-a", 1); reply.status != http.StatusOK {
		t.Fatalf("repeated ack status=%d", reply.status)
	}
	if reply := h.ack("target-a", 0); reply.status != http.StatusBadRequest || reply.failure.Error != "ack_beyond_served" {
		t.Fatalf("decreasing ack status=%d error=%+v", reply.status, reply.failure)
	}
	if reply := h.target("target-a", http.MethodPost, "/ack", []byte(`{"delivered":1}`)); reply.status != http.StatusBadRequest ||
		reply.failure.Error != "invalid_request" {
		t.Fatalf("malformed ack status=%d error=%+v", reply.status, reply.failure)
	}
	h.end(3, "eof")
	h.frames("target-a", "after=1")
	reply = h.ack("target-a", 3)
	decodeJSON(t, reply.body, &ack)
	if ack.State != contracts.RoomStreamTargetCompleted || ack.FinalSequence == nil || *ack.FinalSequence != 3 {
		t.Fatalf("final ack %s", reply.body)
	}
	if _, details := h.finish(); details.TerminalReason != contracts.RoomStreamEnded || details.Dropped != 0 {
		t.Fatalf("result %+v", details)
	}
}

func TestDirectStreamEndsWhenEveryTargetHasTheFinalFrame(t *testing.T) {
	h := startStream(t, streamOptions{targets: []string{"target-a", "target-b"},
		tune: func(handler *DirectStreamHandler) { handler.progressInterval = 50 * time.Millisecond }})
	total := int64(0)
	for seq := uint64(1); seq <= 4; seq++ {
		h.mustPost(seq, frameData(seq, int(seq)*1000))
		total += int64(seq) * 1000
	}
	for _, member := range h.targets {
		h.frames(member, "after=0")
	}
	h.ack("target-a", 4)
	committed := make(chan streamReceipt, 1)
	go func() {
		reply := h.source(http.MethodGet, "/source/receipt?committed_after=3&wait_ms=4000", nil)
		var receipt streamReceipt
		decodeJSON(t, reply.body, &receipt)
		committed <- receipt
	}()
	select {
	case receipt := <-committed:
		t.Fatalf("receipt returned before every target acked: %+v", receipt)
	case <-time.After(100 * time.Millisecond):
	}
	h.ack("target-b", 4)
	select {
	case receipt := <-committed:
		if receipt.CommittedThrough != 4 || receipt.BufferedBytes != 0 {
			t.Fatalf("receipt long poll %+v", receipt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("receipt long poll did not observe the commit")
	}
	reply, receipt := h.end(4, "eof")
	if reply.status != http.StatusOK || receipt.State != streamEnded || receipt.Targets[0].State != contracts.RoomStreamTargetCompleted {
		t.Fatalf("end status=%d receipt=%+v", reply.status, receipt)
	}
	result, details := h.finish()
	if details.TerminalReason != contracts.RoomStreamEnded || details.Sequence != 4 || details.Bytes != total ||
		result.BytesProcessed != total || details.Dropped != 0 || len(details.Targets) != 2 {
		t.Fatalf("result %+v bytes=%d", details, result.BytesProcessed)
	}
	sawCounters := false
	for len(h.progress) > 0 {
		var progress contracts.StreamProgressDetails
		decodeJSON(t, []byte(<-h.progress), &progress)
		if progress.Runtime == nil && len(progress.Targets) == 2 {
			sawCounters = true
		}
	}
	if !sawCounters {
		t.Fatal("no counters progress with per-target state was reported")
	}
	if status, _, failure := h.postFrame(5, frameData(5, 1)); status != http.StatusGone || failure.Error != "session_ended" {
		t.Fatalf("frame after the session ended status=%d error=%+v", status, failure)
	}
}

func TestDirectStreamSourceReceiptLongPollWakesOnStateChanges(t *testing.T) {
	h := startStream(t, streamOptions{targets: []string{"target-a", "target-b"}})
	started := time.Now()
	reply := h.source(http.MethodGet, "/source/receipt?committed_after=0&wait_ms=100", nil)
	if reply.status != http.StatusOK || time.Since(started) < 90*time.Millisecond {
		t.Fatalf("receipt long poll status=%d returned after %s", reply.status, time.Since(started))
	}
	woke := make(chan streamReceipt, 1)
	go func() {
		reply := h.source(http.MethodGet, "/source/receipt?committed_after=0&wait_ms=4000", nil)
		var receipt streamReceipt
		decodeJSON(t, reply.body, &receipt)
		woke <- receipt
	}()
	time.Sleep(50 * time.Millisecond)
	if reply := h.target("target-b", http.MethodPost, "/close",
		[]byte(`{"state":"dropped","delivered_through":0,"reason":"no_live_subscriber"}`)); reply.status != http.StatusNoContent {
		t.Fatalf("close status=%d", reply.status)
	}
	select {
	case receipt := <-woke:
		if state := targetState(receipt, "target-b"); state.State != contracts.RoomStreamTargetDropped || reasonOf(state) != "no_live_subscriber" {
			t.Fatalf("receipt after a target change %+v", receipt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("receipt long poll ignored a target state change")
	}
}

func TestDirectStreamLivenessDropsSilentTargets(t *testing.T) {
	h := startStream(t, streamOptions{targets: []string{"target-a", "target-b", "target-c"},
		heartbeatMS: contracts.RoomStreamMinHeartbeatTimeoutMS})
	stopSource := h.keepAlive(500*time.Millisecond, h.runtime.SourceAccessToken)
	defer stopSource()
	stopA := h.keepAlive(500*time.Millisecond, h.targetToken("target-a"))
	defer stopA()
	if reply := h.request(http.MethodPost, "/heartbeat", h.targetToken("target-b"), nil, nil); reply.status != http.StatusOK {
		t.Fatalf("target heartbeat status=%d", reply.status)
	}
	h.mustPost(1, frameData(1, 10))
	h.frames("target-a", "after=0")
	h.ack("target-a", 1)
	waitFor(t, 8*time.Second, func() bool {
		receipt := h.receipt()
		return targetState(receipt, "target-b").State == contracts.RoomStreamTargetDropped &&
			targetState(receipt, "target-c").State == contracts.RoomStreamTargetDropped
	})
	receipt := h.receipt()
	if reasonOf(targetState(receipt, "target-b")) != contracts.RoomStreamReasonTargetHeartbeatTimeout ||
		reasonOf(targetState(receipt, "target-c")) != contracts.RoomStreamReasonTargetJoinTimeout ||
		targetState(receipt, "target-a").State != contracts.RoomStreamTargetActive || receipt.CommittedThrough != 1 {
		t.Fatalf("liveness outcome %+v", receipt)
	}
	h.end(1, "eof")
	_, details := h.finish()
	if details.TerminalReason != contracts.RoomStreamEnded || details.Dropped != 2 || details.Targets[0].State != contracts.RoomStreamTargetCompleted {
		t.Fatalf("result %+v", details)
	}
}

func TestDirectStreamLostSourceFailsOpenTargets(t *testing.T) {
	h := startStream(t, streamOptions{heartbeatMS: contracts.RoomStreamMinHeartbeatTimeoutMS})
	stop := h.keepAlive(500*time.Millisecond, h.targetToken("target-a"))
	defer stop()
	h.mustPost(1, frameData(1, 10))
	_, details := h.finish()
	if details.TerminalReason != contracts.RoomStreamSourceLost || details.Targets[0].State != contracts.RoomStreamTargetFailed ||
		reasonOf(details.Targets[0]) != contracts.RoomStreamReasonSourceLost || details.Dropped != 1 {
		t.Fatalf("result %+v", details)
	}
	reply, _ := h.frames("target-a", "after=0")
	if reply.status != http.StatusGone || reply.failure.Error != "target_closed" || reply.failure.Reason == nil ||
		*reply.failure.Reason != contracts.RoomStreamReasonSourceLost {
		t.Fatalf("failed target status=%d error=%+v", reply.status, reply.failure)
	}
}

func TestDirectStreamAbortFailsOpenTargets(t *testing.T) {
	h := startStream(t, streamOptions{targets: []string{"target-a", "target-b"}})
	h.mustPost(1, frameData(1, 10))
	reply, receipt := h.end(0, "aborted")
	if reply.status != http.StatusOK || receipt.State != streamEnded {
		t.Fatalf("abort status=%d receipt=%+v", reply.status, receipt)
	}
	_, details := h.finish()
	if details.TerminalReason != contracts.RoomStreamSourceAborted || details.Dropped != 2 ||
		reasonOf(details.Targets[1]) != contracts.RoomStreamReasonSourceAborted {
		t.Fatalf("result %+v", details)
	}
}

func TestDirectStreamWithoutLiveTargetsEndsTheSession(t *testing.T) {
	h := startStream(t, streamOptions{targets: []string{"target-a", "target-b"}})
	h.mustPost(1, frameData(1, 10))
	for _, body := range []string{
		`{"state":"completed","delivered_through":0,"reason":"done"}`,
		`{"state":"dropped","delivered_through":0}`,
		`{"state":"dropped","delivered_through":0,"reason":""}`,
	} {
		if reply := h.target("target-a", http.MethodPost, "/close", []byte(body)); reply.status != http.StatusBadRequest ||
			reply.failure.Error != "invalid_request" {
			t.Fatalf("close %s status=%d error=%+v", body, reply.status, reply.failure)
		}
	}
	if reply := h.target("target-a", http.MethodPost, "/close", []byte(`{"state":"dropped","delivered_through":1,"reason":"no_live_subscriber"}`)); reply.status != http.StatusBadRequest || reply.failure.Error != "ack_beyond_served" {
		t.Fatalf("close beyond served status=%d error=%+v", reply.status, reply.failure)
	}
	h.target("target-a", http.MethodPost, "/close", []byte(`{"state":"dropped","delivered_through":0,"reason":"no_live_subscriber"}`))
	h.target("target-b", http.MethodPost, "/close", []byte(`{"state":"failed","delivered_through":0,"reason":"stream_key_unavailable"}`))
	_, details := h.finish()
	if details.TerminalReason != contracts.RoomStreamEnded || details.Dropped != 2 || reasonOf(details.Targets[0]) != "no_live_subscriber" ||
		details.Targets[1].State != contracts.RoomStreamTargetFailed || reasonOf(details.Targets[1]) != "stream_key_unavailable" {
		t.Fatalf("result %+v", details)
	}
	if status, _, failure := h.postFrame(2, frameData(2, 10)); status != http.StatusGone || failure.Error != "session_ended" {
		t.Fatalf("source after every target closed status=%d error=%+v", status, failure)
	}
}

func TestDirectStreamLeaseWithoutFramesEndsAtTheHandoff(t *testing.T) {
	h := startStream(t, streamOptions{base: 42})
	if reply, _ := h.end(40, "renewed"); reply.status != http.StatusConflict || reply.failure.Error != "frames_missing" {
		t.Fatalf("end before the base status=%d error=%+v", reply.status, reply.failure)
	}
	if reply, receipt := h.end(41, "renewed"); reply.status != http.StatusOK || receipt.BaseSequence != 42 ||
		receipt.AcceptedThrough != 41 || receipt.State != streamEnding {
		t.Fatalf("handoff end status=%d receipt=%+v", reply.status, receipt)
	}
	reply, _ := h.frames("target-a", "after=41")
	if reply.status != http.StatusNoContent || reply.header.Get("X-Beam-Stream-Final-Sequence") != "41" {
		t.Fatalf("target at the handoff status=%d headers=%v", reply.status, reply.header)
	}
	if _, details := h.finish(); details.TerminalReason != contracts.RoomStreamEnded || details.Sequence != 41 || details.Bytes != 0 {
		t.Fatalf("result %+v", details)
	}
}

func TestDirectStreamExpiresAtTheLeaseEnd(t *testing.T) {
	h := startStream(t, streamOptions{expiresIn: 600 * time.Millisecond})
	h.mustPost(1, frameData(1, 10))
	result, details := h.finish()
	if details.TerminalReason != contracts.RoomStreamExpired || result.BytesProcessed != 0 ||
		details.Targets[0].State != contracts.RoomStreamTargetJoining {
		t.Fatalf("result %+v", details)
	}
}

func TestDirectServerSharesOneCertificateForMessagesAndStreams(t *testing.T) {
	server := NewDirectServer(DirectServerConfig{ListenAddress: "127.0.0.1:0", AdvertiseURL: "https://127.0.0.1:{port}"})
	t.Cleanup(func() { _ = server.Close() })
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	stream := startStream(t, streamOptions{server: server})
	now := time.Now().UTC()
	record, _ := json.Marshal(messageRecord{MessageID: "message-1", Ciphertext: []byte("ciphertext")})
	payload, _ := json.Marshal(contracts.RoomWorkerSpec[contracts.MessageUnitDetails]{Schema: contracts.RoomWorkloadSchema,
		Identity: contracts.RoomWorkloadIdentity{WorkloadID: "message-workload", Kind: domain.KindRoomMessage, RoomID: "room-1",
			ChannelID: "channel-1", SourceMemberID: "source", TargetSnapshot: 1, AuthorizationEpoch: 1, PlanEpoch: 1,
			UnitID: "unit-1", Epoch: 1, Attempt: 1, WorkerID: "worker-1", ExpiresAt: now.Add(30 * time.Second)},
		Source: contracts.RoomPathLease{PathID: "source", Role: "source", Protocol: "btr-message",
			Endpoints: []contracts.HTTPEndpoint{{URL: "https://worker.invalid/source"}}, ExpiresAt: now.Add(30 * time.Second)},
		Targets: []contracts.RoomWorkerTarget{{MemberID: "target-a", Path: contracts.RoomPathLease{PathID: "target-a", Role: "target",
			TargetMemberID: "target-a", Protocol: "btr-message", Endpoints: []contracts.HTTPEndpoint{{URL: "https://worker.invalid/a"}},
			ExpiresAt: now.Add(30 * time.Second)}}},
		Details: contracts.MessageUnitDetails{MessageID: "message-1", ContentType: "application/json", SizeBytes: int64(len("ciphertext"))}})
	progress := make(chan string, 4)
	ctx, cancel := context.WithCancel(workloadprogress.WithReporter(context.Background(), func(values map[string]string) {
		progress <- values["room_progress_details"]
	}))
	defer cancel()
	go func() {
		_, _ = NewDirectMessageHandler(server).Execute(ctx, domain.Spec{WorkloadID: "message", AttemptID: "attempt",
			Identity: domain.Identity{WorkerID: "worker-1"}, Kind: domain.KindRoomMessage, Class: domain.ClassJob, Payload: payload})
	}()
	var message contracts.MessageProgressDetails
	select {
	case encoded := <-progress:
		decodeJSON(t, []byte(encoded), &message)
	case <-time.After(5 * time.Second):
		t.Fatal("message runtime was not reported")
	}
	messageURL, _ := url.Parse(message.Runtime.BaseURL)
	streamURL, _ := url.Parse(stream.runtime.BaseURL)
	if message.Runtime.TLSCertificateSHA256 != stream.runtime.TLSCertificateSHA256 || messageURL.Host != streamURL.Host ||
		!strings.HasPrefix(messageURL.Path, "/v1/room-messages/") || !strings.HasPrefix(streamURL.Path, "/v1/room-streams/") {
		t.Fatalf("message %+v and stream %+v do not share the listener", message.Runtime, stream.runtime)
	}
	request, _ := http.NewRequest(http.MethodPost, message.Runtime.BaseURL+"/source", bytes.NewReader(record))
	request.Header.Set("Authorization", "Bearer "+message.Runtime.AccessToken)
	response, err := stream.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("message source over the shared listener status=%d", response.StatusCode)
	}
	if receipt := stream.receipt(); receipt.State != streamOpen {
		t.Fatalf("stream over the shared listener %+v", receipt)
	}
}
