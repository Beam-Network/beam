package sfu

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
	"github.com/pion/webrtc/v4"
)

const (
	allocationInterval    = 500 * time.Millisecond
	budgetHeadroom        = 0.9
	audioDefaultBPS       = 48_000
	pauseAfter            = 5 * time.Second
	upgradeAfter          = 3 * time.Second
	upgradeBackoffMax     = 60 * time.Second
	flapWindow            = 30 * time.Second
	upgradeGrace          = time.Second
	keepLoss              = 0.02
	estimateSmoothing     = 3 * time.Second
	keepOverEstimate      = probeHeadroom
	allocationStatusEvery = 10 * time.Second
	allocationDecisionGap = time.Second
	probeLogGap           = 5 * time.Second
)

var defaultLayerBPS = [3]int64{180_000, 600_000, 1_800_000}

func (c *subscriberConn) allocate() {
	defer c.session.guard("whep allocation", c.abort)
	ticker := time.NewTicker(allocationInterval)
	defer ticker.Stop()
	for !c.closed.Load() {
		c.allocateOnce()
		c.sampleUsage(time.Now())
		<-ticker.C
	}
}

type slotAllocation struct {
	decided       bool
	spatial       int
	half          bool
	reason        string
	shortSince    time.Time
	upSince       time.Time
	missedSince   time.Time
	dwell         time.Duration
	dwellAt       time.Time
	upgradedAt    time.Time
	temporalSince time.Time
}

func (s *slotAllocation) upgradeDwell() time.Duration {
	if s.dwell <= 0 {
		return upgradeAfter
	}
	return s.dwell
}

func (s *slotAllocation) downgraded(now time.Time) {
	if !s.upgradedAt.IsZero() && now.Sub(s.upgradedAt) < flapWindow {
		s.dwell = min(s.upgradeDwell()*2, upgradeBackoffMax)
		s.dwellAt = now
	} else {
		s.dwell = upgradeAfter
	}
	s.upgradedAt = time.Time{}
}

func (s *slotAllocation) decay(now time.Time) {
	for s.upgradeDwell() > upgradeAfter && now.Sub(s.dwellAt) >= flapWindow {
		s.dwell = max(s.upgradeDwell()/2, upgradeAfter)
		s.dwellAt = s.dwellAt.Add(flapWindow)
	}
}

type videoCandidate struct {
	slot         *DownTrack
	state        *slotAllocation
	options      []int
	rates        []int64
	choice       int
	half         bool
	held         bool
	kept         bool
	temporalKept bool
	reason       string
	previous     int
	layered      []bool
	temporal     int
	capped       bool
	priority     int
}

func levelOf(choice int, half bool) int {
	if choice < 0 {
		return -1
	}
	if half {
		return 2 * choice
	}
	return 2*choice + 1
}

func (v *videoCandidate) level() int    { return levelOf(v.choice, v.half) }
func (v *videoCandidate) maxLevel() int { return levelOf(len(v.options)-1, false) }

func (v *videoCandidate) setLevel(level int) {
	switch {
	case level < 0:
		v.choice, v.half = -1, false
	case level%2 == 0:
		v.choice, v.half = level/2, true
	default:
		v.choice, v.half = level/2, false
	}
}

func (v *videoCandidate) lowestLevel() int {
	if v.layered[0] {
		return 0
	}
	return 1
}

func (v *videoCandidate) rateAt(level int) int64 {
	switch {
	case level < 0:
		return 0
	case level%2 == 0 && v.layered[level/2]:
		return v.rates[level/2] / 2
	default:
		return v.rates[level/2]
	}
}

func (v *videoCandidate) levelOfLayer(spatial int, half bool) int {
	if spatial < 0 {
		return -1
	}
	index := -1
	for position, option := range v.options {
		if option <= spatial {
			index = position
		}
	}
	if index < 0 {
		return v.lowestLevel()
	}
	return levelOf(index, half && v.options[index] == spatial && v.layered[index])
}

func (v *videoCandidate) layerOf(level int) (spatial int, half bool) {
	if level < 0 {
		return -1, false
	}
	return v.options[level/2], level%2 == 0
}

