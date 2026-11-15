package runtime

import (
	"context"
	"sync"
	"time"
)

type leaseContext struct {
	parent   context.Context
	mu       sync.Mutex
	deadline time.Time
	timer    *time.Timer
	done     chan struct{}
	err      error
}

func newLeaseContext(parent context.Context, deadline time.Time) (*leaseContext, context.CancelFunc) {
	ctx := &leaseContext{parent: parent, deadline: deadline, done: make(chan struct{})}
	ctx.mu.Lock()
	ctx.timer = time.AfterFunc(time.Until(deadline), func() { ctx.end(context.DeadlineExceeded) })
	ctx.mu.Unlock()
	return ctx, func() { ctx.end(context.Canceled) }
}

func (ctx *leaseContext) Deadline() (time.Time, bool) {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	return ctx.deadline, true
}

func (ctx *leaseContext) Done() <-chan struct{} { return ctx.done }

func (ctx *leaseContext) Err() error {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	return ctx.err
}

func (ctx *leaseContext) Value(key any) any { return ctx.parent.Value(key) }

func (ctx *leaseContext) end(err error) {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	if ctx.err != nil {
		return
	}
	ctx.err = err
	if ctx.timer != nil {
		ctx.timer.Stop()
	}
	close(ctx.done)
}

func (ctx *leaseContext) extend(deadline time.Time) bool {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	if ctx.err != nil || !ctx.timer.Stop() {
		return false
	}
	if deadline.After(ctx.deadline) {
		ctx.deadline = deadline
	}
	ctx.timer.Reset(time.Until(ctx.deadline))
	return true
}
