package connectors

import (
	"context"
	"errors"
	"net"
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
	flushErr         error
	flushHasDeadline bool
}

func (connection *fakeRoomControlConnection) chanSubscribe(_ string, _ chan *nats.Msg) (roomControlSubscription, error) {
	subscription := &fakeControlSubscription{}
	connection.subscriptions = append(connection.subscriptions, subscription)
	return subscription, nil
}

func (connection *fakeRoomControlConnection) flushWithContext(ctx context.Context) error {
	_, connection.flushHasDeadline = ctx.Deadline()
	return connection.flushErr
}

func (connection *fakeRoomControlConnection) requestWithContext(_ context.Context, _ string, payload []byte) (*nats.Msg, error) {
	var request orchestratorControlEnvelope
	if err := msgpack.Unmarshal(payload, &request); err != nil {
		return nil, err
	}
	responsePayload := map[string]any{}
	if request.MessageType == "capability_update" {
		responsePayload["accepted"] = true
	}
	response, err := msgpack.Marshal(orchestratorControlEnvelope{
		RequestID:   request.RequestID,
		Producer:    "transfer-runtime",
		MessageType: request.MessageType + "_ack",
		Payload:     responsePayload,
	})
	if err != nil {
		return nil, err
	}
	return &nats.Msg{Data: response}, nil
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
	if len(connection.subscriptions) != 1 || !connection.subscriptions[0].unsubscribed {
		t.Fatalf("partial subscriptions were not closed: %+v", connection.subscriptions)
	}
	if !connection.flushHasDeadline {
		t.Fatal("control subscription flush did not receive a deadline")
	}
}
