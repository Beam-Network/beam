package wcp

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

const (
	ProtocolVersion = 1
	ALPN            = "beam-wcp/1"
	maxFrameBytes   = 1 << 20
)

const (
	TypeChallenge     = "challenge"
	TypeHello         = "hello"
	TypeWelcome       = "welcome"
	TypeHeartbeat     = "heartbeat"
	TypeOffer         = "workload.offer"
	TypeDecision      = "workload.decision"
	TypeCommit        = "workload.commit"
	TypeCancel        = "workload.cancel"
	TypeResult        = "workload.result"
	TypeProgress      = "workload.progress"
	TypeCheckpoint    = "workload.checkpoint"
	TypeReceipt       = "evidence.receipt"
	TypeReceiptAck    = "evidence.receipt_ack"
	TypeCircuitUpsert = "circuit.upsert"
	TypeCircuitRevoke = "circuit.revoke"
	TypeDrain         = "worker.drain"
	TypeError         = "error"
)

type Envelope struct {
	Version   uint32          `json:"version"`
	MessageID string          `json:"message_id"`
	ReplyTo   string          `json:"reply_to,omitempty"`
	Sequence  uint64          `json:"sequence"`
	Ack       uint64          `json:"ack,omitempty"`
	SentAt    time.Time       `json:"sent_at"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type Challenge struct {
	OrchestratorID string    `json:"orchestrator_id"`
	Nonce          string    `json:"nonce"`
	SentAt         time.Time `json:"sent_at"`
}

type Hello struct {
	Identity               domain.Identity               `json:"identity"`
	PublicKey              string                        `json:"public_key"`
	Signature              string                        `json:"signature"`
	OrchestratorDelegation []byte                        `json:"orchestrator_delegation,omitempty"`
	SoftwareVersion        string                        `json:"software_version"`
	Capabilities           []string                      `json:"capabilities"`
	CapabilityManifest     *contracts.CapabilityManifest `json:"capability_manifest,omitempty"`
	TotalResources         domain.Resources              `json:"total_resources"`
	LastPlanVersion        uint64                        `json:"last_plan_version"`
	LastEventCursor        uint64                        `json:"last_event_cursor"`
	CircuitEndpoint        string                        `json:"circuit_endpoint,omitempty"`
}

type Welcome struct {
	OrchestratorID    string        `json:"orchestrator_id"`
	SessionID         string        `json:"session_id"`
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`
	ConfigEpoch       uint64        `json:"config_epoch"`
	PlanVersion       uint64        `json:"plan_version"`
}

type Heartbeat struct {
	Identity           domain.Identity               `json:"identity"`
	Status             string                        `json:"status"`
	Region             string                        `json:"region,omitempty"`
	Capabilities       []string                      `json:"capabilities"`
	CapabilityManifest *contracts.CapabilityManifest `json:"capability_manifest,omitempty"`
	Total              domain.Resources              `json:"total"`
	Available          domain.Resources              `json:"available"`
	PlanVersion        uint64                        `json:"plan_version"`
	EventCursor        uint64                        `json:"event_cursor"`
	CircuitEndpoint    string                        `json:"circuit_endpoint,omitempty"`
}

type Cancel struct {
	WorkloadID string `json:"workload_id"`
	AttemptID  string `json:"attempt_id"`
}

type ErrorMessage struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ReceiptAck struct {
	ReceiptID      string    `json:"receipt_id"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}

type framedConn struct {
	reader io.Reader
	writer io.Writer
	mu     sync.Mutex
	seq    uint64
}

func newFramedConn(connection io.ReadWriter) *framedConn {
	return &framedConn{reader: connection, writer: connection}
}

func (c *framedConn) write(messageType, replyTo string, value any, ack uint64) (Envelope, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return Envelope{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	envelope := Envelope{
		Version: ProtocolVersion, MessageID: randomID("wcp"), ReplyTo: replyTo,
		Sequence: c.seq, Ack: ack, SentAt: time.Now().UTC(), Type: messageType, Payload: payload,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return Envelope{}, err
	}
	if len(encoded) > maxFrameBytes {
		return Envelope{}, errors.New("WCP frame is too large")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(encoded)))
	if _, err := c.writer.Write(header[:]); err != nil {
		return Envelope{}, err
	}
	if _, err := c.writer.Write(encoded); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

func (c *framedConn) read() (Envelope, error) {
	var header [4]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return Envelope{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxFrameBytes {
		return Envelope{}, errors.New("invalid WCP frame size")
	}
	encoded := make([]byte, size)
	if _, err := io.ReadFull(c.reader, encoded); err != nil {
		return Envelope{}, err
	}
	var envelope Envelope
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return Envelope{}, err
	}
	if envelope.Version != ProtocolVersion || envelope.MessageID == "" || envelope.Type == "" {
		return Envelope{}, errors.New("invalid WCP envelope")
	}
	return envelope, nil
}

func decodePayload[T any](envelope Envelope) (T, error) {
	var result T
	if err := json.Unmarshal(envelope.Payload, &result); err != nil {
		return result, fmt.Errorf("decode %s payload: %w", envelope.Type, err)
	}
	return result, nil
}

func NodeID(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return "node_" + base64.RawURLEncoding.EncodeToString(digest[:20])
}

func SignHello(privateKey ed25519.PrivateKey, challenge Challenge, identity domain.Identity) string {
	signature := ed25519.Sign(privateKey, helloMessage(challenge, identity, privateKey.Public().(ed25519.PublicKey)))
	return base64.RawURLEncoding.EncodeToString(signature)
}

func VerifyHello(hello Hello, challenge Challenge) error {
	publicKey, err := base64.RawURLEncoding.DecodeString(hello.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("invalid Worker public key")
	}
	if hello.Identity.NodeID != NodeID(ed25519.PublicKey(publicKey)) {
		return errors.New("node_id does not match Worker public key")
	}
	signature, err := base64.RawURLEncoding.DecodeString(hello.Signature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), helloMessage(challenge, hello.Identity, ed25519.PublicKey(publicKey)), signature) {
		return errors.New("invalid Worker challenge signature")
	}
	return nil
}

func helloMessage(challenge Challenge, identity domain.Identity, publicKey ed25519.PublicKey) []byte {
	return []byte(fmt.Sprintf("beam:wcp:v1:%s:%s:%s:%s:%s", challenge.OrchestratorID, challenge.Nonce,
		identity.WorkerID, identity.NodeID, base64.RawURLEncoding.EncodeToString(publicKey)))
}

func NewChallenge(orchestratorID string) Challenge {
	bytes := make([]byte, 32)
	_, _ = rand.Read(bytes)
	return Challenge{OrchestratorID: orchestratorID, Nonce: base64.RawURLEncoding.EncodeToString(bytes), SentAt: time.Now().UTC()}
}

func LoadOrCreateNodeKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("node key path is required")
	}
	encoded, err := os.ReadFile(path)
	if err == nil {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(string(encoded))
		if decodeErr != nil || len(raw) != ed25519.PrivateKeySize {
			return nil, errors.New("invalid persisted node key")
		}
		return ed25519.PrivateKey(raw), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".node-key-*.tmp")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return nil, err
	}
	if _, err := temporary.WriteString(base64.RawURLEncoding.EncodeToString(privateKey)); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return nil, err
	}
	return privateKey, nil
}

func randomID(prefix string) string {
	bytes := make([]byte, 16)
	_, _ = rand.Read(bytes)
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(bytes)
}
