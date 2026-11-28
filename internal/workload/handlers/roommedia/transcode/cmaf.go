package transcode

import (
	"bytes"
	"log"
	"sync"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/opus"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

type Fragment struct {
	Data        []byte
	Independent bool
	Duration    time.Duration
}

func samplesDuration(samples []*fmp4.Sample, rate int64) time.Duration {
	var ticks int64
	for _, sample := range samples {
		ticks += int64(sample.Duration)
	}
	if rate <= 0 {
		return 0
	}
	return time.Duration(ticks * int64(time.Second) / rate)
}

type output struct {
	id    string
	video bool

	mu        sync.Mutex
	init      []byte
	ready     chan struct{}
	recent    []Fragment
	bytes     int
	subs      map[*outputSub]struct{}
	delivered bool
	rerun     bool
	encoded   func(time.Duration)
}

type outputSub struct {
	queue chan Fragment
	done  chan struct{}
	once  sync.Once
}

func (s *outputSub) close() { s.once.Do(func() { close(s.done) }) }

func newOutput(id string, video bool) *output {
	return &output{id: id, video: video, ready: make(chan struct{}), subs: map[*outputSub]struct{}{}}
}

func (o *output) setInit(init []byte) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.init == nil {
		o.init = init
		close(o.ready)
		return true
	}
	if !bytes.Equal(o.init, init) {
		log.Printf("ERROR media transcode rendition %s: the encoder produced other parameter sets than the session's init segment; its fragments are dropped", o.id)
		return false
	}
	return true
}

func (o *output) reinit(init []byte) {
	o.mu.Lock()
	if o.init == nil {
		o.init = init
		close(o.ready)
		o.mu.Unlock()
		return
	}
	if bytes.Equal(o.init, init) {
		o.mu.Unlock()
		return
	}
	o.init = init
	o.recent, o.bytes = nil, 0
	o.rerun = false
	slow := o.deliver(Fragment{Data: init})
	o.mu.Unlock()
	for _, sub := range slow {
		sub.close()
	}
}

func (o *output) newRun() {
	o.mu.Lock()
	o.rerun = o.delivered
	o.mu.Unlock()
}

func (o *output) push(fragment Fragment) {
	if o.encoded != nil {
		o.encoded(fragment.Duration)
	}
	o.mu.Lock()
	var slow []*outputSub
	if o.rerun && o.init != nil {
		o.rerun = false
		o.recent, o.bytes = nil, 0
		slow = o.deliver(Fragment{Data: o.init})
	}
	o.delivered = true
	if fragment.Independent || !o.video {
		if o.video || len(o.recent) >= 8 {
			o.recent, o.bytes = nil, 0
		}
	}
	if o.bytes < 64<<20 {
		o.recent = append(o.recent, fragment)
		o.bytes += len(fragment.Data)
	}
	slow = append(slow, o.deliver(fragment)...)
	o.mu.Unlock()
	for _, sub := range slow {
		sub.close()
	}
}

func (o *output) deliver(fragment Fragment) []*outputSub {
	var slow []*outputSub
	for sub := range o.subs {
		select {
		case sub.queue <- fragment:
		default:
			slow = append(slow, sub)
			delete(o.subs, sub)
		}
	}
	return slow
}

