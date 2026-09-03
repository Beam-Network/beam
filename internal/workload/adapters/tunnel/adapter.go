package tunnel

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type Assignment struct {
	AssignmentID   string            `json:"assignment_id"`
	TunnelID       string            `json:"tunnel_id"`
	Mode           string            `json:"mode,omitempty"`
	Protocol       string            `json:"protocol"`
	Capabilities   []string          `json:"capabilities,omitempty"`
	Resources      domain.Resources  `json:"resources"`
	OfferExpiresAt time.Time         `json:"offer_expires_at"`
	ListenAddress  string            `json:"listen_address,omitempty"`
	Target         string            `json:"target"`
	NetworkTargets []string          `json:"network_targets,omitempty"`
	IngressToken   string            `json:"ingress_token,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

func ToWorkload(assignment Assignment, identity domain.Identity) (domain.Spec, error) {
	if assignment.AssignmentID == "" || assignment.TunnelID == "" || assignment.Protocol == "" {
		return domain.Spec{}, errors.New("Tunnel assignment_id, tunnel_id, and protocol are required")
	}
	kind := domain.KindTunnelHTTP
	if assignment.Protocol == "tcp" {
		kind = domain.KindTunnelTCP
	}
	payload, err := json.Marshal(contracts.TunnelAssignment{
		TunnelID: assignment.TunnelID, Mode: assignment.Mode,
		Protocol: assignment.Protocol, ListenAddress: assignment.ListenAddress,
		Target: assignment.Target, IngressToken: assignment.IngressToken, Metadata: assignment.Metadata,
	})
	if err != nil {
		return domain.Spec{}, err
	}
	networkTargets := assignment.NetworkTargets
	if len(networkTargets) == 0 && assignment.Target != "" {
		networkTargets = []string{assignment.Target}
	}
	return domain.Spec{
		WorkloadID: assignment.TunnelID, AttemptID: assignment.AssignmentID,
		Identity: identity, Kind: kind, Class: domain.ClassService,
		Source:               domain.Source{System: "beam-tunnel", Reference: assignment.AssignmentID},
		RequiredCapabilities: assignment.Capabilities, Resources: assignment.Resources,
		Lease:    domain.Lease{OfferExpiresAt: assignment.OfferExpiresAt},
		Security: domain.SecurityPolicy{NetworkTargets: networkTargets, TrustProfile: "tunnel-assignment"},
		Evidence: domain.EvidencePolicy{ReceiptRequired: true}, Payload: payload,
	}, nil
}

type RoomAssignment struct {
	AssignmentID         string            `json:"assignment_id"`
	RoomID               string            `json:"room_id"`
	Role                 string            `json:"role"`
	Protocol             string            `json:"protocol,omitempty"`
	ListenAddress        string            `json:"listen_address,omitempty"`
	Members              map[string]string `json:"members,omitempty"`
	MaxDatagramBytes     int               `json:"max_datagram_bytes,omitempty"`
	IdleTimeout          time.Duration     `json:"idle_timeout,omitempty"`
	Resources            domain.Resources  `json:"resources"`
	OfferExpiresAt       time.Time         `json:"offer_expires_at"`
	Metadata             map[string]string `json:"metadata,omitempty"`
	RequiredCapabilities []string          `json:"required_capabilities,omitempty"`
}

func ToRoomWorkload(assignment RoomAssignment, identity domain.Identity) (domain.Spec, error) {
	if assignment.AssignmentID == "" || assignment.RoomID == "" || assignment.Role == "" {
		return domain.Spec{}, errors.New("Tunnel room assignment_id, room_id, and role are required")
	}
	protocol := assignment.Protocol
	if protocol == "" {
		protocol = "udp"
	}
	payload, err := json.Marshal(contracts.RoomAssignment{
		RoomID: assignment.RoomID, Role: assignment.Role, Protocol: protocol,
		ListenAddress: assignment.ListenAddress, Members: assignment.Members,
		MaxDatagramBytes:   assignment.MaxDatagramBytes,
		IdleTimeoutSeconds: int64(assignment.IdleTimeout / time.Second), Metadata: assignment.Metadata,
	})
	if err != nil {
		return domain.Spec{}, err
	}
	capabilities := assignment.RequiredCapabilities
	if len(capabilities) == 0 {
		capabilities = []string{"room.media"}
	}
	return domain.Spec{
		WorkloadID: assignment.RoomID, AttemptID: assignment.AssignmentID, Identity: identity,
		Kind: domain.KindRoomMedia, Class: domain.ClassService,
		Source:               domain.Source{System: "beam-tunnel", Reference: assignment.AssignmentID},
		RequiredCapabilities: capabilities, Resources: assignment.Resources,
		Lease:    domain.Lease{OfferExpiresAt: assignment.OfferExpiresAt},
		Evidence: domain.EvidencePolicy{ReceiptRequired: true}, Payload: payload,
	}, nil
}
