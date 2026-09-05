package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/roomtransfer"
	"github.com/Beam-Network/beam/internal/orchestrator/roomworkloads"
	"github.com/Beam-Network/beam/internal/workload/contracts"
)

type RoomTunnelHTTPProvisioner struct {
	transferEndpoint string
	workloadEndpoint string
	client           *http.Client
}

func NewRoomTunnelHTTPProvisioner(coordinatorURL string, client *http.Client) (*RoomTunnelHTTPProvisioner, error) {
	coordinatorURL = strings.TrimRight(strings.TrimSpace(coordinatorURL), "/")
	if coordinatorURL == "" {
		return nil, errors.New("room tunnel coordinator URL is required")
	}
	if err := validateCoordinatorURL(coordinatorURL); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &RoomTunnelHTTPProvisioner{transferEndpoint: coordinatorURL + "/room-transfer/leases/redeem",
		workloadEndpoint: coordinatorURL + "/room-workload/paths/redeem", client: client}, nil
}

func (p *RoomTunnelHTTPProvisioner) Redeem(ctx context.Context, request roomtransfer.RedeemRequest) (contracts.TunnelLease, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return contracts.TunnelLease{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, p.transferEndpoint, bytes.NewReader(payload))
	if err != nil {
		return contracts.TunnelLease{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(httpRequest)
	if err != nil {
		return contracts.TunnelLease{}, err
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return contracts.TunnelLease{}, err
	}
	var result struct {
		Accepted bool                  `json:"accepted"`
		Lease    contracts.TunnelLease `json:"lease"`
		Reason   string                `json:"reason"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return contracts.TunnelLease{}, fmt.Errorf("decode room tunnel lease response: %w", err)
	}
	if response.StatusCode != http.StatusOK || !result.Accepted {
		return contracts.TunnelLease{}, errors.New(fallback(result.Reason, "Tunnel coordinator rejected room lease intent"))
	}
	if result.Lease.LeaseID == "" {
		return contracts.TunnelLease{}, errors.New("Tunnel coordinator returned an empty room lease")
	}
	return result.Lease, nil
}

var _ roomtransfer.Provisioner = (*RoomTunnelHTTPProvisioner)(nil)
var _ roomworkloads.Provisioner = (*RoomTunnelHTTPProvisioner)(nil)

func (p *RoomTunnelHTTPProvisioner) RedeemRoomPath(ctx context.Context,
	request contracts.RoomPathRedemptionRequest) (contracts.RoomPathLease, error) {
	identity, path := request.Identity, request.Path
	protocol := contracts.RoomPathProtocol(identity.Kind)
	payload, err := json.Marshal(map[string]any{
		"schema_version": "room-workload-path/v1", "intent_id": path.PathID,
		"workload_id": identity.WorkloadID, "kind": identity.Kind, "room_id": identity.RoomID,
		"channel_id": identity.ChannelID, "unit_id": identity.UnitID, "epoch": identity.Epoch,
		"attempt": identity.Attempt, "role": path.Role, "target_member_id": path.TargetMemberID,
		"worker_id": request.WorkerID, "protocol": protocol, "details": json.RawMessage("{}"),
		"expires_at": minRoomPathExpiry(identity.ExpiresAt, path.ExpiresAt), "coordinator_signature": path.CoordinatorSignature,
	})
	if err != nil {
		return contracts.RoomPathLease{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, p.workloadEndpoint, bytes.NewReader(payload))
	if err != nil {
		return contracts.RoomPathLease{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(httpRequest)
	if err != nil {
		return contracts.RoomPathLease{}, err
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return contracts.RoomPathLease{}, err
	}
	var result struct {
		Accepted bool   `json:"accepted"`
		Reason   string `json:"reason"`
		Lease    struct {
			IntentID       string                   `json:"intent_id"`
			Role           string                   `json:"role"`
			TargetMemberID string                   `json:"target_member_id"`
			Endpoints      []contracts.HTTPEndpoint `json:"endpoints"`
			ExpiresAt      time.Time                `json:"expires_at"`
		} `json:"lease"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return contracts.RoomPathLease{}, fmt.Errorf("decode room workload path response: %w", err)
	}
	if response.StatusCode != http.StatusOK || !result.Accepted {
		return contracts.RoomPathLease{}, errors.New(fallback(result.Reason, "Tunnel coordinator rejected room workload path"))
	}
	return contracts.RoomPathLease{PathID: result.Lease.IntentID, Role: result.Lease.Role,
		TargetMemberID: result.Lease.TargetMemberID, Protocol: path.Protocol, Endpoints: result.Lease.Endpoints,
		ExpiresAt: result.Lease.ExpiresAt}, nil
}

func minRoomPathExpiry(left, right time.Time) time.Time {
	if right.Before(left) {
		return right
	}
	return left
}

func validateCoordinatorURL(raw string) error {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Host == "" {
		return errors.New("room tunnel coordinator URL must be an absolute URL")
	}
	if endpoint.Scheme == "https" {
		return nil
	}
	if endpoint.Scheme == "http" && isLoopbackHost(endpoint.Hostname()) {
		return nil
	}
	return errors.New("room tunnel coordinator URL must use HTTPS outside loopback; mainnet uses https://coordinator.b1m.ai")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}
