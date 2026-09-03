package bittensor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

const maxResponseBytes = 64 << 10

type Identity struct {
	Hotkey  string `json:"hotkey"`
	Coldkey string `json:"coldkey"`
}

type SignedMessage struct {
	Hotkey    string `json:"hotkey"`
	Message   string `json:"message"`
	Signature string `json:"signature"`
}

type Client struct {
	socketPath string
	timeout    time.Duration
}

func NewClient(socketPath string) (*Client, error) {
	if socketPath == "" {
		return nil, errors.New("Bittensor agent socket path is required")
	}
	return &Client{socketPath: socketPath, timeout: 10 * time.Second}, nil
}

func (c *Client) Identity(ctx context.Context) (Identity, error) {
	var result Identity
	err := c.call(ctx, "get_identity", map[string]string{}, &result)
	return result, err
}

func (c *Client) SignEnrollment(ctx context.Context, workerID, nodePublicKey, nonce string) (SignedMessage, error) {
	var result SignedMessage
	err := c.call(ctx, "sign_enrollment", map[string]string{
		"worker_id": workerID, "node_public_key": nodePublicKey, "nonce": nonce,
	}, &result)
	return result, err
}

func (c *Client) BindNodeKey(ctx context.Context, workerID, orchestratorID, nodePublicKey, nonce string, expiresAt time.Time) (SignedMessage, error) {
	var result SignedMessage
	err := c.call(ctx, "bind_node_key", map[string]string{
		"worker_id": workerID, "orchestrator_id": orchestratorID, "node_public_key": nodePublicKey,
		"nonce": nonce, "expires_at": expiresAt.UTC().Format(time.RFC3339Nano),
	}, &result)
	return result, err
}

func (c *Client) SignPaymentEvidence(ctx context.Context, workerID, taskID, offerID, chunkHash string) (SignedMessage, error) {
	var result SignedMessage
	err := c.call(ctx, "sign_payment_evidence", map[string]string{
		"worker_id": workerID, "task_id": taskID, "offer_id": offerID, "chunk_hash": chunkHash,
	}, &result)
	return result, err
}

type request struct {
	Method string `json:"method"`
	Params any    `json:"params"`
}

type response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

func (c *Client) call(ctx context.Context, method string, params, destination any) error {
	dialer := net.Dialer{Timeout: c.timeout}
	connection, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("connect Bittensor agent: %w", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Now().Add(c.timeout))
	}
	if err := json.NewEncoder(connection).Encode(request{Method: method, Params: params}); err != nil {
		return fmt.Errorf("send Bittensor agent request: %w", err)
	}
	reader := bufio.NewReaderSize(connection, maxResponseBytes)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read Bittensor agent response: %w", err)
	}
	if len(line) > maxResponseBytes {
		return errors.New("Bittensor agent response is too large")
	}
	var envelope response
	if err := json.Unmarshal(line, &envelope); err != nil {
		return fmt.Errorf("decode Bittensor agent response: %w", err)
	}
	if !envelope.OK {
		if envelope.Error == "" {
			envelope.Error = "request rejected"
		}
		return fmt.Errorf("Bittensor agent: %s", envelope.Error)
	}
	if err := json.Unmarshal(envelope.Result, destination); err != nil {
		return fmt.Errorf("decode Bittensor agent result: %w", err)
	}
	return nil
}
