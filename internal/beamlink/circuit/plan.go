package circuit

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

const (
	ALPN    = "beam-circuit/1"
	MuxALPN = "beam-circuit-mux/1"
)

type Peer struct {
	WorkerID         string   `json:"worker_id"`
	NodeID           string   `json:"node_id"`
	Endpoint         string   `json:"endpoint"`
	StandbyEndpoints []string `json:"standby_endpoints,omitempty"`
	Roles            []string `json:"roles,omitempty"`
	Capabilities     []string `json:"capabilities,omitempty"`
}

type Plan struct {
	CircuitID   string    `json:"circuit_id"`
	WorkloadID  string    `json:"workload_id"`
	PlanVersion uint64    `json:"plan_version"`
	Token       string    `json:"token"`
	Peers       []Peer    `json:"peers"`
	ExpiresAt   time.Time `json:"expires_at"`
	IssuedAt    time.Time `json:"issued_at"`
}

type Revocation struct {
	CircuitID   string    `json:"circuit_id"`
	WorkloadID  string    `json:"workload_id,omitempty"`
	PlanVersion uint64    `json:"plan_version"`
	Reason      string    `json:"reason,omitempty"`
	RevokedAt   time.Time `json:"revoked_at"`
}

func (p Plan) Validate(now time.Time) error {
	if strings.TrimSpace(p.CircuitID) == "" || strings.TrimSpace(p.WorkloadID) == "" {
		return errors.New("circuit_id and workload_id are required")
	}
	if p.PlanVersion == 0 {
		return errors.New("circuit plan_version must be positive")
	}
	token, err := base64.RawURLEncoding.DecodeString(p.Token)
	if err != nil || len(token) < 32 {
		return errors.New("circuit token must contain at least 256 bits")
	}
	if p.ExpiresAt.IsZero() || !now.Before(p.ExpiresAt) {
		return errors.New("circuit plan has expired")
	}
	if len(p.Peers) < 2 {
		return errors.New("circuit requires at least two peers")
	}
	workers := make(map[string]struct{}, len(p.Peers))
	nodes := make(map[string]struct{}, len(p.Peers))
	for _, peer := range p.Peers {
		if peer.WorkerID == "" || peer.NodeID == "" || peer.Endpoint == "" {
			return errors.New("circuit peers require worker_id, node_id, and endpoint")
		}
		if _, _, err := net.SplitHostPort(peer.Endpoint); err != nil {
			return fmt.Errorf("invalid circuit endpoint for %s", peer.NodeID)
		}
		endpoints := map[string]struct{}{peer.Endpoint: {}}
		for _, endpoint := range peer.StandbyEndpoints {
			if _, _, err := net.SplitHostPort(endpoint); err != nil {
				return fmt.Errorf("invalid standby circuit endpoint for %s", peer.NodeID)
			}
			if _, exists := endpoints[endpoint]; exists {
				return fmt.Errorf("duplicate circuit endpoint for %s", peer.NodeID)
			}
			endpoints[endpoint] = struct{}{}
		}
		if _, exists := workers[peer.WorkerID]; exists {
			return fmt.Errorf("duplicate circuit worker %s", peer.WorkerID)
		}
		if _, exists := nodes[peer.NodeID]; exists {
			return fmt.Errorf("duplicate circuit node %s", peer.NodeID)
		}
		workers[peer.WorkerID] = struct{}{}
		nodes[peer.NodeID] = struct{}{}
	}
	return nil
}

func (p Plan) Peer(nodeID string) (Peer, bool) {
	for _, peer := range p.Peers {
		if peer.NodeID == nodeID {
			return peer, true
		}
	}
	return Peer{}, false
}

func (p Plan) Includes(identity domain.Identity) bool {
	return slices.ContainsFunc(p.Peers, func(peer Peer) bool {
		return peer.WorkerID == identity.WorkerID && peer.NodeID == identity.NodeID
	})
}

func NewToken() (string, error) {
	return randomToken(32)
}

func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
