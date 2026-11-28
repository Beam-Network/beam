package sfu

import (
	"math"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	probeTick          = 20 * time.Millisecond
	probeMaxDuration   = 20 * time.Second
	probeBackoffMin    = 2 * time.Second
	probeBackoffMax    = 120 * time.Second
	probeSameGoal      = 0.1
	probeProgress      = 5 * time.Second
	probeGrowth        = 1.05
	probeLoss          = 0.1
	probeRecovered     = 1.2
	probeHeadroom      = 1.25
	probeDrop          = 0.85
	paddingPacketBytes = 255
	maxPaddingBurst    = 8
	maxPaddingBPS      = maxPaddingBurst * paddingPacketBytes * 8 * int64(time.Second/probeTick)
	nodePaddingBPS     = 20_000_000
	nodePaddingBurst   = 100 * time.Millisecond
	probeStartCPU      = 0.6
	probeStopCPU       = 0.75
)

var paddingPayload = func() []byte {
	payload := make([]byte, paddingPacketBytes)
	payload[len(payload)-1] = paddingPacketBytes
	return payload
}()

type prober struct {
	mu        sync.Mutex
	active    bool
	goal      int64
	media     int64
	estimate  int64
	started   time.Time
	peak      int64
	mark      int64
	markAt    time.Time
	failedAt  int64
	failedFor int64
	failures  int
	backoff   time.Duration
	notBefore time.Time
	running   bool
	padded    int64
	budget    *paddingBudget
	reserved  int64
}

func (p *prober) update(c *subscriberConn, now time.Time, estimate int64, loss float64, goal, media int64) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.estimate, p.media = estimate, media
	if p.active {
		p.peak = max(p.peak, estimate)
		if float64(estimate) >= float64(p.mark)*probeGrowth {
			p.mark, p.markAt = estimate, now
		}
		switch {
		case goal <= 0 || estimate >= goal:
			p.end()
			p.backoff = 0
			return "probe_done"
		case estimate < int64(float64(p.peak)*probeDrop):
			p.fail(now, estimate)
			return "probe_failed_drop"
		case loss > probeLoss:
			p.fail(now, estimate)
			return "probe_failed_loss"
		case now.Sub(p.markAt) >= probeProgress:
			p.fail(now, estimate)
			return "probe_failed_flat"
		case now.Sub(p.started) >= probeMaxDuration:
			p.fail(now, estimate)
			return "probe_timeout"
		}
		p.goal = goal
		return ""
	}
	if goal <= 0 || estimate >= goal {
		return ""
	}
	if now.Before(p.notBefore) {
		changed := !sameGoal(goal, p.failedFor) || float64(estimate) >= float64(p.failedAt)*probeRecovered
		if !changed || now.Before(p.notBefore.Add(probeBackoffMin-p.backoff)) {
			return ""
		}
	}
	if c != nil && c.api != nil {
		p.budget = c.api.padding
	}
	reserved := min(max(goal-media, 0), maxPaddingBPS)
	if !p.budget.admit(reserved) {
		return ""
	}
	p.reserved = reserved
	p.active, p.started, p.peak, p.goal = true, now, estimate, goal
	p.mark, p.markAt = estimate, now
	if !p.running && c != nil && c.pc != nil {
		p.running = true
		go c.runProbe()
	}
	return "probe_start"
}

func (p *prober) end() {
	p.active = false
	p.budget.release(p.reserved)
	p.reserved = 0
}

func (p *prober) fail(now time.Time, estimate int64) {
	p.end()
	if p.failures > 0 && sameGoal(p.goal, p.failedFor) {
		p.failures++
	} else {
		p.failures = 1
	}
	p.failedAt, p.failedFor = estimate, p.goal
	p.backoff = probeBackoffMin
	for range p.failures - 1 {
		p.backoff = min(p.backoff*2, probeBackoffMax)
	}
	p.notBefore = now.Add(p.backoff)
}

func sameGoal(a, b int64) bool {
	if a <= 0 || b <= 0 {
		return a == b
	}
	return math.Abs(float64(a-b)) <= probeSameGoal*float64(max(a, b))
}

type paddingBudget struct {
	mu       sync.Mutex
	bps      int64
	tokens   float64
	last     time.Time
	reserved int64
	cpu      func() float64
}

func newPaddingBudget(bps int64, cpu func() float64) *paddingBudget {
	return &paddingBudget{bps: bps, cpu: cpu}
}

