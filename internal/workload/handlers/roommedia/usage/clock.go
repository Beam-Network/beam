package usage

import (
	"log"
	"time"
)

const smallStep = int64(time.Second / time.Millisecond)

func (r *Reporter) NowMS() int64 {
	for {
		now, last := r.config.Now().UnixMilli(), r.clock.Load()
		if now <= last && last-now <= smallStep {
			return last
		}
		if r.clock.CompareAndSwap(last, now) {
			return now
		}
	}
}

func (r *Reporter) MonoMS() int64 { return r.monoMS() }

func (r *Reporter) monoMS() int64 {
	if r.config.Mono != nil {
		return r.config.Mono().Milliseconds()
	}
	return r.config.Now().Sub(r.born).Milliseconds()
}

func (r *Reporter) ahead() int64 {
	now := r.config.Now()
	elapsed := now.Sub(r.born)
	if r.config.Mono != nil {
		elapsed = r.config.Mono()
	}
	lead := time.Duration(now.UnixNano()-r.born.UnixNano()) - elapsed
	if !r.leadSet || lead < r.lead {
		r.lead, r.leadSet = lead, true
		return 0
	}
	ms := (lead - r.lead).Milliseconds()
	r.lead += time.Duration(ms) * time.Millisecond
	return ms
}

func (r *Reporter) stepped(connections []*Connection, last, at int64) {
	r.mu.Lock()
	r.periodStart = min(r.periodStart, at)
	start := r.periodStart
	r.mu.Unlock()
	log.Printf("media %s: the clock went back %d ms: the usage reports follow it", r.config.NodeID, last-at)
	for _, connection := range connections {
		connection.mu.Lock()
		connection.presence.rebase(last, at, start)
		connection.objects.before(at)
		if connection.latest != nil {
			connection.latest.ReceivedMS = min(connection.latest.ReceivedMS, at)
		}
		connection.sampledMS = min(connection.sampledMS, at)
		connection.mu.Unlock()
	}
}
