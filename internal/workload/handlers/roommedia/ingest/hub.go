package ingest

import (
	"errors"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/substructs"
)

const (
	CodecH264 = "h264"
	CodecH265 = "h265"
	CodecAAC  = "aac"
	CodecOpus = "opus"
	CodecMP3  = "mp3"
)

type TrackInfo struct {
	Video        bool
	Codec        string
	AAC          *mpeg4audio.AudioSpecificConfig
	OpusChannels int
}

func (t TrackInfo) same(other TrackInfo) bool {
	if t.Video != other.Video || t.Codec != other.Codec || t.OpusChannels != other.OpusChannels {
		return false
	}
	if (t.AAC == nil) != (other.AAC == nil) {
		return false
	}
	return t.AAC == nil || (t.AAC.Type == other.AAC.Type && t.AAC.SampleRate == other.AAC.SampleRate &&
		t.AAC.ChannelConfig == other.AAC.ChannelConfig)
}

type Unit struct {
	Track int
	PTS   int64
	DTS   int64
	Key   bool
	AU    [][]byte
}

func (u Unit) size() int {
	size := 0
	for _, part := range u.AU {
		size += len(part)
	}
	return size
}

type Publisher struct {
	Protocol    string
	StreamKeyID *string
	TokenNonce  *string
	Identity    *string
	meter       *usage.Connection
	Close       func(reason string)

	offset     int64
	haveOffset bool
	keySeen    bool
	closeOnce  sync.Once
	reason     string
}

func (p *Publisher) Stop(reason string) {
	p.closeOnce.Do(func() {
		p.reason = reason
		if p.Close != nil {
			p.Close(reason)
		}
	})
}

func (p *Publisher) Reason() string { return p.reason }

const (
	gopMaxBytes   = 32 << 20
	gopMaxUnits   = 4096
	subQueueUnits = 8192
)

type Hub struct {
	mu        sync.Mutex
	tracks    []TrackInfo
	ready     chan struct{}
	gop       []Unit
	gopBytes  int
	subs      map[*Sub]struct{}
	current   *Publisher
	lastDTS   int64
	haveLast  bool
	keyNeeded func()
	grace     time.Duration
	graceEnd  *time.Timer
	waitKey   bool
}

func NewHub(grace time.Duration) *Hub {
	return &Hub{ready: make(chan struct{}), subs: map[*Sub]struct{}{}, grace: grace}
}

func (h *Hub) Current() *Publisher {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.current
}

func (h *Hub) Attach(p *Publisher, tracks []TrackInfo, keyNeeded func()) {
	h.mu.Lock()
	previous := h.current
	h.current = p
	h.keyNeeded = keyNeeded
	if h.graceEnd != nil {
		h.graceEnd.Stop()
		h.graceEnd = nil
	}
	changed := h.tracks != nil && !slices.EqualFunc(h.tracks, tracks, TrackInfo.same)
	var dropped []*Sub
	if changed {
		for sub := range h.subs {
			dropped = append(dropped, sub)
		}
		h.subs = map[*Sub]struct{}{}
		h.gop, h.gopBytes = nil, 0
	}
	first := h.tracks == nil
	h.tracks = append([]TrackInfo(nil), tracks...)
	if first {
		close(h.ready)
	}
	h.mu.Unlock()
	for _, sub := range dropped {
		sub.close()
	}
	if previous != nil && previous != p {
		previous.Stop("replaced")
	}
}

func (h *Hub) Detach(p *Publisher) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current != p {
		return false
	}
	h.current = nil
	h.keyNeeded = nil
	if h.grace > 0 {
		h.graceEnd = time.AfterFunc(h.grace, h.graceExpired)
	}
	return true
}

func (h *Hub) graceExpired() {
	h.mu.Lock()
	if h.current != nil {
		h.mu.Unlock()
		return
	}
	subs := h.subs
	h.subs = map[*Sub]struct{}{}
	h.gop, h.gopBytes = nil, 0
	h.graceEnd = nil
	h.mu.Unlock()
	for sub := range subs {
		sub.close()
	}
}

