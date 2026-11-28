package broadcast

import (
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type Hooks struct {
	URL            func(path string) (string, error)
	Hostname       func() *string
	SetEndpoints   func([]mc.TopologyEndpoint)
	Event          func(body any)
	SetLoad        func(func() mc.NodeLoad)
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

type Env struct {
	Config     Config
	Router     *Router
	Node       *Node
	Emitter    *Emitter
	Usage      *Usage
	Hooks      Hooks
	Bucket     Bucket
	BucketErr  error
	StateDir   string
	Resumed    bool
	Redelivery bool

	Publishers  atomic.Int64
	Subscribers atomic.Int64
	IngressB    atomic.Int64
	EgressB     atomic.Int64
	Players     func(now time.Time) int64

	hostsMu sync.Mutex
	hosts   map[string]func()
}

func (e *Env) URL(path string) string {
	value, err := e.Hooks.URL(path)
	if err != nil {
		return ""
	}
	return value
}

func (e *Env) Host() string {
	if host := e.Hooks.Hostname(); host != nil {
		return *host
	}
	if parsed, err := url.Parse(e.URL("/")); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return e.Config.PublicHost
}

func (e *Env) Announce(endpoints []mc.TopologyEndpoint) {
	if endpoints == nil {
		endpoints = []mc.TopologyEndpoint{}
	}
	e.Hooks.SetEndpoints(endpoints)
	served := []string{e.Host(), e.Config.PublicIP}
	e.hostsMu.Lock()
	defer e.hostsMu.Unlock()
	if e.hosts == nil {
		e.hosts = map[string]func(){}
	}
	for _, host := range served {
		if _, held := e.hosts[host]; host != "" && !held {
			e.hosts[host] = serveHost(host)
		}
	}
}

func (e *Env) releaseHosts() {
	e.hostsMu.Lock()
	defer e.hostsMu.Unlock()
	for host, release := range e.hosts {
		release()
		delete(e.hosts, host)
	}
}

func (e *Env) RunLoad(ctx context.Context, interval time.Duration, reannounce func()) {
	var lastIn, lastOut atomic.Int64
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	e.Hooks.SetLoad(func() mc.NodeLoad {
		now := time.Now().UnixNano()
		seconds := float64(now-last.Swap(now)) / float64(time.Second)
		if seconds <= 0 {
			seconds = 1
		}
		in, out := e.IngressB.Load(), e.EgressB.Load()
		subscribers := e.Subscribers.Load()
		if e.Players != nil {
			subscribers += e.Players(time.Now())
		}
		return mc.NodeLoad{Publishers: e.Publishers.Load(), Subscribers: subscribers,
			IngressBPS: int64(float64(in-lastIn.Swap(in)) * 8 / seconds), EgressBPS: int64(float64(out-lastOut.Swap(out)) * 8 / seconds),
			CPURatio: CPURatio()}
	})
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			e.releaseHosts()
			return
		case <-ticker.C:
			if reannounce != nil {
				reannounce()
			}
		}
	}
}

func (e *Env) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if e.Hooks.GetCertificate != nil {
		if certificate, err := e.Hooks.GetCertificate(hello); err == nil && certificate != nil {
			return certificate, nil
		}
	}
	host := hello.ServerName
	if host == "" {
		if local, _, err := net.SplitHostPort(hello.Conn.LocalAddr().String()); err == nil {
			host = local
		}
	}
	return fallbackCertificate(host)
}

type Emitter struct {
	event func(any)
	final func(context.Context, any)
}

func NewEmitter(event func(any)) *Emitter { return &Emitter{event: event} }

func NewEmitterWithFinal(event func(any), final func(context.Context, any)) *Emitter {
	return &Emitter{event: event, final: final}
}

func (e *Emitter) Event(body any) {
	if e != nil && e.event != nil {
		e.event(body)
	}
}

func (e *Emitter) FinalEvent(ctx context.Context, body any) {
	if e != nil && e.final != nil {
		e.final(ctx, body)
		return
	}
	e.Event(body)
}
