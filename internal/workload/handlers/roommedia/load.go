package roommedia

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cpuMeter struct {
	mu       sync.Mutex
	lastAt   time.Time
	lastCPU  time.Duration
	value    float64
	sampleAt time.Time
}

func (m *cpuMeter) ratio() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if now.Sub(m.sampleAt) < time.Second {
		return m.value
	}
	used, ok := processCPU()
	if !ok {
		return 0
	}
	if !m.lastAt.IsZero() {
		elapsed := now.Sub(m.lastAt)
		if elapsed > 0 {
			m.value = min(max(float64(used-m.lastCPU)/float64(elapsed)/float64(runtime.NumCPU()), 0), 1)
		}
	}
	m.lastAt, m.lastCPU, m.sampleAt = now, used, now
	return m.value
}

func declaredCapacity() (int64, int64) {
	return mbpsFromEnvironment("BEAM_MEDIA_EGRESS_MBPS"), mbpsFromEnvironment("BEAM_MEDIA_INGRESS_MBPS")
}

func mbpsFromEnvironment(name string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil || value <= 0 {
		return 0
	}
	return value * 1_000_000
}
