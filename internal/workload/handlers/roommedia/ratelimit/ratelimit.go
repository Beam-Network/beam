package ratelimit

import (
	"container/list"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	Rate          = 10.0
	Burst         = 30.0
	MaxKeys       = 65_536
	refillTime    = time.Duration(Burst / Rate * float64(time.Second))
	sweepInterval = time.Second
)

type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*list.Element
	order   list.List
	swept   time.Time
}

type bucket struct {
	key    string
	tokens float64
	at     time.Time
}

func (l *Limiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]*list.Element{}
	}
	if now.Sub(l.swept) >= sweepInterval {
		l.sweep(now)
	}
	element := l.buckets[key]
	if element == nil {
		if l.order.Len() >= MaxKeys {
			oldest := l.order.Back()
			delete(l.buckets, oldest.Value.(*bucket).key)
			l.order.Remove(oldest)
		}
		element = l.order.PushFront(&bucket{key: key, tokens: Burst, at: now})
		l.buckets[key] = element
	} else {
		l.order.MoveToFront(element)
	}
	current := element.Value.(*bucket)
	current.tokens = min(Burst, current.tokens+now.Sub(current.at).Seconds()*Rate)
	current.at = now
	if current.tokens < 1 {
		return false
	}
	current.tokens--
	return true
}

func (l *Limiter) Refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if element := l.buckets[key]; element != nil {
		current := element.Value.(*bucket)
		current.tokens = min(Burst, current.tokens+1)
	}
}

func (l *Limiter) sweep(now time.Time) {
	for element := l.order.Back(); element != nil; {
		current := element.Value.(*bucket)
		if now.Sub(current.at) < refillTime {
			break
		}
		previous := element.Prev()
		delete(l.buckets, current.key)
		l.order.Remove(element)
		element = previous
	}
	l.swept = now
}

func ClientIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = request.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if ip.To4() != nil {
		return ip.To4().String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