func (v *videoCandidate) nextStep(level int) int64 {
	switch {
	case level < 0:
		return v.rateAt(v.lowestLevel())
	case level%2 == 0:
		return v.rates[level/2] - v.rateAt(level)
	}
	return v.rateAt(level+2) - v.rateAt(level)
}

func (c *subscriberConn) currentEstimate() int64 {
	if c.api != nil {
		if estimator := c.api.Estimator(); estimator != nil {
			value := int64(estimator.GetTargetBitrate())
			c.estimate.Store(value)
			return value
		}
	}
	return c.estimate.Load()
}

func (c *subscriberConn) currentLoss() float64 {
	if c.api != nil {
		if estimator := c.api.Estimator(); estimator != nil {
			if loss, ok := estimator.GetStats()["averageLoss"].(float64); ok {
				c.loss.Store(math.Float64bits(loss))
			}
		}
	}
	return math.Float64frombits(c.loss.Load())
}

func (c *subscriberConn) allocateOnce() { c.allocateAt(time.Now()) }

func (c *subscriberConn) allocateAt(now time.Time) {
	if c.closed.Load() {
		return
	}
	c.allocMu.Lock()
	defer c.allocMu.Unlock()
	if c.allocStates == nil {
		c.allocStates = map[*DownTrack]*slotAllocation{}
	}
	slots := c.slotsSnapshot()
	estimate := c.currentEstimate()
	loss := c.currentLoss()
	capacity := estimate
	if capacity <= 0 {
		capacity = gccInitialBitrate
	}
	budget := int64(float64(capacity) * budgetHeadroom)
	smoothed, smoothedLoss := c.smoothEstimate(now, capacity, loss)
	lossy := smoothedLoss > keepLoss || loss > probeLoss
	var audio int64
	var candidates []*videoCandidate
	live := map[*DownTrack]bool{}
	for _, slot := range slots {
		track := slot.assigned()
		if track == nil {
			continue
		}
		if slot.kind == webrtc.RTPCodecTypeAudio {
			rate := int64(audioDefaultBPS)
			if layers := track.Layers(); len(layers) > 0 {
				if measured := layers[0].rate.value(now); measured > 0 {
					rate = measured
				}
			}
			audio += rate
			continue
		}
		fixed, prefs := slot.adaptation()
		if fixed {
			continue
		}
		available := track.activeSpatials(now)
		if len(available) == 0 {
			if layers := track.Layers(); len(layers) > 0 {
				lowest := layers[0].spatial
				for _, layer := range layers {
					lowest = min(lowest, layer.spatial)
				}
				slot.setTarget(lowest, 2, "publisher")
			}
			continue
		}
		options, capped := c.layerOptions(track, available, prefs)
		if len(options) == 0 {
			continue
		}
		state := c.allocStates[slot]
		if state == nil {
			state = &slotAllocation{spatial: -1}
			c.allocStates[slot] = state
		}
		live[slot] = true
		candidate := &videoCandidate{slot: slot, state: state, options: options, choice: -1, temporal: 2, capped: capped}
		if prefs.maxTemporal >= 0 {
			candidate.temporal = prefs.maxTemporal
		}
		if track.info.Source == "screen" {
			candidate.priority = 1
		}
		for _, spatial := range options {
			rate := defaultLayerBPS[min(spatial, 2)]
			if layer := track.layerAt(spatial); layer != nil {
				if measured := layer.rate.value(now); measured > 0 {
					rate = measured
				}
			}
			candidate.rates = append(candidate.rates, rate)
		}
		for _, spatial := range options {
			layer := track.layerAt(spatial)
			candidate.layered = append(candidate.layered, layer != nil && layer.temporal.Load())
		}
		candidate.previous = -1
		if state.decided {
			candidate.previous = candidate.levelOfLayer(state.spatial, state.half)
		}
		candidates = append(candidates, candidate)
	}
	for slot := range c.allocStates {
		if !live[slot] {
			delete(c.allocStates, slot)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].priority > candidates[j].priority })

	remaining := budget - audio
	slack := capacity - budget
	held := false
	for _, candidate := range candidates {
		state := candidate.state
		state.decay(now)
		switch {
		case candidate.rates[0] <= remaining:
			candidate.choice = 0
			remaining -= candidate.rates[0]
			state.shortSince = time.Time{}
		case candidate.layered[0] && candidate.rateAt(0) <= remaining:
			candidate.choice, candidate.half = 0, true
			remaining -= candidate.rateAt(0)
			state.shortSince = time.Time{}
		default:
			if state.shortSince.IsZero() {
				state.shortSince = now
			}
			lowest := candidate.lowestLevel()
			cost := candidate.rateAt(lowest)
			if candidate.previous >= 0 && now.Sub(state.shortSince) < pauseAfter && loss <= probeLoss && cost <= remaining+slack {
				candidate.setLevel(lowest)
				candidate.held = true
				remaining -= cost
				held = true
			}
		}
	}

	used := audio
	for _, candidate := range candidates {
		used += candidate.rateAt(candidate.level())
	}
	upgradeBudget := min(int64(float64(smoothed)*budgetHeadroom), int64(float64(capacity)*keepOverEstimate))
	if lossy {
		upgradeBudget = min(upgradeBudget, budget)
	}
	upgrades := upgradeBudget - used
	for upgraded := true; upgraded; {
		upgraded = false
		for _, candidate := range candidates {
			if candidate.choice < 0 || candidate.half || candidate.choice+1 >= len(candidate.options) {
				continue
			}
			delta := candidate.rates[candidate.choice+1] - candidate.rates[candidate.choice]
			if delta <= upgrades {
				candidate.choice++
				upgrades -= delta
				upgraded = true
			}
		}
	}

	for _, candidate := range candidates {
		state, want, previous := candidate.state, candidate.level(), candidate.previous
		if !state.decided || previous < 0 || want <= previous {
			if !state.upSince.IsZero() {
				if state.missedSince.IsZero() {
					state.missedSince = now
				}
				if now.Sub(state.missedSince) >= upgradeGrace {
					state.upSince, state.missedSince = time.Time{}, time.Time{}
				}
			}
			continue
		}
		state.missedSince = time.Time{}
		if state.upSince.IsZero() {
			state.upSince = now
		}
		dwell := state.upgradeDwell()
		sameLayer := want/2 == previous/2
		if sameLayer {
			dwell = upgradeAfter
		}
		if now.Sub(state.upSince) < dwell {
			candidate.setLevel(previous)
			continue
		}
		state.upSince = time.Time{}
		candidate.reason = "upgrade"
		if !sameLayer {
			state.upgradedAt = now
		}
	}

	used = audio
	for _, candidate := range candidates {
		used += candidate.rateAt(candidate.level())
	}
	keepBound := int64(float64(capacity) * keepOverEstimate)
	kept := false
	for _, candidate := range candidates {
		state, current, target := candidate.state, candidate.level(), candidate.previous
		if target <= current {
			continue
		}
		if current < 0 {
			candidate.reason = "budget"
			if loss > probeLoss {
				candidate.reason = "loss"
			}
			continue
		}
		short := func(level int) string {
			switch need := used + candidate.rateAt(level) - candidate.rateAt(current); {
			case lossy:
				return "loss"
			case need > keepBound:
				return "deep"
			case need > smoothed:
				return "smoothed"
			}
			return ""
		}
		keep := func(level int, temporal bool) {
			used += candidate.rateAt(level) - candidate.rateAt(current)
			candidate.setLevel(level)
			candidate.kept, candidate.temporalKept, kept = true, temporal, true
		}
		reason := short(target)
		switch {
		case !state.temporalSince.IsZero() && target%2 == 0:
			if reason == "" && now.Sub(state.temporalSince) >= pauseAfter {
				reason = "temporal_expired"
			}
			if reason == "" {
				keep(target, true)
			}
		case reason == "":
			keep(target, false)
		case target%2 == 1 && candidate.layered[target/2] && target-1 > current && short(target-1) == "":
			state.temporalSince = now
			keep(target-1, true)
		}
		if !candidate.kept || candidate.temporalKept && candidate.level() != target {
			candidate.reason = reason
		}
	}

	var step int64
	deficient := held || kept
	for _, candidate := range candidates {
		if want := candidate.level(); want < candidate.maxLevel() {
			deficient = true
			if next := candidate.nextStep(want); step == 0 || next < step {
				step = next
			}
		}
	}
	goal := int64(0)
	if deficient {
		if held || kept {
			step = 0
		}
		goal = int64(math.Ceil(float64(used+step) / budgetHeadroom))
	}

	media := audio
	for _, candidate := range candidates {
		state, final := candidate.state, candidate.level()
		if !candidate.temporalKept {
			state.temporalSince = time.Time{}
		}
		spatial, half := candidate.layerOf(final)
		previousSpatial, previousHalf, decided := state.spatial, state.half, state.decided
		if decided && previousSpatial >= 0 && spatial >= 0 && spatial < previousSpatial && final < candidate.maxLevel() {
			state.downgraded(now)
		}
		media += candidate.rateAt(final)
		state.spatial, state.half, state.decided = spatial, half, true
		if candidate.choice < 0 {
			candidate.slot.setTarget(-1, 0, "bandwidth")
		} else {
			temporal := candidate.temporal
			if candidate.half {
				temporal = 0
			}
			reason := "bandwidth"
			if candidate.choice == len(candidate.options)-1 && !candidate.half {
				reason = "publisher"
				if candidate.capped {
					reason = "request"
				}
			}
			candidate.slot.setTarget(candidate.options[candidate.choice], temporal, reason)
		}
		if !decided || previousSpatial != spatial || previousHalf != half {
			switch {
			case !decided:
				candidate.reason = "first"
			case previousSpatial < 0:
				candidate.reason = "resume"
			case candidate.reason == "" && spatial >= 0 && spatial < previousSpatial && final == candidate.maxLevel():
				candidate.reason = "options"
			case candidate.reason == "":
				candidate.reason = "budget"
			}
			state.reason = candidate.reason
			c.logDecision(now, candidate, previousSpatial, previousHalf, decided, estimate, smoothed, budget, audio, loss, smoothedLoss)
		}
	}
	probeEvent := c.probe.update(c, now, estimate, loss, goal, media)
	c.logAllocation(now, estimate, smoothed, budget, audio, media, goal, len(candidates), kept, probeEvent)
}

