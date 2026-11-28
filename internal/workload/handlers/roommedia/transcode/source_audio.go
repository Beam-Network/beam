package transcode

import (
	"sync"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type sourceAudio struct {
	out         *output
	rate        int64
	silent      []byte
	partSamples int64

	mu       sync.Mutex
	started  bool
	source   bool
	inflight int64
	progress time.Time
	next     int64
	cuts     []int64
	samples  []*fmp4.Sample
	base     int64
	duration int64
	sequence uint32
}

const encoderStallLimit = 2 * time.Second

func newSourceAudio(out *output, rendition mc.AudioRendition, partMS int64) (*sourceAudio, error) {
	config := mpeg4audio.AudioSpecificConfig{Type: mpeg4audio.ObjectTypeAACLC, SampleRate: int(rendition.SampleRate),
		ChannelConfig: uint8(rendition.Channels)}
	init, err := marshalInit(&fmp4.InitTrack{ID: 1, TimeScale: uint32(rendition.SampleRate), Codec: &mp4codecs.MPEG4Audio{Config: config}})
	if err != nil {
		return nil, err
	}
	out.reinit(init)
	return &sourceAudio{out: out, rate: rendition.SampleRate, silent: silentAACFrame(rendition.Channels),
		partSamples: max(partMS*rendition.SampleRate/1000, aacFrameSamples)}, nil
}

func (a *sourceAudio) setSource(source bool) {
	a.mu.Lock()
	a.source = source
	if source {
		a.progress = time.Now()
	}
	a.mu.Unlock()
}

func (a *sourceAudio) handed(ticks int64) {
	a.mu.Lock()
	a.inflight = max(a.inflight, a.samplesAt(ticks))
	a.mu.Unlock()
}

func (a *sourceAudio) samplesAt(ticks int64) int64 { return floorDiv(ticks*a.rate, 90000) }

func (a *sourceAudio) cut(ticks int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	at := a.samplesAt(ticks)
	if !a.started {
		a.started, a.next = true, at
		return
	}
	if at > a.next {
		a.cuts = append(a.cuts, at)
	}
}

func (a *sourceAudio) advance(ticks int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.started {
		return
	}
	target := a.samplesAt(ticks)
	if a.source {
		target -= a.rate
		if a.next < a.inflight && time.Since(a.progress) < encoderStallLimit {
			return
		}
	}
	for a.next+aacFrameSamples <= target {
		a.emit(a.silent)
	}
}

func (a *sourceAudio) write(ticks int64, payload []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.progress = time.Now()
	if !a.started {
		return
	}
	at := a.samplesAt(ticks)
	if at+aacFrameSamples/2 <= a.next {
		return
	}
	if at-a.next > a.rate/10 {
		for at-a.next >= aacFrameSamples/2 {
			a.emit(a.silent)
		}
	}
	a.emit(append([]byte(nil), payload...))
}

func (a *sourceAudio) emit(payload []byte) {
	cutHere := false
	for len(a.cuts) > 0 && a.cuts[0] <= a.next+aacFrameSamples/2 {
		a.cuts = a.cuts[1:]
		cutHere = true
	}
	if len(a.samples) > 0 && (cutHere || a.duration+aacFrameSamples > a.partSamples) {
		a.flush()
	}
	if len(a.samples) == 0 {
		a.base = a.next
	}
	a.samples = append(a.samples, &fmp4.Sample{Duration: aacFrameSamples, Payload: payload})
	a.duration += aacFrameSamples
	a.next += aacFrameSamples
}

func (a *sourceAudio) flush() {
	if len(a.samples) == 0 {
		return
	}
	a.sequence++
	data, err := marshalPart(a.sequence, uint64(max(a.base, 0)), a.samples)
	duration := samplesDuration(a.samples, a.rate)
	a.samples, a.duration = nil, 0
	if err == nil {
		a.out.push(Fragment{Data: data, Independent: true, Duration: duration})
	}
}

func (a *sourceAudio) finish() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.flush()
}