func (b *paddingBudget) admit(bps int64) bool {
	if b == nil {
		return true
	}
	if b.cpu != nil && b.cpu() > probeStartCPU {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reserved > 0 && b.reserved+bps > b.bps {
		return false
	}
	b.reserved += bps
	return true
}

func (b *paddingBudget) release(bps int64) {
	if b == nil || bps == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reserved -= bps
}

func (b *paddingBudget) burst() float64 {
	return float64(b.bps) * nodePaddingBurst.Seconds() / 8
}

func (b *paddingBudget) available(now time.Time) int {
	if b == nil {
		return math.MaxInt32
	}
	if b.cpu != nil && b.cpu() > probeStopCPU {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.tokens = b.burst()
	} else if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.tokens+float64(b.bps)*elapsed.Seconds()/8, b.burst())
	}
	b.last = now
	return int(max(b.tokens, 0))
}

func (b *paddingBudget) spend(bytes int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens -= float64(bytes)
}

func (p *prober) paddingRate() (int64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active {
		p.running = false
		return 0, false
	}
	target := min(p.goal, int64(float64(p.estimate)*probeHeadroom))
	return min(max(target-p.media, 0), maxPaddingBPS), true
}

func (p *prober) sent(bytes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.padded += int64(bytes)
}

func (p *prober) snapshot() (int64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.padded, p.active
}

func (c *subscriberConn) runProbe() {
	defer c.session.guard("whep probe", c.abort)
	defer func() {
		c.probe.mu.Lock()
		defer c.probe.mu.Unlock()
		if c.probe.running {
			c.probe.end()
			c.probe.running = false
		}
	}()
	ticker := time.NewTicker(probeTick)
	defer ticker.Stop()
	var debt float64
	for range ticker.C {
		if c.closed.Load() {
			return
		}
		rate, active := c.probe.paddingRate()
		if !active {
			return
		}
		debt = min(debt+float64(rate)*probeTick.Seconds()/8, float64(rate)*4*probeTick.Seconds()/8)
		if debt < paddingPacketBytes {
			continue
		}
		var budget *paddingBudget
		if c.api != nil {
			budget = c.api.padding
		}
		allowed := min(int(debt), budget.available(time.Now()))
		if allowed < paddingPacketBytes {
			continue
		}
		if sent := c.sendPadding(allowed); sent > 0 {
			debt -= float64(sent)
			budget.spend(sent)
			c.probe.sent(sent)
		}
	}
}

func (c *subscriberConn) sendPadding(bytes int) int {
	var forwarding *DownTrack
	for _, slot := range c.slotsSnapshot() {
		if slot.kind != webrtc.RTPCodecTypeVideo {
			continue
		}
		paused, ok := slot.canPad()
		if !ok {
			continue
		}
		if paused {
			return slot.pad(bytes)
		}
		if forwarding == nil {
			forwarding = slot
		}
	}
	if forwarding != nil {
		return forwarding.pad(bytes)
	}
	return 0
}

func (d *DownTrack) canPad() (paused, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.track == nil || d.fixed || !d.started || d.lastPT == 0 {
		return false, false
	}
	return d.current < 0, true
}

func (d *DownTrack) pad(bytes int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.track == nil || d.fixed || !d.started || d.lastPT == 0 || d.kind != webrtc.RTPCodecTypeVideo || d.conn.suspended.Load() {
		return 0
	}
	forwarding := d.current >= 0
	if forwarding && !d.frameEnded {
		return 0
	}
	bound, writer, ssrc := func() (bool, webrtc.TrackLocalWriter, uint32) {
		d.bindMu.Lock()
		defer d.bindMu.Unlock()
		return d.bound, d.writer, d.ssrc
	}()
	if !bound || writer == nil {
		return 0
	}
	sent := 0
	for packets := 0; sent < bytes && packets < maxPaddingBurst; packets++ {
		header := rtp.Header{Version: 2, Padding: true, PayloadType: d.lastPT, SequenceNumber: d.lastSeqOut + 1,
			Timestamp: d.lastTSOut, SSRC: ssrc}
		written, err := writer.WriteRTP(&header, paddingPayload)
		if err != nil {
			break
		}
		if forwarding {
			d.notePadding()
		}
		d.lastSeqOut++
		sent += written
	}
	return sent
}
