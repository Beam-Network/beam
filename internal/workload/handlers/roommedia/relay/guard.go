package relay

import "runtime/debug"

func (u *upstream) guard(what string) {
	if value := recover(); value != nil {
		u.recovered(what, value)
	}
}

func (u *upstream) recovered(what string, value any) {
	u.relay.opts.Logf("relay %s: recovered panic in %s (parent %s): %v\n%s", u.relay.opts.NodeID, what, u.parent.nodeID,
		value, debug.Stack())
	go func() {
		defer func() { _ = recover() }()
		u.close()
	}()
}

func (r *Relay) locked(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn()
}

func (u *upstream) locked(fn func()) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fn()
}
