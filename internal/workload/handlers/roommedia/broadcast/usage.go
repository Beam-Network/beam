package broadcast

import (
	"context"
	"crypto/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

const (
	TransportWHIP     = usage.TransportWHIP
	TransportHLS      = usage.TransportHLS
	TransportRTMP     = usage.TransportRTMP
	TransportRTMPS    = usage.TransportRTMPS
	TransportSRT      = usage.TransportSRT
	TransportChild    = usage.TransportChild
	TransportUpstream = usage.TransportUpstream
	DirectionIngress  = usage.DirectionIngress
	DirectionEgress   = usage.DirectionEgress
)

const requestIdle = 30 * time.Second

type Usage struct {
	reporter *usage.Reporter

	mu       sync.Mutex
	requests map[string]*Connection
}

type Connection struct {
	inner     *usage.Connection
	mu        sync.Mutex
	seen      time.Time
	stream    bool
	connected atomic.Bool
}

func NewUsage(reporter *usage.Reporter) *Usage {
	return &Usage{reporter: reporter, requests: map[string]*Connection{}}
}

func (u *Usage) Open(nonce, identity *string, direction, transport string) *Connection {
	connection := u.open(usage.ConnectionInfo{TokenNonce: nonce, Identity: identity, Direction: direction, Transport: transport, Detached: true})
	connection.stream = true
	return connection
}

func (u *Usage) OpenWith(info usage.ConnectionInfo) *Connection { return u.open(info) }

func (u *Usage) open(info usage.ConnectionInfo) *Connection {
	return &Connection{inner: u.reporter.Open(info), seen: time.Now()}
}

func (u *Usage) Reporter() *usage.Reporter { return u.reporter }

func (u *Usage) RequestV1(key string, nonce, identity *string, direction, transport string) *Connection {
	u.mu.Lock()
	defer u.mu.Unlock()
	if connection := u.requests[key]; connection != nil {
		connection.touch()
		return connection
	}
	connection := u.open(usage.ConnectionInfo{TokenNonce: nonce, Identity: identity, Direction: direction, Transport: transport,
		Versions: usage.OnlyV1})
	u.requests[key] = connection
	return connection
}

func (u *Usage) Run(ctx context.Context) {
	ticker := time.NewTicker(requestIdle / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			u.mu.Lock()
			for key, connection := range u.requests {
				connection.Close()
				delete(u.requests, key)
			}
			u.mu.Unlock()
			return
		case now := <-ticker.C:
			u.mu.Lock()
			for key, connection := range u.requests {
				connection.mu.Lock()
				idle := now.Sub(connection.seen) > requestIdle
				connection.mu.Unlock()
				if idle {
					connection.Close()
					delete(u.requests, key)
				}
			}
			u.mu.Unlock()
		}
	}
}

func (c *Connection) touch() {
	c.mu.Lock()
	c.seen = time.Now()
	c.mu.Unlock()
}

func (c *Connection) Add(bytes int) {
	if c != nil {
		if c.stream && bytes > 0 && c.connected.CompareAndSwap(false, true) {
			c.inner.Connect()
		}
		c.inner.AddRTP(bytes, bytes, 0)
		c.touch()
	}
}

func (c *Connection) AddV1(bytes int) {
	if c != nil {
		c.inner.AddBytes(bytes)
		c.touch()
	}
}

func (c *Connection) Meter() *usage.Connection { return c.inner }

func (c *Connection) Close() {
	if c != nil {
		c.inner.Close()
	}
}

const idAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

func RandomID(length int) string {
	buffer := make([]byte, length)
	_, _ = rand.Read(buffer)
	for i := range buffer {
		buffer[i] = idAlphabet[int(buffer[i])%len(idAlphabet)]
	}
	return string(buffer)
}

func StringPtr(value string) *string { return &value }
