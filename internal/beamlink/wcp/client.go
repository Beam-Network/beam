package wcp

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/evidence"
	"github.com/Beam-Network/beam/internal/resources"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

type ClientConfig struct {
	Address                string
	TLSConfig              *tls.Config
	PrivateKey             ed25519.PrivateKey
	Identity               domain.Identity
	OrchestratorDelegation []byte
	SoftwareVersion        string
	Region                 string
	Capabilities           []string
	TotalResources         domain.Resources
	Engine                 *runtime.Engine
	Governor               *resources.Governor
	Store                  runtime.Store
	ReconnectMinimum       time.Duration
	ReconnectMaximum       time.Duration
	CircuitEndpoint        string
	Circuits               circuit.Controller
	Receipts               *evidence.Recorder
}

type Client struct {
	config      ClientConfig
	planVersion atomic.Uint64
	eventCursor atomic.Uint64
	draining    atomic.Bool
}

func NewClient(config ClientConfig) (*Client, error) {
	if config.Address == "" || config.TLSConfig == nil || len(config.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("WCP address, TLS config, and Ed25519 node key are required")
	}
	if config.Engine == nil || config.Governor == nil || config.Store == nil {
		return nil, errors.New("WCP Engine, Governor, and Store are required")
	}
	if err := config.Identity.Validate(); err != nil {
		return nil, err
	}
	derivedNodeID := NodeID(config.PrivateKey.Public().(ed25519.PublicKey))
	if config.Identity.NodeID != derivedNodeID {
		return nil, fmt.Errorf("configured node_id %q does not match node key %q", config.Identity.NodeID, derivedNodeID)
	}
	if config.Identity.OrchestratorID == "" {
		return nil, errors.New("orchestrator_id is required for WCP")
	}
	if config.ReconnectMinimum <= 0 {
		config.ReconnectMinimum = 500 * time.Millisecond
	}
	if config.ReconnectMaximum < config.ReconnectMinimum {
		config.ReconnectMaximum = 30 * time.Second
	}
	config.TLSConfig = config.TLSConfig.Clone()
	config.TLSConfig.MinVersion = tls.VersionTLS13
	config.TLSConfig.NextProtos = []string{ALPN}
	return &Client{config: config}, nil
}

func (c *Client) Run(ctx context.Context) error {
	backoff := c.config.ReconnectMinimum
	for {
		err := c.runSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			backoff = c.config.ReconnectMinimum
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff = min(backoff*2, c.config.ReconnectMaximum)
	}
}

func (c *Client) runSession(ctx context.Context) error {
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: c.config.TLSConfig.Clone()}
	rawConnection, err := dialer.DialContext(ctx, "tcp", c.config.Address)
	if err != nil {
		return err
	}
	connection := rawConnection.(*tls.Conn)
	defer connection.Close()
	if state := connection.ConnectionState(); state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != ALPN {
		return errors.New("Orchestrator did not negotiate the BeamLink WCP profile")
	}
	framed := newFramedConn(connection)
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	challengeEnvelope, err := framed.read()
	if err != nil || challengeEnvelope.Type != TypeChallenge {
		return errors.New("Orchestrator did not send a WCP challenge")
	}
	challenge, err := decodePayload[Challenge](challengeEnvelope)
	if err != nil {
		return err
	}
	if challenge.OrchestratorID != c.config.Identity.OrchestratorID || time.Since(challenge.SentAt) > 30*time.Second {
		return errors.New("invalid or stale Orchestrator challenge")
	}
	publicKey := c.config.PrivateKey.Public().(ed25519.PublicKey)
	snapshot := c.config.Governor.Snapshot()
	capabilityManifest := c.capabilityManifest(c.config.TotalResources, snapshot.Available)
	helloEnvelope, err := framed.write(TypeHello, challengeEnvelope.MessageID, Hello{
		Identity: c.config.Identity, PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		Signature:              SignHello(c.config.PrivateKey, challenge, c.config.Identity),
		OrchestratorDelegation: c.config.OrchestratorDelegation, SoftwareVersion: c.config.SoftwareVersion,
		Capabilities: capabilityManifest.Capabilities, CapabilityManifest: &capabilityManifest, TotalResources: c.config.TotalResources,
		LastPlanVersion: c.planVersion.Load(), LastEventCursor: c.eventCursor.Load(),
		CircuitEndpoint: c.config.CircuitEndpoint,
	}, challengeEnvelope.Sequence)
	if err != nil {
		return err
	}
	welcomeEnvelope, err := framed.read()
	if err != nil || welcomeEnvelope.Type != TypeWelcome || welcomeEnvelope.ReplyTo != helloEnvelope.MessageID {
		return errors.New("Orchestrator rejected WCP hello")
	}
	welcome, err := decodePayload[Welcome](welcomeEnvelope)
	if err != nil {
		return err
	}
	if welcome.OrchestratorID != c.config.Identity.OrchestratorID || welcome.HeartbeatInterval <= 0 {
		return errors.New("invalid Orchestrator welcome")
	}
	c.planVersion.Store(max(c.planVersion.Load(), welcome.PlanVersion))
	_ = connection.SetReadDeadline(time.Time{})

	sessionContext, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	readError := make(chan error, 1)
	go func() { readError <- c.readLoop(sessionContext, framed, welcomeEnvelope.Sequence) }()
	heartbeatError := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(welcome.HeartbeatInterval)
		defer ticker.Stop()
		for {
			if err := c.sendHeartbeat(framed); err != nil {
				heartbeatError <- err
				return
			}
			select {
			case <-sessionContext.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	sentResults := make(map[string]struct{})
	sentProgress := make(map[string]struct{})
	sentCheckpoints := make(map[string]struct{})
	sentReceipts := make(map[string]time.Time)
	if err := c.replayResults(framed, sentResults); err != nil {
		return err
	}
	if err := c.replayCheckpoints(framed, sentCheckpoints); err != nil {
		return err
	}
	if err := c.replayReceipts(framed, sentReceipts); err != nil {
		return err
	}
	resultTicker := time.NewTicker(min(welcome.HeartbeatInterval, 250*time.Millisecond))
	defer resultTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readError:
			return err
		case err := <-heartbeatError:
			return err
		case <-resultTicker.C:
			if err := c.replayCheckpoints(framed, sentCheckpoints); err != nil {
				return err
			}
			if err := c.replayProgress(framed, sentProgress); err != nil {
				return err
			}
			if err := c.replayResults(framed, sentResults); err != nil {
				return err
			}
			if err := c.replayReceipts(framed, sentReceipts); err != nil {
				return err
			}
		}
	}
}

