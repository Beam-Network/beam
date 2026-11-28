package broadcast

import (
	"net/http"
	"sync"
	"time"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/ratelimit"
)

const (
	heldPerNode    = 4096
	heldPerPlayer  = 16
	heldPerAddress = 512
	heldPerChild   = 2048
)

type Holder struct {
	key   string
	limit int
}

type HoldLimiter struct {
	mu       sync.Mutex
	total    int
	byHolder map[string]int
}

func (l *HoldLimiter) Acquire(holder Holder) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byHolder == nil {
		l.byHolder = map[string]int{}
	}
	if l.total >= heldPerNode || l.byHolder[holder.key] >= holder.limit {
		return nil, false
	}
	l.total++
	l.byHolder[holder.key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.total--
			if l.byHolder[holder.key]--; l.byHolder[holder.key] <= 0 {
				delete(l.byHolder, holder.key)
			}
			l.mu.Unlock()
		})
	}, true
}

func HolderOf(request *http.Request, access HLSAccess) Holder {
	switch {
	case access.Child != "":
		return Holder{key: "child:" + access.Child, limit: heldPerChild}
	case access.Nonce != "":
		return Holder{key: "player:" + access.Nonce, limit: heldPerPlayer}
	default:
		return Holder{key: "address:" + ClientKey(request), limit: heldPerAddress}
	}
}

func WriteRateLimited(response http.ResponseWriter) {
	retry := int64(1000)
	body := mc.APIError{Error: "rate_limited", RetryAfterMS: &retry}
	encoded, _ := mc.MarshalWire(body)
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Retry-After", "1")
	response.WriteHeader(http.StatusTooManyRequests)
	_, _ = response.Write(encoded)
}

func MaxPartIndex(topology *mc.Topology) int64 {
	if topology == nil || topology.Packaging == nil || topology.Packaging.PartMS <= 0 {
		return 64
	}
	return mc.HLSMaxPartIndex(topology.Ladder, *topology.Packaging)
}

func HoldLimit(topology *mc.Topology) time.Duration {
	if topology == nil || topology.Packaging == nil {
		return 3 * time.Second
	}
	return 3 * time.Duration(max(mc.HLSTargetDurationS(topology.Ladder, *topology.Packaging), 1)) * time.Second
}

type RequestLimiter = ratelimit.Limiter
