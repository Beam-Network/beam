package sfu

import (
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type wireCounter struct {
	interceptor.NoOp
	total *atomic.Int64
}

type wireCounterFactory struct{ total *atomic.Int64 }

func (f wireCounterFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &wireCounter{total: f.total}, nil
}

func (w *wireCounter) BindLocalStream(_ *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		size := header.MarshalSize() + len(payload) + int(header.PaddingSize)
		written, err := writer.Write(header, payload, attributes)
		if err == nil {
			w.total.Add(int64(size))
		}
		return written, err
	})
}

const outageSendLimit = 5 * time.Second

func (c *subscriberConn) followOutage(connected bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outage != nil {
		c.outage.Stop()
		c.outage = nil
	}
	if connected {
		if c.suspended.Swap(false) {
			go func() {
				defer c.session.guard("whep resume", c.abort)
				for _, slot := range c.slotsSnapshot() {
					slot.requestKeyframe()
				}
			}()
		}
		return
	}
	if c.closed.Load() || !c.session.opts.Usage.V2() {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(outageSendLimit, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.outage == timer {
			c.suspended.Store(true)
		}
	})
	c.outage = timer
}

func simulcastScale(rid string) int64 {
	switch spatialOfRID(rid, 2) {
	case 0:
		return 4
	case 1:
		return 2
	}
	return 1
}

func (l *Layer) read() (width, height int64) {
	if width, height := l.meteredWidth.Load(), l.meteredHeight.Load(); width > 0 && height > 0 {
		return int64(width), int64(height)
	}
	return int64(l.width.Load()), int64(l.height.Load())
}

func (t *Track) size(layer *Layer) (width, height int64) {
	if width, height := layer.read(); width > 0 && height > 0 {
		return width, height
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, declared := range t.info.Layers {
		if declared.RID == layer.rid && declared.Width > 0 && declared.Height > 0 {
			return declared.Width, declared.Height
		}
	}
	for _, other := range t.layers {
		width, height := other.read()
		if width <= 0 || height <= 0 {
			for _, declared := range t.info.Layers {
				if declared.RID == other.rid {
					width, height = declared.Width, declared.Height
				}
			}
		}
		if other != layer && width > 0 && height > 0 {
			own, theirs := simulcastScale(layer.rid), simulcastScale(other.rid)
			return width * theirs / own, height * theirs / own
		}
	}
	return 0, 0
}

const maxSpatialLayers = 4

type trackForwards struct {
	track *Track
	at    [maxSpatialLayers]int64
}

func (d *DownTrack) noteForwarded(at int64) {
	if d.kind != webrtc.RTPCodecTypeVideo || d.current < 0 || d.current >= maxSpatialLayers {
		return
	}
	if d.forwards[0].track != d.track {
		d.forwards[1], d.forwards[0] = d.forwards[0], trackForwards{track: d.track}
	}
	d.forwards[0].at[d.current] = at
}

func (d *DownTrack) forwarded(since int64) string {
	track, spatial, last, forwards := func() (*Track, int, int64, [2]trackForwards) {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.track, d.current, d.lastForward, d.forwards
	}()
	switch {
	case last < since:
		return ""
	case d.kind == webrtc.RTPCodecTypeAudio:
		return mediacontracts.ClassAudio
	}
	best := ""
	for _, entry := range forwards {
		if entry.track == nil {
			continue
		}
		for index, at := range entry.at {
			if at < since {
				continue
			}
			if layer := entry.track.layerAt(index); layer != nil {
				best = mediacontracts.MostDemandingClass([]string{best, mediacontracts.VideoClass(entry.track.size(layer))})
			}
		}
	}
	if best != "" {
		return best
	}
	var layer *Layer
	if track != nil {
		layer = track.layerAt(spatial)
	}
	if layer == nil {
		return mediacontracts.ClassSD
	}
	return mediacontracts.VideoClass(track.size(layer))
}

func (c *subscriberConn) mediaClass(since time.Time) string {
	best := ""
	for _, slot := range c.slotsSnapshot() {
		if class := slot.forwarded(since.UnixNano()); class != "" {
			best = mediacontracts.MostDemandingClass([]string{best, class})
		}
	}
	return best
}

func (c *publisherConn) mediaClass(since time.Time) string {
	tracks := func() []*Track {
		c.mu.Lock()
		defer c.mu.Unlock()
		tracks := make([]*Track, 0, len(c.tracks))
		for _, track := range c.tracks {
			tracks = append(tracks, track)
		}
		return tracks
	}()
	best := ""
	for _, track := range tracks {
		for _, layer := range track.Layers() {
			if layer.lastPacket.Load() < since.UnixNano() {
				continue
			}
			class := mediacontracts.ClassAudio
			if track.kind == webrtc.RTPCodecTypeVideo {
				class = mediacontracts.VideoClass(track.size(layer))
			}
			best = mediacontracts.MostDemandingClass([]string{best, class})
		}
	}
	return best
}