func (c *subscriberConn) smoothEstimate(now time.Time, estimate int64, loss float64) (int64, float64) {
	switch elapsed := now.Sub(c.smoothedAt); {
	case c.smoothedAt.IsZero() || elapsed > 10*estimateSmoothing:
		c.smoothed, c.smoothedLoss = float64(estimate), loss
	case elapsed > 0:
		weight := 1 - math.Exp(-elapsed.Seconds()/estimateSmoothing.Seconds())
		c.smoothed += weight * (float64(estimate) - c.smoothed)
		c.smoothedLoss += weight * (loss - c.smoothedLoss)
	}
	c.smoothedAt = now
	return int64(c.smoothed), c.smoothedLoss
}

func (v *videoCandidate) layerName(spatial int, half bool) string {
	if spatial < 0 {
		return "paused"
	}
	temporal := v.temporal
	if half {
		temporal = 0
	}
	return fmt.Sprintf("s%dt%d", spatial, temporal)
}

func (c *subscriberConn) logDecision(now time.Time, candidate *videoCandidate, fromSpatial int, fromHalf, decided bool, estimate, smoothed, budget, audio int64,
	loss, smoothedLoss float64) {
	if !c.allocLog.lastDecision.IsZero() && now.Sub(c.allocLog.lastDecision) < allocationDecisionGap {
		c.allocLog.suppressed++
		return
	}
	c.allocLog.lastDecision = now
	s := c.session
	track := ""
	if assigned := candidate.slot.assigned(); assigned != nil {
		track = assigned.info.TrackID
	}
	toSpatial, toHalf := candidate.layerOf(candidate.level())
	if !decided {
		fromSpatial, fromHalf = -1, false
	}
	event := "switch"
	switch {
	case toSpatial < 0:
		event = "pause"
	case fromSpatial < 0:
		event = "resume"
	}
	short := ""
	if !candidate.state.shortSince.IsZero() {
		short = now.Sub(candidate.state.shortSince).Round(100 * time.Millisecond).String()
	}
	s.opts.Logf("sfu: session %s: whep %s layer event=%s slot=%s track=%s from=%s to=%s reason=%s held=%t kept=%t short_for=%s estimate_bps=%d smoothed_bps=%d budget_bps=%d audio_bps=%d lowest_bps=%d loss=%.3f smoothed_loss=%.3f dwell=%s suppressed=%d",
		s.opts.SessionID, c.id, event, candidate.slot.id, track, candidate.layerName(fromSpatial, fromHalf), candidate.layerName(toSpatial, toHalf),
		candidate.reason, candidate.held, candidate.kept, short, estimate, smoothed, budget, audio, candidate.rates[0], loss, smoothedLoss,
		candidate.state.upgradeDwell(), c.allocLog.suppressed)
	c.allocLog.suppressed = 0
}

