package sfu

import (
	"sort"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const (
	speechWindow   = 300 * time.Millisecond
	dominantWindow = 1500 * time.Millisecond
	speechLevel    = 70
	silenceLevel   = 127
	dominantHold   = 2 * time.Second
	speakerTick    = 100 * time.Millisecond
	maxSpeakers    = 16
)

type levelSample struct {
	at    int64
	level int
}

type levelMeter struct {
	mu      sync.Mutex
	samples []levelSample
}

func (m *levelMeter) observe(now time.Time, level int, _ bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	nanos := now.UnixNano()
	m.samples = append(m.samples, levelSample{at: nanos, level: level})
	cut := 0
	for cut < len(m.samples) && nanos-m.samples[cut].at > int64(dominantWindow) {
		cut++
	}
	if cut > 0 {
		m.samples = append(m.samples[:0], m.samples[cut:]...)
	}
}

func (m *levelMeter) mean(now time.Time, window time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	nanos := now.UnixNano()
	total, count := 0, 0
	for _, sample := range m.samples {
		if nanos-sample.at <= int64(window) {
			total += sample.level
			count++
		}
	}
	if count == 0 {
		return silenceLevel
	}
	return total / count
}

type speakerDetector struct {
	dominant     string
	since        time.Time
	last         []mediacontracts.Speaker
	lastDominant *string
}

func (d *speakerDetector) update(now time.Time, tracks []*Track) ([]mediacontracts.Speaker, *string, bool) {
	type candidate struct {
		speaker mediacontracts.Speaker
		long    int
	}
	candidates := make([]candidate, 0, len(tracks))
	for _, track := range tracks {
		if track.muted() {
			continue
		}
		level := track.level.mean(now, speechWindow)
		if level >= speechLevel {
			continue
		}
		candidates = append(candidates, candidate{speaker: mediacontracts.Speaker{ParticipantID: track.info.ParticipantID,
			TrackID: track.info.TrackID, LevelDBov: int64(level)}, long: track.level.mean(now, dominantWindow)})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].speaker.LevelDBov < candidates[j].speaker.LevelDBov })
	if len(candidates) > maxSpeakers {
		candidates = candidates[:maxSpeakers]
	}
	speakers := make([]mediacontracts.Speaker, 0, len(candidates))
	loudest, loudestLevel := "", silenceLevel
	currentSpeaking := false
	for _, candidate := range candidates {
		speakers = append(speakers, candidate.speaker)
		if candidate.long < loudestLevel {
			loudest, loudestLevel = candidate.speaker.ParticipantID, candidate.long
		}
		if candidate.speaker.ParticipantID == d.dominant {
			currentSpeaking = true
		}
	}
	switch {
	case loudest == "":
	case d.dominant == "" || !currentSpeaking && now.Sub(d.since) >= dominantHold/2:
		d.dominant, d.since = loudest, now
	case loudest != d.dominant && now.Sub(d.since) >= dominantHold:
		d.dominant, d.since = loudest, now
	}
	var dominant *string
	if d.dominant != "" {
		value := d.dominant
		dominant = &value
	}
	changed := !sameSpeakers(d.last, speakers) || !sameString(d.lastDominant, dominant)
	d.last, d.lastDominant = speakers, dominant
	return speakers, dominant, changed
}

func (d *speakerDetector) forget(participantID string) {
	if d.dominant == participantID {
		d.dominant = ""
	}
}

func sameSpeakers(left, right []mediacontracts.Speaker) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].TrackID != right[index].TrackID {
			return false
		}
		if delta := left[index].LevelDBov - right[index].LevelDBov; delta >= 6 || delta <= -6 {
			return false
		}
	}
	return true
}

func sameString(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