func (o *output) subscribe(done <-chan struct{}) ([]byte, *outputSub) {
	select {
	case <-o.ready:
	case <-done:
		return nil, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	sub := &outputSub{queue: make(chan Fragment, 1024), done: make(chan struct{})}
	for _, fragment := range o.recent {
		sub.queue <- fragment
	}
	o.subs[sub] = struct{}{}
	return o.init, sub
}

func (o *output) unsubscribe(sub *outputSub) {
	o.mu.Lock()
	delete(o.subs, sub)
	o.mu.Unlock()
	sub.close()
}

func (o *output) closeAll() {
	o.mu.Lock()
	subs := o.subs
	o.subs = map[*outputSub]struct{}{}
	o.mu.Unlock()
	for sub := range subs {
		sub.close()
	}
}

func marshalInit(track *fmp4.InitTrack) ([]byte, error) {
	var buffer seekablebuffer.Buffer
	if err := (&fmp4.Init{Tracks: []*fmp4.InitTrack{track}}).Marshal(&buffer); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func marshalPart(sequence uint32, baseTime uint64, samples []*fmp4.Sample) ([]byte, error) {
	var buffer seekablebuffer.Buffer
	part := fmp4.Part{SequenceNumber: sequence, Tracks: []*fmp4.PartTrack{{ID: 1, BaseTime: baseTime, Samples: samples}}}
	if err := part.Marshal(&buffer); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

type videoFragmenter struct {
	out       *output
	h265      bool
	partTicks int64

	inited   bool
	accepted bool
	sequence uint32
	pending  *pendingSample
	samples  []*fmp4.Sample
	base     int64
	duration int64
	lastDur  int64
}

type pendingSample struct {
	dts    int64
	key    bool
	sample *fmp4.Sample
}

func (f *videoFragmenter) parameters(au [][]byte) (vps, sps, pps []byte) {
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		if f.h265 {
			switch h265.NALUType((nalu[0] >> 1) & 0x3f) {
			case h265.NALUType_VPS_NUT:
				vps = nalu
			case h265.NALUType_SPS_NUT:
				sps = nalu
			case h265.NALUType_PPS_NUT:
				pps = nalu
			}
			continue
		}
		switch h264.NALUType(nalu[0] & 0x1f) {
		case h264.NALUTypeSPS:
			sps = nalu
		case h264.NALUTypePPS:
			pps = nalu
		}
	}
	return vps, sps, pps
}

func (f *videoFragmenter) write(pts, dts int64, au [][]byte) {
	filtered := au[:0:0]
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		if !f.h265 && h264.NALUType(nalu[0]&0x1f) == h264.NALUTypeAccessUnitDelimiter {
			continue
		}
		if f.h265 && h265.NALUType((nalu[0]>>1)&0x3f) == h265.NALUType_AUD_NUT {
			continue
		}
		filtered = append(filtered, nalu)
	}
	if len(filtered) == 0 {
		return
	}
	var key bool
	if f.h265 {
		key = h265.IsRandomAccess(filtered)
	} else {
		key = h264.IsRandomAccess(filtered)
	}
	if !f.inited {
		if !key {
			return
		}
		vps, sps, pps := f.parameters(filtered)
		if sps == nil || pps == nil || (f.h265 && vps == nil) {
			return
		}
		var codec mp4codecs.Codec = &mp4codecs.H264{SPS: sps, PPS: pps}
		if f.h265 {
			codec = &mp4codecs.H265{VPS: vps, SPS: sps, PPS: pps}
		}
		init, err := marshalInit(&fmp4.InitTrack{ID: 1, TimeScale: 90000, Codec: codec})
		if err != nil {
			return
		}
		f.accepted = f.out.setInit(init)
		f.inited = true
	}
	payload, err := h264.AVCC(filtered).Marshal()
	if err != nil {
		return
	}
	sample := &fmp4.Sample{PTSOffset: int32(pts - dts), IsNonSyncSample: !key, Payload: payload}
	next := &pendingSample{dts: dts, key: key, sample: sample}
	if previous := f.pending; previous != nil {
		duration := dts - previous.dts
		if duration <= 0 || duration > 90000 {
			duration = max(f.lastDur, 1)
		}
		previous.sample.Duration = uint32(duration)
		f.lastDur = duration
		if len(f.samples) == 0 {
			f.base = previous.dts
		}
		f.samples = append(f.samples, previous.sample)
		f.duration += duration
		if key || f.duration+f.lastDur > f.partTicks {
			f.flush()
		}
	}
	f.pending = next
}

func (f *videoFragmenter) flush() {
	if len(f.samples) == 0 {
		return
	}
	f.sequence++
	data, err := marshalPart(f.sequence, uint64(max(f.base, 0)), f.samples)
	independent, duration := !f.samples[0].IsNonSyncSample, samplesDuration(f.samples, 90_000)
	f.samples, f.duration = nil, 0
	if err == nil && f.accepted {
		f.out.push(Fragment{Data: data, Independent: independent, Duration: duration})
	}
}

type audioFragmenter struct {
	out    *output
	opus   bool
	partMS int64

	rate     int64
	inited   bool
	accepted bool
	sequence uint32
	samples  []*fmp4.Sample
	base     int64
	next     int64
	haveNext bool
	duration int64
}

func (f *audioFragmenter) initAAC(config *mpeg4audio.AudioSpecificConfig) {
	if f.inited || config == nil {
		return
	}
	init, err := marshalInit(&fmp4.InitTrack{ID: 1, TimeScale: uint32(config.SampleRate), Codec: &mp4codecs.MPEG4Audio{Config: *config}})
	if err != nil {
		return
	}
	f.rate = int64(config.SampleRate)
	f.accepted = f.out.setInit(init)
	f.inited = true
}

func (f *audioFragmenter) initOpus(channels int) {
	if f.inited {
		return
	}
	init, err := marshalInit(&fmp4.InitTrack{ID: 1, TimeScale: 48000, Codec: &mp4codecs.Opus{ChannelCount: max(1, channels)}})
	if err != nil {
		return
	}
	f.rate = 48000
	f.accepted = f.out.setInit(init)
	f.inited = true
}

func (f *audioFragmenter) write(pts int64, frames [][]byte) {
	if !f.inited || f.rate == 0 {
		return
	}
	at := pts * f.rate / 90000
	if !f.haveNext || absInt(at-f.next) > f.rate/10 {
		f.flush()
		f.next, f.haveNext = at, true
	}
	limit := f.partMS * f.rate / 1000
	for _, frame := range frames {
		if len(frame) == 0 {
			continue
		}
		duration := int64(mpeg4audio.SamplesPerAccessUnit)
		if f.opus {
			duration = opus.PacketDuration2(frame)
			if duration <= 0 {
				continue
			}
		}
		if len(f.samples) > 0 && f.duration+duration > limit {
			f.flush()
		}
		if len(f.samples) == 0 {
			f.base = f.next
		}
		f.samples = append(f.samples, &fmp4.Sample{Duration: uint32(duration), Payload: append([]byte(nil), frame...)})
		f.duration += duration
		f.next += duration
	}
}

func (f *audioFragmenter) flush() {
	if len(f.samples) == 0 {
		return
	}
	f.sequence++
	data, err := marshalPart(f.sequence, uint64(max(f.base, 0)), f.samples)
	duration := samplesDuration(f.samples, f.rate)
	f.samples, f.duration = nil, 0
	if err == nil && f.accepted {
		f.out.push(Fragment{Data: data, Independent: true, Duration: duration})
	}
}

func absInt(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}