func (c *subscriberConn) logAllocation(now time.Time, estimate, smoothed, budget, audio, media, goal int64, videos int, kept bool, probeEvent string) {
	short := goal > 0
	changed := short != c.allocLog.short
	c.allocLog.short = short
	_, probing := c.probe.snapshot()
	event := ""
	switch {
	case probeEvent != "":
		if !c.allocLog.lastProbe.IsZero() && now.Sub(c.allocLog.lastProbe) < probeLogGap {
			c.allocLog.probeSuppressed++
			return
		}
		c.allocLog.lastProbe = now
		event = probeEvent
	case videos == 0:
		return
	case changed, (short || probing) && (c.allocLog.lastStatus.IsZero() || now.Sub(c.allocLog.lastStatus) >= allocationStatusEvery):
		event = "status"
	default:
		return
	}
	c.allocLog.lastStatus = now
	s := c.session
	padding, probing := c.probe.snapshot()
	var plis int64
	for _, slot := range c.slotsSnapshot() {
		plis += slot.takeSuppressedKeyframes()
	}
	s.opts.Logf("sfu: session %s: whep %s bwe event=%s estimate_bps=%d smoothed_bps=%d budget_bps=%d audio_bps=%d media_bps=%d goal_bps=%d short=%t kept=%t probing=%t padding_bytes=%d video_slots=%d probe_events_suppressed=%d plis_suppressed=%d",
		s.opts.SessionID, c.id, event, estimate, smoothed, budget, audio, media, goal, short, kept, probing, padding, videos, c.allocLog.probeSuppressed, plis)
	if event != "status" {
		c.allocLog.probeSuppressed = 0
	}
}