func (c *Client) replayCheckpoints(framed *framedConn, sent map[string]struct{}) error {
	for _, record := range c.config.Store.List() {
		if record.Checkpoint == nil {
			continue
		}
		key := fmt.Sprintf("%s/%d", record.Spec.Key(), record.Checkpoint.Sequence)
		if _, exists := sent[key]; exists {
			continue
		}
		if _, err := framed.write(TypeCheckpoint, "", *record.Checkpoint, c.eventCursor.Load()); err != nil {
			return err
		}
		sent[key] = struct{}{}
	}
	return nil
}

func (c *Client) replayProgress(framed *framedConn, sent map[string]struct{}) error {
	for _, record := range c.config.Store.List() {
		if record.Progress == nil {
			continue
		}
		key := record.Spec.Key() + "/" + record.Progress.ObservedAt.UTC().Format(time.RFC3339Nano)
		if _, exists := sent[key]; exists {
			continue
		}
		if _, err := framed.write(TypeProgress, "", *record.Progress, c.eventCursor.Load()); err != nil {
			return err
		}
		sent[key] = struct{}{}
	}
	return nil
}

func (c *Client) readLoop(ctx context.Context, framed *framedConn, initialSequence uint64) error {
	lastSequence := initialSequence
	for {
		envelope, err := framed.read()
		if err != nil {
			return err
		}
		if envelope.Sequence <= lastSequence {
			return errors.New("Orchestrator WCP sequence rolled back")
		}
		lastSequence = envelope.Sequence
		c.eventCursor.Store(envelope.Sequence)
		switch envelope.Type {
		case TypeOffer:
			spec, err := decodePayload[domain.Spec](envelope)
			if err != nil {
				return err
			}
			var decision runtime.Decision
			if c.draining.Load() {
				decision = runtime.Decision{WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID, Reason: "Worker is draining"}
			} else {
				decision, _ = c.config.Engine.Offer(ctx, spec)
			}
			if _, err := framed.write(TypeDecision, envelope.MessageID, decision, lastSequence); err != nil {
				return err
			}
		case TypeCommit:
			commit, err := decodePayload[domain.Commit](envelope)
			if err != nil {
				return err
			}
			if err := c.config.Engine.Commit(ctx, commit); err != nil {
				_, _ = framed.write(TypeError, envelope.MessageID, ErrorMessage{Code: "commit_rejected", Message: err.Error()}, lastSequence)
				continue
			}
			c.planVersion.Store(max(c.planVersion.Load(), commit.PlanVersion))
		case TypeCancel:
			cancel, err := decodePayload[Cancel](envelope)
			if err != nil {
				return err
			}
			if err := c.config.Engine.Cancel(cancel.WorkloadID + "/" + cancel.AttemptID); err != nil {
				_, _ = framed.write(TypeError, envelope.MessageID, ErrorMessage{Code: "cancel_rejected", Message: err.Error()}, lastSequence)
			}
		case TypeDrain:
			c.draining.Store(true)
		case TypeReceiptAck:
			acknowledgement, err := decodePayload[ReceiptAck](envelope)
			if err != nil {
				return err
			}
			if acknowledgement.ReceiptID == "" || acknowledgement.AcknowledgedAt.IsZero() {
				return errors.New("invalid Orchestrator receipt acknowledgement")
			}
			if c.config.Receipts == nil {
				return errors.New("Orchestrator acknowledged a receipt while receipt recording is disabled")
			}
			if err := c.config.Receipts.Acknowledge(acknowledgement.ReceiptID, acknowledgement.AcknowledgedAt); err != nil {
				return err
			}
		case TypeCircuitUpsert:
			plan, err := decodePayload[circuit.Plan](envelope)
			if err != nil {
				return err
			}
			if c.config.Circuits == nil {
				_, _ = framed.write(TypeError, envelope.MessageID, ErrorMessage{Code: "circuits_disabled", Message: "Worker circuit service is disabled"}, lastSequence)
				continue
			}
			if err := c.config.Circuits.Upsert(plan); err != nil {
				_, _ = framed.write(TypeError, envelope.MessageID, ErrorMessage{Code: "circuit_rejected", Message: err.Error()}, lastSequence)
				continue
			}
			if c.config.Receipts != nil {
				observedAt := time.Now().UTC()
				if observedAt.Before(plan.IssuedAt) {
					observedAt = plan.IssuedAt
				}
				if _, err := c.config.Receipts.RecordCircuit(plan, "authorized", observedAt, ""); err != nil {
					return err
				}
			}
		case TypeCircuitRevoke:
			revocation, err := decodePayload[circuit.Revocation](envelope)
			if err != nil {
				return err
			}
			if c.config.Circuits != nil {
				if err := c.config.Circuits.Revoke(revocation); err != nil {
					_, _ = framed.write(TypeError, envelope.MessageID, ErrorMessage{Code: "revoke_rejected", Message: err.Error()}, lastSequence)
					continue
				}
				if c.config.Receipts != nil {
					plan := circuit.Plan{CircuitID: revocation.CircuitID, WorkloadID: revocation.WorkloadID,
						PlanVersion: revocation.PlanVersion, IssuedAt: revocation.RevokedAt, ExpiresAt: revocation.RevokedAt}
					if _, err := c.config.Receipts.RecordCircuit(plan, "revoked", revocation.RevokedAt, revocation.Reason); err != nil {
						return err
					}
				}
			}
		case TypeError:
			continue
		default:
			return fmt.Errorf("unsupported Orchestrator message type %q", envelope.Type)
		}
	}
}

