package subsystem

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
)

type circuitTransport struct {
	address string
	token   string
}

func newCircuitTransport(address, token string) (*circuitTransport, error) {
	if strings.TrimSpace(address) == "" {
		return nil, errors.New("isolated distribution subsystem requires the Worker control address")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid Worker control address: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return &circuitTransport{address: net.JoinHostPort(host, port), token: token}, nil
}

func (t *circuitTransport) Dial(ctx context.Context, circuitID, peerNodeID string, request circuit.OpenRequest) (*circuit.Conn, error) {
	return t.DialWithFailover(ctx, circuitID, peerNodeID, nil, request)
}

func (t *circuitTransport) DialWithFailover(ctx context.Context, circuitID, peerNodeID string, standbyNodeIDs []string, request circuit.OpenRequest) (*circuit.Conn, error) {
	metadata, err := json.Marshal(request.Metadata)
	if err != nil {
		return nil, err
	}
	query := url.Values{
		"circuit_id":   []string{circuitID},
		"peer_node_id": []string{peerNodeID},
		"channel_id":   []string{request.ChannelID},
		"protocol":     []string{request.Protocol},
		"metadata":     []string{base64.RawURLEncoding.EncodeToString(metadata)},
	}
	query["standby_node_id"] = append([]string(nil), standbyNodeIDs...)
	return t.open(ctx, "/v1/circuits/dial", query)
}

func (t *circuitTransport) AcceptFor(ctx context.Context, protocol, workloadID string) (*circuit.Conn, error) {
	return t.open(ctx, "/v1/circuits/accept", url.Values{"protocol": []string{protocol}, "workload_id": []string{workloadID}})
}

func (t *circuitTransport) open(ctx context.Context, path string, query url.Values) (*circuit.Conn, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", t.address)
	if err != nil {
		return nil, err
	}
	closed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-closed:
		}
	}()
	request := &http.Request{
		Method: http.MethodPost, URL: &url.URL{Path: path, RawQuery: query.Encode()},
		Host: t.address, Header: make(http.Header),
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "beam-circuit")
	if t.token != "" {
		request.Header.Set("Authorization", "Bearer "+t.token)
	}
	if err := request.Write(connection); err != nil {
		close(closed)
		_ = connection.Close()
		return nil, err
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		close(closed)
		_ = connection.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
		close(closed)
		_ = connection.Close()
		return nil, fmt.Errorf("Worker Circuit bridge returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	encoded := response.Header.Get("X-Beam-Circuit")
	descriptorBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		close(closed)
		_ = connection.Close()
		return nil, errors.New("Worker Circuit bridge returned invalid metadata")
	}
	var descriptor struct {
		CircuitID  string            `json:"circuit_id"`
		WorkloadID string            `json:"workload_id"`
		Peer       circuit.Peer      `json:"peer"`
		ChannelID  string            `json:"channel_id"`
		Protocol   string            `json:"protocol"`
		Metadata   map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(descriptorBytes, &descriptor); err != nil {
		close(closed)
		_ = connection.Close()
		return nil, errors.New("Worker Circuit bridge returned invalid metadata")
	}
	close(closed)
	return &circuit.Conn{
		Conn:      &bufferedCircuitConnection{Conn: connection, reader: reader},
		CircuitID: descriptor.CircuitID, WorkloadID: descriptor.WorkloadID, Peer: descriptor.Peer,
		ChannelID: descriptor.ChannelID, Protocol: descriptor.Protocol, Metadata: descriptor.Metadata,
	}, nil
}

type bufferedCircuitConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedCircuitConnection) Read(value []byte) (int, error) { return c.reader.Read(value) }
