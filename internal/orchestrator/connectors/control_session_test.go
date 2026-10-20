package connectors

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadruntime "github.com/Beam-Network/beam/internal/workload/runtime"
	"github.com/nats-io/nats.go"
	"github.com/vmihailenco/msgpack/v5"
)

type fakeControlSubscription struct {
	unsubscribed bool
}

func (subscription *fakeControlSubscription) Unsubscribe() error {
	subscription.unsubscribed = true
	return nil
}

type fakeRoomControlConnection struct {
	subscriptions    []*fakeControlSubscription
	subjects         []string
	flushErr         error
	flushHasDeadline bool
	requests         []fakeControlRequest
	reply            func(string) map[string]any
	publishedMu      sync.Mutex
	published        []fakeControlRequest
}

type fakeControlRequest struct {
	subject string
	payload map[string]any
}

func (connection *fakeRoomControlConnection) publish(subject string, payload []byte) error {
	var message map[string]any
	if err := msgpack.Unmarshal(payload, &message); err != nil {
		return err
	}
	connection.publishedMu.Lock()
	defer connection.publishedMu.Unlock()
	connection.published = append(connection.published, fakeControlRequest{subject: subject, payload: message})
	return nil
}

func (connection *fakeRoomControlConnection) publishedMessages() []fakeControlRequest {
	connection.publishedMu.Lock()
	defer connection.publishedMu.Unlock()
	return append([]fakeControlRequest(nil), connection.published...)
}

func (connection *fakeRoomControlConnection) chanSubscribe(subject string, _ chan *nats.Msg) (roomControlSubscription, error) {
	subscription := &fakeControlSubscription{}
	connection.subscriptions = append(connection.subscriptions, subscription)
	connection.subjects = append(connection.subjects, subject)
	return subscription, nil
}

func (connection *fakeRoomControlConnection) flushWithContext(ctx context.Context) error {
	_, connection.flushHasDeadline = ctx.Deadline()
	return connection.flushErr
}

func (connection *fakeRoomControlConnection) requestWithContext(_ context.Context, subject string, payload []byte) (*nats.Msg, error) {
	var request map[string]any
	if err := msgpack.Unmarshal(payload, &request); err != nil {
		return nil, err
	}
	connection.requests = append(connection.requests, fakeControlRequest{subject: subject, payload: request})
	messageType, _ := request["type"].(string)
	response := map[string]any{"type": controlReplyType(messageType)}
	if messageType == "capability_update" {
		response["accepted"] = true
	}
	if connection.reply != nil {
		response = connection.reply(messageType)
	}
	encoded, err := msgpack.Marshal(response)
	if err != nil {
		return nil, err
	}
	return &nats.Msg{Data: encoded}, nil
}

type fakeControlWCP struct{}

func (fakeControlWCP) Offer(context.Context, string, domain.Spec) (workloadruntime.Decision, error) {
	return workloadruntime.Decision{Accepted: true}, nil
}
func (fakeControlWCP) Commit(context.Context, string, domain.Commit) error     { return nil }
func (fakeControlWCP) Cancel(context.Context, string, string, string) error    { return nil }
func (fakeControlWCP) Connected(string) bool                                   { return true }
func (fakeControlWCP) UpsertCircuit(context.Context, circuit.Plan) error       { return nil }
func (fakeControlWCP) RevokeCircuit(context.Context, circuit.Revocation) error { return nil }

func TestRoomResultReplayFinishesOnlyConfirmedOrPermanentlyMissingTransfers(t *testing.T) {
	for _, test := range []struct {
		received  bool
		reason    string
		wantError bool
	}{
		{true, "", false}, {false, "transfer_not_found", false},
		{false, "verification_pending", true}, {false, "", true},
		{false, "stale_attempt", true}, {false, "invalid_room_task_result_payload", true},
	} {
		connection := &fakeRoomControlConnection{reply: func(string) map[string]any {
			return map[string]any{"type": "room_task_result_ack", "received": test.received, "reason": test.reason}
		}}
		control := newRoomControl(NATSConfig{Environment: "dev", Hotkey: "hotkey-1"}, nil, nil, nil, nil)
		control.conn = connection
		err := control.request(context.Background(), "room_task_result", map[string]any{})
		if (err != nil) != test.wantError {
			t.Fatalf("received=%v reason=%q error=%v", test.received, test.reason, err)
		}
		var terminal *dispatch.TerminalDeliveryError
		wantTerminal := test.reason == "stale_attempt" || test.reason == "invalid_room_task_result_payload"
		if errors.As(err, &terminal) != wantTerminal {
			t.Fatalf("reason=%q terminal disposition=%v, want %v", test.reason, errors.As(err, &terminal), wantTerminal)
		}
	}
}

func TestBeamCoreConnectorRejectsConcurrentRun(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()

	connector, err := NewBeamCoreConnector(
		NATSConfig{URL: "nats://" + listener.Addr().String(), Name: "single-owner-test"},
		&dispatch.Service{},
	)
	if err != nil {
		t.Fatal(err)
	}
	firstResult := make(chan error, 1)
	go func() { firstResult <- connector.Run(context.Background()) }()

	var connection net.Conn
	select {
	case connection = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("first Run did not start its NATS connection")
	}
	if err := connector.Run(context.Background()); err == nil || err.Error() != "BeamCore control session is already running" {
		t.Fatalf("second Run error=%v", err)
	}
	_ = connection.Close()
	select {
	case <-firstResult:
	case <-time.After(3 * time.Second):
		t.Fatal("first Run did not release after its candidate connection closed")
	}
}

func TestRoomControlBindCleansPartialSubscriptionOnFlushFailure(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	orchestratorRegistry, err := registry.New(orchestratordomain.Orchestrator{OrchestratorID: "orch-1", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := dispatch.NewService(
		dispatch.Config{OrchestratorID: "orch-1", Now: func() time.Time { return now }},
		orchestratorRegistry,
		fakeControlWCP{},
		dispatch.NewMemoryStore(),
	)
	if err != nil {
		t.Fatal(err)
	}
	flushErr := errors.New("flush failed")
	connection := &fakeRoomControlConnection{flushErr: flushErr}
	control := &roomControl{
		config: NATSConfig{Environment: "dev", Hotkey: "hotkey-1", GatewayURL: "http://gateway.test", RequestTimeout: time.Second},
		conn:   connection,
		tasks:  tasks,
	}

	if _, err := control.bind(context.Background()); !errors.Is(err, flushErr) {
		t.Fatalf("bind error=%v", err)
	}
	if len(connection.subscriptions) != 2 {
		t.Fatalf("expected offer and cancellation subscriptions, got %d", len(connection.subscriptions))
	}
	for index, subscription := range connection.subscriptions {
		if !subscription.unsubscribed {
			t.Fatalf("partial subscription %d was not closed", index)
		}
	}
	if !connection.flushHasDeadline {
		t.Fatal("control subscription flush did not receive a deadline")
	}
}