func (c *Client) sendHeartbeat(framed *framedConn) error {
	snapshot := c.config.Governor.Snapshot()
	status := "active"
	if c.draining.Load() {
		status = "draining"
	}
	capabilityManifest := c.capabilityManifest(c.config.TotalResources, snapshot.Available)
	_, err := framed.write(TypeHeartbeat, "", Heartbeat{
		Identity: c.config.Identity, Status: status, Region: c.config.Region,
		Capabilities: capabilityManifest.Capabilities, CapabilityManifest: &capabilityManifest, Total: c.config.TotalResources,
		Available: snapshot.Available, PlanVersion: c.planVersion.Load(), EventCursor: c.eventCursor.Load(),
		CircuitEndpoint: c.config.CircuitEndpoint,
	}, c.eventCursor.Load())
	return err
}

func (c *Client) capabilityManifest(total, available domain.Resources) contracts.CapabilityManifest {
	maxConnections := total.Connections
	availableConnections := available.Connections
	if maxConnections <= 0 && len(c.config.Capabilities) > 0 {
		maxConnections = 1
	}
	return contracts.NewWorkerCapabilityManifest(
		c.config.Identity.WorkerID,
		c.config.SoftwareVersion,
		c.config.Capabilities,
		maxConnections,
		availableConnections,
		time.Now().UTC(),
	)
}

func (c *Client) replayResults(framed *framedConn, sent map[string]struct{}) error {
	for _, record := range c.config.Store.List() {
		if record.Result != nil {
			key := record.Spec.Key() + "/" + string(record.Result.State) + "/" + record.Result.CompletedAt.UTC().Format(time.RFC3339Nano)
			if _, exists := sent[key]; exists {
				continue
			}
			if _, err := framed.write(TypeResult, "", *record.Result, c.eventCursor.Load()); err != nil {
				return err
			}
			sent[key] = struct{}{}
		}
	}
	return nil
}

func (c *Client) replayReceipts(framed *framedConn, sent map[string]time.Time) error {
	if c.config.Receipts == nil {
		return nil
	}
	now := time.Now()
	for _, receipt := range c.config.Receipts.Pending() {
		if last, exists := sent[receipt.ReceiptID]; exists && now.Sub(last) < 2*time.Second {
			continue
		}
		if _, err := framed.write(TypeReceipt, "", receipt, c.eventCursor.Load()); err != nil {
			return err
		}
		sent[receipt.ReceiptID] = now
	}
	return nil
}
