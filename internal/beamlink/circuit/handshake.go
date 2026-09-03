package circuit

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"
)

func NodeID(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return "node_" + base64.RawURLEncoding.EncodeToString(digest[:20])
}

func nodeCertificate(nodeID string, privateKey ed25519.PrivateKey) (tls.Certificate, error) {
	serialBytes := make([]byte, 16)
	if _, err := rand.Read(serialBytes); err != nil {
		return tls.Certificate{}, err
	}
	serial := new(big.Int).SetBytes(serialBytes)
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: nodeID},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey, Leaf: template}, nil
}

func verifyPeerState(state tls.ConnectionState, expectedNodeID string, protocols ...string) error {
	if len(protocols) == 0 {
		protocols = []string{MuxALPN, ALPN}
	}
	validProtocol := false
	for _, protocol := range protocols {
		validProtocol = validProtocol || state.NegotiatedProtocol == protocol
	}
	if state.Version != tls.VersionTLS13 || !validProtocol || len(state.PeerCertificates) != 1 {
		return errors.New("invalid circuit TLS profile")
	}
	certificate := state.PeerCertificates[0]
	publicKey, ok := certificate.PublicKey.(ed25519.PublicKey)
	if !ok || NodeID(publicKey) != expectedNodeID || certificate.Subject.CommonName != expectedNodeID {
		return errors.New("circuit TLS certificate does not match authorized node_id")
	}
	now := time.Now()
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return errors.New("circuit TLS certificate has expired")
	}
	return nil
}

func helloProof(token string, message hello) string {
	return proof(token, strings.Join([]string{"hello", message.CircuitID, fmt.Sprint(message.PlanVersion),
		message.FromNodeID, message.ToNodeID, message.ChannelID, message.Protocol, message.Nonce}, "\x00"))
}

func verifyHelloProof(token string, message hello) bool {
	expected := helloProof(token, message)
	return hmac.Equal([]byte(expected), []byte(message.Proof))
}

func ackProof(token string, message helloAck) string {
	return proof(token, strings.Join([]string{"ack", message.CircuitID, fmt.Sprint(message.PlanVersion),
		message.FromNodeID, message.ToNodeID, message.ChannelID, message.ClientNonce, message.ServerNonce}, "\x00"))
}

func verifyAckProof(token string, message helloAck) bool {
	expected := ackProof(token, message)
	return hmac.Equal([]byte(expected), []byte(message.Proof))
}

func proof(encodedToken, message string) string {
	token, _ := base64.RawURLEncoding.DecodeString(encodedToken)
	mac := hmac.New(sha256.New, token)
	_, _ = mac.Write([]byte(message))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func writeHandshake(writer io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) == 0 || len(encoded) > maxHandshakeBytes {
		return errors.New("invalid circuit handshake size")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(encoded)))
	if err := writeAll(writer, header[:]); err != nil {
		return err
	}
	return writeAll(writer, encoded)
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

func readHandshake(reader io.Reader, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxHandshakeBytes {
		return errors.New("invalid circuit handshake size")
	}
	encoded := make([]byte, size)
	if _, err := io.ReadFull(reader, encoded); err != nil {
		return err
	}
	return json.Unmarshal(encoded, value)
}
