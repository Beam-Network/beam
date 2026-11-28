package roommedia

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/relay"
	sfuv2 "github.com/Beam-Network/beam/internal/workload/handlers/roommedia/sfu"
)

func init() {
	RegisterNodeRole(mediacontracts.RoleProfiles[mediacontracts.RoleSFU], runRTCNode)
	RegisterNodeRole(mediacontracts.RoleProfiles[mediacontracts.RoleRelay], runRTCNode)
}

const drainedIdleEnd = 30 * time.Second

func runRTCNode(ctx context.Context, node *Node) (string, error) {
	h := node.handler
	details := node.Details()
	view := node.View()
	factory, err := h.apiFactory()
	if err != nil {
		return "worker_failed", err
	}
	config := h.config
	var session *sfuv2.Session
	session, err = sfuv2.NewSession(sfuv2.Options{SessionID: details.SessionID, NodeID: details.NodeID, Role: details.Role,
		Factory: factory, Usage: node.Usage(), SessionEvent: node.EmitSessionEvent, Admission: func(kind string) error { return h.admit(kind, session) },
		ExternalSpeakers: details.Role == mediacontracts.RoleRelay, PublisherGrace: config.PublisherReconnectGrace,
		ICE: sfuv2.ICEConfig{Servers: config.ICEServers, TURNSecret: config.TURNSecret, TURNTTL: config.TURNCredentialTTL,
			UsernamePrefix: config.TURNUsernamePrefix},
		Logf: func(format string, args ...any) { log.Printf("room media "+details.NodeID+": "+format, args...) }})
	if err != nil {
		return "worker_failed", err
	}
	defer session.Close()
	session.ApplyView(view)
	h.trackSession(session, true)
	defer h.trackSession(session, false)
	base := "/v1/rtc/" + details.SessionID + "/"
	if err := node.Mount(base, session.Handler()); err != nil {
		return "worker_failed", err
	}
	kinds := []string{"whep"}
	if details.Role == mediacontracts.RoleSFU {
		kinds = []string{"whip", "whep"}
	}
	endpoints := make([]mediacontracts.TopologyEndpoint, 0, len(kinds))
	for _, kind := range kinds {
		endpoint, err := node.EndpointURL(base + kind)
		if err != nil {
			return "worker_failed", err
		}
		endpoints = append(endpoints, mediacontracts.TopologyEndpoint{Kind: kind, URL: endpoint})
	}
	node.SetEndpoints(endpoints)
	node.SetLoad(func() mediacontracts.NodeLoad {
		load := session.Load()
		return mediacontracts.NodeLoad{Publishers: load.Publishers, Subscribers: load.Subscribers, IngressBPS: load.IngressBPS,
			EgressBPS: load.EgressBPS, CPURatio: h.cpu.ratio()}
	})
	node.SetCounters(func() NodeCounters {
		load := session.Load()
		return NodeCounters{Sequence: uint64(load.Packets), Packets: load.Packets}
	})
	var upstream *relay.Relay
	if details.Role == mediacontracts.RoleRelay {
		upstream, err = relay.New(relay.Options{Session: session, Factory: factory, NodeID: details.NodeID,
			SessionID: details.SessionID, ServiceToken: *details.ServiceToken, Usage: node.Usage(),
			E2EE: view.Protection.Mode == "e2ee", ICEServers: config.ICEServers,
			Logf: func(format string, args ...any) { log.Printf("room media "+details.NodeID+": "+format, args...) }})
		if err != nil {
			return "worker_failed", err
		}
		defer upstream.Close()
		upstream.SetParent(relay.ParentOf(view, details.NodeID))
	}
	idleSince := time.Time{}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "expired", nil
			}
			return "source_closed", nil
		case <-session.Failed():
			return "worker_failed", session.Failure()
		case next := <-node.Updates():
			session.ApplyView(next)
			if upstream != nil {
				upstream.SetParent(relay.ParentOf(next, details.NodeID))
			}
		case now := <-ticker.C:
			if !draining(node.View(), details.NodeID) {
				idleSince = time.Time{}
				continue
			}
			load := session.Load()
			if load.Subscribers+load.Publishers > 0 {
				idleSince = time.Time{}
				continue
			}
			if idleSince.IsZero() {
				idleSince = now
			} else if now.Sub(idleSince) >= drainedIdleEnd {
				return "ended", nil
			}
		}
	}
}

func draining(view *mediacontracts.Topology, nodeID string) bool {
	if view == nil {
		return false
	}
	for _, node := range view.Nodes {
		if node.NodeID == nodeID {
			return node.State == "draining"
		}
	}
	return true
}

func (h *Handler) PrepareRealtime() error {
	factory, err := h.apiFactory()
	if err != nil {
		return err
	}
	return factory.Bind()
}

func (h *Handler) apiFactory() (*sfuv2.APIFactory, error) {
	h.rtcMu.Lock()
	defer h.rtcMu.Unlock()
	if h.factory != nil {
		return h.factory, nil
	}
	factory, err := sfuv2.NewAPIFactory(sfuv2.Network{CPU: h.cpu.ratio, PublicIP: h.config.PublicIP, UDPPortMin: h.config.UDPPortMin,
		UDPPortMax: h.config.UDPPortMax, ReceiveMTU: 1500})
	if err != nil {
		return nil, err
	}
	h.factory = factory
	return factory, nil
}

func (h *Handler) trackSession(session *sfuv2.Session, add bool) {
	h.rtcMu.Lock()
	defer h.rtcMu.Unlock()
	if add {
		h.sessions[session] = struct{}{}
	} else {
		delete(h.sessions, session)
	}
}

func (h *Handler) admit(kind string, session *sfuv2.Session) error {
	if h.cpu.ratio() > 0.9 {
		return sfuv2.SessionFull("the Worker's CPU is saturated")
	}
	egressBudget, ingressBudget := h.capacity()
	h.rtcMu.Lock()
	var egress, ingress, subscribers int64
	for session := range h.sessions {
		load := session.Load()
		egress += load.EgressBPS
		ingress += load.IngressBPS
		subscribers += load.Subscribers
	}
	h.rtcMu.Unlock()
	own := session.Load()
	switch kind {
	case "whep":
		if egressBudget <= 0 {
			if own.Subscribers-own.Children >= int64(h.config.MaxViewers) {
				return sfuv2.SessionFull("the node's subscriber limit is reached")
			}
			return nil
		}
		perSubscriber := int64(1_500_000)
		if subscribers > 0 && egress/subscribers > perSubscriber {
			perSubscriber = egress / subscribers
		}
		if egress+perSubscriber > egressBudget*8/10 {
			return sfuv2.SessionFull("the Worker's egress capacity is reached")
		}
	case "whip":
		if ingressBudget <= 0 {
			if own.Publishers >= int64(h.config.MaxViewers) {
				return sfuv2.SessionFull("the node's publisher limit is reached")
			}
			return nil
		}
		if ingress+4_000_000 > ingressBudget*8/10 {
			return sfuv2.SessionFull("the Worker's ingress capacity is reached")
		}
	}
	return nil
}

func (h *Handler) capacity() (int64, int64) {
	if fleet := h.config.Fleet; fleet != nil {
		if capacity := fleet.Capacity(); capacity != nil {
			return bandwidthBPS(capacity.Egress), bandwidthBPS(capacity.Ingress)
		}
	}
	return declaredCapacity()
}

func bandwidthBPS(value mediacontracts.Bandwidth) int64 {
	if value.MeasuredMbps != nil && *value.MeasuredMbps > 0 {
		return *value.MeasuredMbps * 1_000_000
	}
	return value.DeclaredMbps * 1_000_000
}
