package packager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

func (r *rendition) addFragment(data []byte) error {
	r.mu.Lock()
	timescale := r.timescale
	r.mu.Unlock()
	if timescale == 0 {
		return errors.New("fragment before the init segment")
	}
	var parts fmp4.Parts
	if err := parts.Unmarshal(data); err != nil {
		return err
	}
	if len(parts) == 0 || len(parts[0].Tracks) == 0 || len(parts[0].Tracks[0].Samples) == 0 {
		return nil
	}
	track := parts[0].Tracks[0]
	var ticks uint64
	for _, sample := range track.Samples {
		ticks += uint64(sample.Duration)
	}
	start := float64(track.BaseTime) / float64(timescale)
	duration := float64(ticks) / float64(timescale)
	independent := !track.Samples[0].IsNonSyncSample
	if r.key != nil {
		protected, err := encryptFragment(data, r.key, r.video, r.h265)
		if err != nil {
			return err
		}
		data = protected
	}
	timeIndex := int64(math.Floor(start*1000/float64(r.segmentMS) + 0.25))
	fragment := pendingFragment{data: data, base: track.BaseTime, timescale: timescale, start: start, duration: duration,
		independent: independent, timeIndex: timeIndex}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.follow == nil {
		r.place(fragment)
		return nil
	}
	r.waiting = append(r.waiting, fragment)
	r.drain(false)
	return nil
}

type pendingFragment struct {
	data        []byte
	base        uint64
	timescale   uint32
	start       float64
	duration    float64
	independent bool
	timeIndex   int64
}

func (r *rendition) drain(flush bool) {
	videoEnd := r.follow.reached()
	waited := 0.0
	for _, fragment := range r.waiting {
		waited += fragment.duration
	}
	for len(r.waiting) > 0 {
		head := r.waiting[0]
		if !flush && head.start+float64(aacHalfFrame)/float64(max(head.timescale, 1)) >= videoEnd &&
			waited <= float64(r.targetDuration()) {
			return
		}
		r.waiting = r.waiting[1:]
		waited -= head.duration
		r.place(head)
	}
	r.waiting = nil
}

func (r *rendition) place(fragment pendingFragment) {
	data, start, duration, independent, timeIndex := fragment.data, fragment.start, fragment.duration, fragment.independent, fragment.timeIndex
	if r.resync {
		if last := r.last(); last != nil {
			if start < last.end-resyncGapS && last.end-start <= 2*float64(r.targetDuration()) {
				return
			}
			if start > last.end+resyncGapS && r.follow == nil {
				if open := r.current(); open != nil {
					r.complete(open)
				}
				r.restarted = true
				log.Printf("media package rendition %s: %.3f s lost across a pull reconnect: the next segment starts with "+
					"EXT-X-DISCONTINUITY", r.id, start-last.end)
			}
		}
		r.resync = false
	}
	current := r.current()
	previous := r.last()
	discontinuity := current != nil && math.Abs(start-current.end) > 1.0
	overTarget := current != nil && current.duration+duration > float64(r.targetDuration())
	boundary := current != nil && independent && timeIndex > current.timeIndex
	followed := false
	var followedStart segmentStart
	if r.follow != nil && current != nil {
		next, known, _ := r.follow.after(current.timeIndex)
		boundary = known && next.reachedBy(fragment.base, fragment.timescale)
		if boundary {
			timeIndex, followed, followedStart = next.index, true, next
		}
		discontinuity = false
	}
	if current == nil || discontinuity || overTarget || boundary {
		if current == nil && !independent && r.video {
			return
		}
		if current != nil {
			r.complete(current)
		}
		if previous != nil && math.Abs(start-previous.end) > 1.0 && r.follow == nil {
			discontinuity = true
		}
		if followed {
			discontinuity = followedStart.discontinuity
		}
		if overTarget && !independent && r.video {
			discontinuity = true
		}
		current = &segment{init: r.currentInit(), msn: r.nextMSN, timeIndex: timeIndex, discontinuity: discontinuity || r.newInit || r.restarted}
		if r.starts != nil {
			r.starts.add(segmentStart{base: fragment.base, timescale: fragment.timescale, index: timeIndex, discontinuity: current.discontinuity})
		}
		r.newInit, r.restarted = false, false
		if r.onMSN != nil {
			r.onMSN(r, current.msn)
		}
		r.nextMSN++
		r.segments = append(r.segments, current)
	}
	current.parts = append(current.parts, &part{index: len(current.parts), data: data, sha: sha256Hex(data),
		duration: duration, independent: independent})
	current.duration += duration
	current.end = start + duration
	if r.starts != nil {
		r.starts.reach(current.end)
	}
	r.notify()
}