func (c *subscriberConn) layerOptions(track *Track, available []int, prefs slotPrefs) ([]int, bool) {
	limit, capped := 2, false
	if prefs.maxSpatial >= 0 {
		limit, capped = prefs.maxSpatial, true
	}
	if prefs.width > 0 || prefs.height > 0 {
		for _, spatial := range available {
			layer := track.layerAt(spatial)
			if layer == nil {
				continue
			}
			width, height := int64(layer.width.Load()), int64(layer.height.Load())
			if width == 0 || height == 0 {
				continue
			}
			if width >= prefs.width && height >= prefs.height {
				if spatial < limit {
					limit, capped = spatial, true
				}
				break
			}
		}
	}
	options := make([]int, 0, len(available))
	for _, spatial := range available {
		if spatial <= limit {
			options = append(options, spatial)
		}
	}
	if len(options) == 0 && len(available) > 0 {
		options = append(options, available[0])
	}
	return options, capped
}

func (c *subscriberConn) sampleUsage(now time.Time) {
	slots := c.slotsSnapshot()
	best := usage.LayerNone
	audio := false
	for _, slot := range slots {
		spatial, layers, video, active := slot.forwarding(now)
		if !active {
			continue
		}
		if !video {
			audio = true
			continue
		}
		level := usage.LayerHigh
		switch {
		case layers >= 3 && spatial == 0:
			level = usage.LayerLow
		case layers >= 3 && spatial == 1:
			level = usage.LayerMedium
		case layers == 2 && spatial == 0:
			level = usage.LayerLow
		}
		if level > best {
			best = level
		}
	}
	if best == usage.LayerNone && audio {
		best = usage.LayerAudioOnly
	}
	c.usage.SetLayer(best)
}
