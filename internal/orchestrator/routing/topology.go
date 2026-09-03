package routing

import (
	"errors"
	"sync"
	"time"
)

// Topology keeps ephemeral directed link telemetry. Authority remains in the
// membership registry; these observations influence cost only.
type Topology struct {
	mu    sync.RWMutex
	links map[string]LinkMetric
}

func NewTopology() *Topology { return &Topology{links: make(map[string]LinkMetric)} }

func (t *Topology) Observe(metrics []LinkMetric, now time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, metric := range metrics {
		if metric.FromWorkerID == "" || metric.ToWorkerID == "" || metric.FromWorkerID == metric.ToWorkerID ||
			metric.RTTMillis < 0 || metric.ThroughputMbps <= 0 || metric.LossPPM < 0 {
			return errors.New("link observation requires distinct workers, non-negative RTT/loss, and positive throughput")
		}
		if metric.ObservedAt.IsZero() {
			metric.ObservedAt = now
		}
		key := metric.FromWorkerID + "\x00" + metric.ToWorkerID
		if previous, ok := t.links[key]; !ok || !metric.ObservedAt.Before(previous.ObservedAt) {
			t.links[key] = metric
		}
	}
	return nil
}

func (t *Topology) Links() []LinkMetric {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]LinkMetric, 0, len(t.links))
	for _, metric := range t.links {
		result = append(result, metric)
	}
	return result
}
