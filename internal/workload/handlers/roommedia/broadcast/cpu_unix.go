//go:build unix

package broadcast

import (
	"runtime"
	"sync"
	"syscall"
	"time"
)

var cpuSample struct {
	sync.Mutex
	at    time.Time
	spent time.Duration
	ratio float64
}

func CPURatio() float64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	spent := time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
	now := time.Now()
	cpuSample.Lock()
	defer cpuSample.Unlock()
	if !cpuSample.at.IsZero() {
		if wall := now.Sub(cpuSample.at); wall >= time.Second {
			ratio := float64(spent-cpuSample.spent) / float64(wall) / float64(runtime.NumCPU())
			cpuSample.ratio = max(0, min(1, ratio))
			cpuSample.at, cpuSample.spent = now, spent
		}
		return cpuSample.ratio
	}
	cpuSample.at, cpuSample.spent = now, spent
	return 0
}
