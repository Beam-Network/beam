// Package btr maps Beam Tunnel Rooms wire streams onto authenticated,
// multiplexed Worker Circuits. Beam Tunnel remains authoritative for BTR
// assignments, encryption, frame validation, persistence, and delivery state.
package btr

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
)

// These protocol selectors deliberately match beam-tunnel's frozen BTR
// capabilities. They are logical Yamux stream protocols, not new BTR formats.
const (
	MessageProtocol = "btr.message.v1"
	StreamProtocol  = "btr.stream.v1"

	WireVersionV1          uint32 = 1
	MaxTokenBytes                 = 256 << 10
	MaxWirePayloadBytes           = 1 << 20
	MaxEnvelopeHeaderBytes        = 16 << 10
	MaxWireFrameBytes             = (MaxWirePayloadBytes+2)/3*4 + MaxEnvelopeHeaderBytes + MaxTokenBytes
)

type Route struct {
	CircuitID      string
	PrimaryNodeID  string
	StandbyNodeIDs []string
}

type Dialer interface {
	DialWithFailover(context.Context, string, string, []string, circuit.OpenRequest) (*circuit.Conn, error)
}

type Acceptor interface {
	AcceptFor(context.Context, string, string) (*circuit.Conn, error)
}

type Client struct{ Dialer Dialer }

// Stream carries the exact length-delimited JSON WireFrame produced and
// consumed by beam-tunnel/pkg/btr. It does not decrypt or reinterpret payloads.
type Stream struct{ *circuit.Conn }

func (c Client) Open(ctx context.Context, route Route, protocol string, metadata map[string]string) (*Stream, error) {
	if c.Dialer == nil {
		return nil, errors.New("BTR transport requires a circuit dialer")
	}
	if !SupportedProtocol(protocol) {
		return nil, errors.New("unsupported BTR circuit protocol")
	}
	connection, err := c.Dialer.DialWithFailover(ctx, route.CircuitID, route.PrimaryNodeID, route.StandbyNodeIDs,
		circuit.OpenRequest{Protocol: protocol, Metadata: metadata})
	if err != nil {
		return nil, err
	}
	return &Stream{Conn: connection}, nil
}

func Accept(ctx context.Context, acceptor Acceptor, protocol, workloadID string) (*Stream, error) {
	if acceptor == nil || !SupportedProtocol(protocol) {
		return nil, errors.New("BTR transport requires an acceptor and supported protocol")
	}
	connection, err := acceptor.AcceptFor(ctx, protocol, workloadID)
	if err != nil {
		return nil, err
	}
	return &Stream{Conn: connection}, nil
}

func SupportedProtocol(protocol string) bool {
	return protocol == MessageProtocol || protocol == StreamProtocol
}

// WriteWireFrame writes an already encoded canonical BTR WireFrame without
// changing it. The bounded structural check catches transport-level corruption;
// full semantic and cryptographic validation remains in beam-tunnel.
func (s *Stream) WriteWireFrame(frame json.RawMessage) error {
	if s == nil || s.Conn == nil {
		return net.ErrClosed
	}
	if err := validateWireFrame(frame); err != nil {
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(frame)))
	if err := writeAll(s.Conn, header[:]); err != nil {
		return err
	}
	return writeAll(s.Conn, frame)
}

func (s *Stream) ReadWireFrame() (json.RawMessage, error) {
	if s == nil || s.Conn == nil {
		return nil, net.ErrClosed
	}
	var header [4]byte
	if _, err := io.ReadFull(s.Conn, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || uint64(size) > uint64(MaxWireFrameBytes) {
		return nil, errors.New("invalid BTR wire frame size")
	}
	frame := make(json.RawMessage, size)
	if _, err := io.ReadFull(s.Conn, frame); err != nil {
		return nil, err
	}
	if err := validateWireFrame(frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func validateWireFrame(frame json.RawMessage) error {
	if len(frame) == 0 || len(frame) > MaxWireFrameBytes || !json.Valid(frame) {
		return errors.New("invalid BTR wire frame")
	}
	var value struct {
		WireVersion     uint32 `json:"wire_version"`
		AssignmentToken string `json:"assignment_token,omitempty"`
		Envelope        struct {
			WireVersion   uint32          `json:"wire_version"`
			FrameType     string          `json:"frame_type"`
			PayloadLength uint64          `json:"payload_length"`
			Ciphertext    json.RawMessage `json:"ciphertext,omitempty"`
		} `json:"envelope"`
	}
	if err := json.Unmarshal(frame, &value); err != nil || value.WireVersion != WireVersionV1 ||
		value.Envelope.WireVersion != WireVersionV1 || value.Envelope.FrameType == "" || len(value.AssignmentToken) > MaxTokenBytes {
		return errors.New("invalid BTR wire frame envelope")
	}
	// Ciphertext is base64 in JSON. Its definitive decoded length check remains
	// in beam-tunnel's canonical Envelope.Validate implementation.
	return nil
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}
