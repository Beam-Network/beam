package relay

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/sfu"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

const (
	commandTimeout   = 10 * time.Second
	switchTimeout    = 10 * time.Second
	reconnectMinimum = time.Second
	reconnectMaximum = 10 * time.Second
)

type Options struct {
	Session      *sfu.Session
	Factory      *sfu.APIFactory
	NodeID       string
	SessionID    string
	ServiceToken string
	Usage        *usage.Reporter
	E2EE         bool
	ICEServers   []string
	Logf         func(format string, args ...any)
}

type Relay struct {
	opts   Options
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	parent  parentRef
	active  *upstream
	pending *upstream
	closed  bool
	nextID  atomic.Uint64
}

type parentRef struct {
	nodeID string
	url    string
	pin    string
}

func New(opts Options) (*Relay, error) {
	if opts.Session == nil || opts.Factory == nil || opts.NodeID == "" || opts.ServiceToken == "" {
		return nil, errors.New("relay: options are incomplete")
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Relay{opts: opts, ctx: ctx, cancel: cancel}, nil
}

func ParentOf(view *mediacontracts.Topology, nodeID string) (*mediacontracts.TopologyNode, string) {
	if view == nil {
		return nil, ""
	}
	parentID := ""
	for _, link := range view.Links {
		if link.Child == nodeID {
			parentID = link.Parent
		}
	}
	for index := range view.Nodes {
		node := &view.Nodes[index]
		if node.NodeID != parentID {
			continue
		}
		for _, endpoint := range node.Endpoints {
			if endpoint.Kind == "whep" {
				return node, endpoint.URL
			}
		}
		return node, ""
	}
	return nil, ""
}

func (r *Relay) SetParent(node *mediacontracts.TopologyNode, whepURL string) {
	if node == nil || whepURL == "" {
		return
	}
	pin := ""
	if node.TLSCertificateSHA256 != nil {
		pin = *node.TLSCertificateSHA256
	}
	next := parentRef{nodeID: node.NodeID, url: whepURL, pin: pin}
	var previousPending, candidate *upstream
	changed := false
	r.locked(func() {
		if r.closed || next == r.parent {
			return
		}
		changed = true
		r.parent = next
		previousPending = r.pending
		candidate = r.newUpstream(next)
		if r.active == nil {
			r.active = candidate
			r.pending = nil
		} else {
			r.pending = candidate
		}
	})
	if !changed {
		return
	}
	if previousPending != nil {
		previousPending.close()
	}
	go r.run(candidate)
}

func (r *Relay) Close() {
	var active, pending *upstream
	r.locked(func() {
		r.closed = true
		active, pending = r.active, r.pending
		r.active, r.pending = nil, nil
	})
	r.cancel()
	if active != nil {
		active.close()
	}
	if pending != nil {
		pending.close()
	}
}

func (r *Relay) Parent() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.parent.nodeID
}

func (r *Relay) current(u *upstream) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active == u || r.pending == u
}

func (r *Relay) run(u *upstream) {
	defer func() {
		if value := recover(); value != nil {
			u.recovered("upstream loop", value)
			if replacement := r.replace(u); replacement != nil {
				time.AfterFunc(reconnectMaximum, func() {
					if r.ctx.Err() == nil && r.current(replacement) {
						go r.run(replacement)
					}
				})
			}
		}
	}()
	delay := reconnectMinimum
	for r.ctx.Err() == nil && r.current(u) {
		err := u.connect(r.ctx)
		if err == nil {
			delay = reconnectMinimum
			r.waitSwitch(u)
			select {
			case <-u.done:
			case <-r.ctx.Done():
			}
		} else {
			r.opts.Logf("relay %s: upstream to %s failed: %v", r.opts.NodeID, u.parent.nodeID, err)
		}
		u.close()
		if r.ctx.Err() != nil || !r.current(u) {
			return
		}
		select {
		case <-time.After(delay):
		case <-r.ctx.Done():
			return
		}
		delay = min(delay*2, reconnectMaximum)
		replacement := r.replace(u)
		if replacement == nil {
			return
		}
		u = replacement
	}
}

func (r *Relay) replace(u *upstream) *upstream {
	if r.ctx.Err() != nil {
		return nil
	}
	replacement := r.newUpstream(u.parent)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.active == u:
		r.active = replacement
	case r.pending == u:
		r.pending = replacement
	default:
		return nil
	}
	return replacement
}

func (r *Relay) waitSwitch(u *upstream) {
	pending := false
	r.locked(func() { pending = r.pending == u })
	if !pending {
		return
	}
	go func() {
		defer u.guard("parent switch")
		deadline := time.NewTimer(switchTimeout)
		defer deadline.Stop()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
	wait:
		for {
			select {
			case <-u.done:
				return
			case <-r.ctx.Done():
				return
			case <-deadline.C:
				break wait
			case <-ticker.C:
				if u.ready() {
					break wait
				}
			}
		}
		var previous *upstream
		promoted := false
		r.locked(func() {
			if r.pending != u {
				return
			}
			previous, promoted = r.active, true
			r.active, r.pending = u, nil
		})
		if !promoted {
			return
		}
		if previous != nil {
			previous.close()
		}
		u.reconcile()
		r.opts.Logf("relay %s: switched to parent %s", r.opts.NodeID, u.parent.nodeID)
	}()
}