func (h *Hub) Write(p *Publisher, unit Unit) {
	h.mu.Lock()
	if h.current != p || unit.Track < 0 || unit.Track >= len(h.tracks) || len(unit.AU) == 0 {
		h.mu.Unlock()
		return
	}
	if !p.haveOffset {
		base := int64(0)
		if h.haveLast {
			base = h.lastDTS + 3000
		}
		p.offset = base + 90000 - unit.DTS
		p.haveOffset = true
	}
	unit.PTS += p.offset
	unit.DTS += p.offset
	if unit.DTS > h.lastDTS || !h.haveLast {
		h.lastDTS, h.haveLast = unit.DTS, true
	}
	track := h.tracks[unit.Track]
	if h.waitKey && track.Video && !p.keySeen {
		if !unit.Key {
			h.mu.Unlock()
			return
		}
		p.keySeen = true
	}
	hasVideo := slices.ContainsFunc(h.tracks, func(t TrackInfo) bool { return t.Video })
	switch {
	case track.Video && unit.Key:
		h.gop, h.gopBytes = []Unit{unit}, unit.size()
	case !hasVideo && len(h.gop) >= 50:
		h.gop, h.gopBytes = append(h.gop[1:], unit), h.gopBytes+unit.size()
	case len(h.gop) > 0 && len(h.gop) < gopMaxUnits && h.gopBytes < gopMaxBytes:
		h.gop, h.gopBytes = append(h.gop, unit), h.gopBytes+unit.size()
	case !hasVideo:
		h.gop, h.gopBytes = []Unit{unit}, unit.size()
	}
	var slow []*Sub
	for sub := range h.subs {
		if !sub.push(unit) {
			slow = append(slow, sub)
			delete(h.subs, sub)
		}
	}
	h.mu.Unlock()
	for _, sub := range slow {
		sub.close()
	}
}

type Sub struct {
	tracks []TrackInfo
	queue  chan Unit
	done   chan struct{}
	once   sync.Once
}

func (s *Sub) push(unit Unit) bool {
	select {
	case s.queue <- unit:
		return true
	default:
		return false
	}
}

func (s *Sub) close() { s.once.Do(func() { close(s.done) }) }

func (h *Hub) Subscribe(done <-chan struct{}) *Sub {
	select {
	case <-h.ready:
	case <-done:
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	sub := &Sub{tracks: append([]TrackInfo(nil), h.tracks...), queue: make(chan Unit, subQueueUnits), done: make(chan struct{})}
	for _, unit := range h.gop {
		if !sub.push(unit) {
			break
		}
	}
	h.subs[sub] = struct{}{}
	if len(h.gop) == 0 && h.keyNeeded != nil {
		go h.keyNeeded()
	}
	return sub
}

func (h *Hub) Unsubscribe(sub *Sub) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
	sub.close()
}

func (h *Hub) CloseAll() {
	h.mu.Lock()
	subs := h.subs
	h.subs = map[*Sub]struct{}{}
	h.mu.Unlock()
	for sub := range subs {
		sub.close()
	}
}

func (h *Hub) Children() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

var errSlowChild = errors.New("child is too slow")

func (s *Sub) WriteTS(w io.Writer, flush func(), done <-chan struct{}) error {
	tracks := make([]*mpegts.Track, len(s.tracks))
	for i, info := range s.tracks {
		track := &mpegts.Track{PID: uint16(0x100 + i)}
		switch info.Codec {
		case CodecH264:
			track.Codec = &tscodecs.H264{}
		case CodecH265:
			track.Codec = &tscodecs.H265{}
		case CodecAAC:
			track.Codec = &tscodecs.MPEG4Audio{Config: *info.AAC}
		case CodecOpus:
			track.Codec = &tscodecs.Opus{Desc: &substructs.OpusAudioDescriptor{ChannelConfigCode: uint8(max(1, info.OpusChannels))}}
		case CodecMP3:
			track.Codec = &tscodecs.MPEG1Audio{}
		default:
			return errors.New("unsupported source codec")
		}
		tracks[i] = track
	}
	writer := &mpegts.Writer{W: w, Tracks: tracks}
	if err := writer.Initialize(); err != nil {
		return err
	}
	if _, err := writer.WriteTables(); err != nil {
		return err
	}
	flush()
	waitKey := false
	for _, info := range s.tracks {
		waitKey = waitKey || info.Video
	}
	for {
		select {
		case <-done:
			return nil
		case <-s.done:
			return errSlowChild
		case unit := <-s.queue:
			info := s.tracks[unit.Track]
			if waitKey {
				if !info.Video || !unit.Key {
					continue
				}
				waitKey = false
			}
			var err error
			track := tracks[unit.Track]
			switch info.Codec {
			case CodecH264:
				err = writer.WriteH264(track, unit.PTS, unit.DTS, unit.AU)
			case CodecH265:
				err = writer.WriteH265(track, unit.PTS, unit.DTS, unit.AU)
			case CodecAAC:
				err = writer.WriteMPEG4Audio(track, unit.PTS, unit.AU)
			case CodecOpus:
				err = writer.WriteOpus(track, unit.PTS, unit.AU)
			case CodecMP3:
				err = writer.WriteMPEG1Audio(track, unit.PTS, unit.AU)
			}
			if err != nil {
				return err
			}
			if len(s.queue) == 0 {
				flush()
			}
		}
	}
}

func flusher(response http.ResponseWriter) func() {
	controller := http.NewResponseController(response)
	return func() { _ = controller.Flush() }
}