var errNotReady = errors.New("the rendition has no complete segment yet")

func (r *rendition) started() bool {
	for _, seg := range r.segments {
		if seg.complete {
			return true
		}
	}
	return false
}

func (r *rendition) hasStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started()
}

func (r *rendition) current() *segment {
	if len(r.segments) == 0 {
		return nil
	}
	if last := r.segments[len(r.segments)-1]; !last.complete {
		return last
	}
	return nil
}

func (r *rendition) last() *segment {
	if len(r.segments) == 0 {
		return nil
	}
	return r.segments[len(r.segments)-1]
}

func (r *rendition) complete(seg *segment) {
	var buffer bytes.Buffer
	for _, p := range seg.parts {
		buffer.Write(p.data)
	}
	seg.data = buffer.Bytes()
	seg.sha, seg.bytes, seg.complete = sha256Hex(seg.data), int64(len(seg.data)), true
	if r.dir != "" {
		seg.path = filepath.Join(r.dir, fmt.Sprintf("seg_%d.m4s", seg.msn))
		if err := os.WriteFile(seg.path, seg.data, 0o600); err != nil {
			_ = os.Remove(seg.path)
			seg.path = ""
			if !r.storeFailed {
				r.storeFailed = true
				log.Printf("media package rendition %s: DVR store write failed (%v): segments stay in memory and the DVR window "+
					"shrinks to %d of them until the store accepts writes again", r.id, err, memoryWindowSegments)
			}
		} else {
			r.storeFailed = false
		}
	}
	if r.published != nil {
		r.published(r, time.Duration(seg.duration*float64(time.Second)))
	}
	if r.onSegment != nil {
		r.onSegment(r, seg)
	}
	r.evict()
}

func (r *rendition) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.follow != nil {
		r.drain(true)
	}
	if current := r.current(); current != nil && len(current.parts) > 0 {
		r.complete(current)
		r.notify()
	}
}

func (r *rendition) evict() {
	completeSeen := 0
	for i := len(r.segments) - 1; i >= 0; i-- {
		seg := r.segments[i]
		if !seg.complete {
			continue
		}
		completeSeen++
		if completeSeen > partRetentionSegments {
			seg.parts = nil
		}
		if completeSeen > memorySegments && seg.path != "" {
			seg.data = nil
		}
	}
	window := float64(r.dvrS)
	var kept float64
	cut := 0
	for i := len(r.segments) - 1; i >= 0; i-- {
		kept += r.segments[i].duration
		if kept > window && len(r.segments)-i > minimumSegments {
			cut = i
			break
		}
	}
	memoryOnly := 0
	for i := len(r.segments) - 1; i >= cut; i-- {
		if seg := r.segments[i]; seg.complete && seg.path == "" {
			memoryOnly++
			if memoryOnly > memoryWindowSegments {
				cut = i + 1
				if !r.limited {
					r.limited = true
					log.Printf("media package rendition %s: DVR window limited to %d segments held in memory (dvr_window_s %d): "+
						"the packager has no DVR store on disk (--media-cache-dir) or cannot write to it", r.id, memoryWindowSegments, r.dvrS)
				}
				break
			}
		}
	}
	for _, seg := range r.segments[:cut] {
		if seg.discontinuity {
			r.discSeq++
		}
		if seg.path != "" {
			_ = os.Remove(seg.path)
		}
	}
	r.segments = append([]*segment(nil), r.segments[cut:]...)
	r.pruneInits()
}

func (r *rendition) notify() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *rendition) satisfied(msn, part int64) bool {
	last := r.last()
	if last == nil {
		return false
	}
	if last.msn > msn {
		return true
	}
	if last.msn < msn {
		return false
	}
	if part < 0 {
		return last.complete
	}
	return int64(len(last.parts)) > part || last.complete
}

func (r *rendition) satisfiedNow(msn, part int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.satisfied(msn, part)
}

func (r *rendition) wait(ctx context.Context, msn, part int64, limit time.Duration) bool {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	for {
		r.mu.Lock()
		ok := r.satisfied(msn, part)
		changed := r.changed
		r.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-changed:
		}
	}
}

func (r *rendition) tooFar(msn int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return msn > r.nextMSN+1
}
