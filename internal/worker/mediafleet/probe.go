package mediafleet

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ProbePath                = "/v1/probe/bandwidth"
	ProbeMaxBytes            = 100_000_000
	ProbeConcurrency         = 3
	ProbeHourlyBytes         = 2_000_000_000
	ProbeTrustedReserveBytes = 500_000_000
	ProbeGroupInterval       = time.Minute
	ProbeGroupHourlyBytes    = 200_000_000
	ProbeTrustedInterval     = 10 * time.Second
	probeMaxGroups           = 4096
	probePruneInterval       = time.Minute
	probeWriteTimeout        = 30 * time.Second
)

func ParseProbeCIDRs(value string) ([]*net.IPNet, error) {
	var networks []*net.IPNet
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			ip := net.ParseIP(entry)
			if ip == nil {
				return nil, fmt.Errorf("invalid probe source %q", entry)
			}
			bits := 128
			if ip.To4() != nil {
				ip, bits = ip.To4(), 32
			}
			networks = append(networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid probe source %q: %w", entry, err)
		}
		networks = append(networks, network)
	}
	return networks, nil
}

type probeGroup struct {
	running  int
	last     time.Time
	bytes    int64
	window   time.Time
	lastSeen time.Time
}

type probeLimiter struct {
	mu             sync.Mutex
	trusted        []*net.IPNet
	running        int
	windowStart    time.Time
	windowBytes    int64
	untrustedBytes int64
	groups         map[string]*probeGroup
	trustedLast    map[string]time.Time
	lastPrune      time.Time
}

func (f *Fleet) ProbeHandler() http.Handler {
	return http.HandlerFunc(f.serveProbe)
}

func (f *Fleet) WrapHTTPS(next http.Handler) http.Handler {
	if f == nil {
		return next
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == ProbePath {
			f.serveProbe(response, request)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func WrapHTTPS(next http.Handler) http.Handler { return Default().WrapHTTPS(next) }

func (f *Fleet) serveProbe(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	if request.URL.Path != ProbePath {
		http.NotFound(response, request)
		return
	}
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	size, err := strconv.ParseInt(request.URL.Query().Get("bytes"), 10, 64)
	if err != nil || size < 1 || size > ProbeMaxBytes {
		http.Error(response, fmt.Sprintf("bytes must be an integer between 1 and %d", ProbeMaxBytes), http.StatusBadRequest)
		return
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = request.RemoteAddr
	}
	source := net.ParseIP(host)
	if source == nil {
		http.Error(response, "unknown probe source", http.StatusForbidden)
		return
	}
	release, retryAfter := f.probes.admit(source, size, time.Now())
	if release == nil {
		response.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		http.Error(response, "bandwidth probe rate limited", http.StatusTooManyRequests)
		return
	}
	_ = http.NewResponseController(response).SetWriteDeadline(time.Now().Add(probeWriteTimeout))
	response.Header().Set("Content-Type", "application/octet-stream")
	response.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	response.WriteHeader(http.StatusOK)
	sent, _ := io.Copy(response, newPatternReader(size))
	release(sent)
}

func (l *probeLimiter) isTrusted(source net.IP) bool {
	for _, network := range l.trusted {
		if network.Contains(source) {
			return true
		}
	}
	return false
}

func probeGroupKey(source net.IP) string {
	if v4 := source.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return source.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

func (l *probeLimiter) admit(source net.IP, size int64, now time.Time) (func(int64), time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.groups == nil {
		l.groups = map[string]*probeGroup{}
		l.trustedLast = map[string]time.Time{}
	}
	if now.Sub(l.windowStart) >= time.Hour {
		l.windowStart, l.windowBytes, l.untrustedBytes = now, 0, 0
	}
	if now.Sub(l.lastPrune) >= probePruneInterval {
		for key, group := range l.groups {
			if group.running == 0 && now.Sub(group.lastSeen) >= time.Hour {
				delete(l.groups, key)
			}
		}
		for key, last := range l.trustedLast {
			if now.Sub(last) >= ProbeTrustedInterval {
				delete(l.trustedLast, key)
			}
		}
		l.lastPrune = now
	}
	if l.isTrusted(source) {
		key := source.String()
		last, seen := l.trustedLast[key]
		switch {
		case l.running >= ProbeConcurrency:
			return nil, 5 * time.Second
		case l.windowBytes+size > ProbeHourlyBytes:
			return nil, time.Hour - now.Sub(l.windowStart)
		case seen && now.Sub(last) < ProbeTrustedInterval:
			return nil, ProbeTrustedInterval - now.Sub(last)
		}
		if !seen && len(l.trustedLast) >= probeMaxGroups {
			l.evictOldestTrusted()
		}
		l.trustedLast[key] = now
		l.running++
		l.windowBytes += size
		window := l.windowStart
		return func(sent int64) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.running--
			if l.windowStart.Equal(window) {
				l.windowBytes -= max(0, size-sent)
			}
		}, 0
	}
	key := probeGroupKey(source)
	group := l.groups[key]
	state := probeGroup{}
	if group != nil {
		state = *group
	}
	if now.Sub(state.window) >= time.Hour {
		state.window, state.bytes = now, 0
	}
	switch {
	case l.running >= ProbeConcurrency-1:
		return nil, 5 * time.Second
	case l.windowBytes+size > ProbeHourlyBytes || l.untrustedBytes+size > ProbeHourlyBytes-ProbeTrustedReserveBytes:
		return nil, time.Hour - now.Sub(l.windowStart)
	case state.running > 0:
		return nil, 5 * time.Second
	case !state.last.IsZero() && now.Sub(state.last) < ProbeGroupInterval:
		return nil, ProbeGroupInterval - now.Sub(state.last)
	case state.bytes+size > ProbeGroupHourlyBytes:
		return nil, time.Hour - now.Sub(state.window)
	}
	if group == nil {
		if len(l.groups) >= probeMaxGroups && !l.evictOldestIdleGroup() {
			return nil, 5 * time.Second
		}
		group = &probeGroup{}
		l.groups[key] = group
	}
	*group = state
	group.running++
	group.last, group.lastSeen = now, now
	group.bytes += size
	l.running++
	l.windowBytes += size
	l.untrustedBytes += size
	window, groupWindow := l.windowStart, group.window
	return func(sent int64) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.running--
		group.running--
		refund := max(0, size-sent)
		if l.windowStart.Equal(window) {
			l.windowBytes -= refund
			l.untrustedBytes -= refund
		}
		if group.window.Equal(groupWindow) {
			group.bytes -= refund
		}
	}, 0
}

func (l *probeLimiter) evictOldestIdleGroup() bool {
	oldestKey := ""
	var oldest time.Time
	for key, group := range l.groups {
		if group.running == 0 && (oldestKey == "" || group.lastSeen.Before(oldest)) {
			oldestKey, oldest = key, group.lastSeen
		}
	}
	if oldestKey == "" {
		return false
	}
	delete(l.groups, oldestKey)
	return true
}

func (l *probeLimiter) evictOldestTrusted() {
	oldestKey := ""
	var oldest time.Time
	for key, last := range l.trustedLast {
		if oldestKey == "" || last.Before(oldest) {
			oldestKey, oldest = key, last
		}
	}
	delete(l.trustedLast, oldestKey)
}
