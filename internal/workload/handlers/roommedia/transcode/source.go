package transcode

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
)

var errSourceIncompatible = errors.New("source incompatible with the source ladder (no H.264 video): dropped")

const aacFrameSamples = mpeg4audio.SamplesPerAccessUnit

const audioFeedBias = 10 * 90000

const timestampWrap = int64(1) << 33

const maxFrameGapTicks = 90000

const audioHoldTicks = 3 * 90000

func unwrapNear(raw, ref int64) int64 {
	raw &= timestampWrap - 1
	value := ref - (ref & (timestampWrap - 1)) + raw
	switch {
	case value-ref > timestampWrap/2:
		value -= timestampWrap
	case ref-value > timestampWrap/2:
		value += timestampWrap
	}
	return value
}

func hasIDR(au [][]byte) bool {
	for _, nalu := range au {
		if len(nalu) > 0 && h264.NALUType(nalu[0]&0x1f) == h264.NALUTypeIDR {
			return true
		}
	}
	return false
}

func silentAACFrame(channels int64) []byte {
	if channels == 1 {
		return []byte{0x00, 0xc8, 0x00, 0x80, 0x23, 0x80}
	}
	return []byte{0x21, 0x00, 0x49, 0x90, 0x02, 0x19, 0x00, 0x23, 0x80}
}

func (r *Role) runSource(ctx context.Context, body io.Reader) error {
	reader := &mpegts.Reader{R: bufio.NewReaderSize(body, 1<<20)}
	if err := reader.Initialize(); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	reader.OnDecodeError(func(error) {})
	var videoTrack, audioTrack *mpegts.Track
	for _, track := range reader.Tracks() {
		switch track.Codec.(type) {
		case *tscodecs.H264:
			if videoTrack == nil {
				videoTrack = track
			}
		case *tscodecs.MPEG4Audio, *tscodecs.Opus, *tscodecs.MPEG1Audio:
			if audioTrack == nil {
				audioTrack = track
			}
		}
	}
	if videoTrack == nil {
		return errSourceIncompatible
	}
	origin := r.startRun()
	video := &sourceVideo{out: r.outputs[r.ladder.Video[0].RenditionID], segmentMS: r.segmentMS, partTicks: r.partMS * 90,
		origin: origin}
	var audio *sourceAudio
	if len(r.ladder.Audio) > 0 {
		rendition := r.ladder.Audio[0]
		var err error
		audio, err = newSourceAudio(r.outputs[rendition.RenditionID], rendition, r.partMS)
		if err != nil {
			return err
		}
		video.audio = audio
	}
	var encoder *audioEncoder
	if audio != nil && audioTrack != nil {
		encoder = newAudioEncoder(ctx, r.env.Config.Transcode, r.ladder.Audio[0], audioTrack, audio, r.nodeName())
	}
	type early struct {
		pts    int64
		frames [][]byte
	}
	var waiting, held []early
	video.audioSource = encoder != nil
	release := func() {
		for _, item := range held {
			if at, ok := video.audioTime(item.pts); ok {
				encoder.feed(at, item.frames)
			}
		}
		held = nil
		video.audioJumped = false
	}
	feed := func(pts int64, frames [][]byte) {
		if !video.started {
			if len(waiting) < 512 {
				waiting = append(waiting, early{pts: pts, frames: copyFrames(frames)})
			}
			return
		}
		source := unwrapNear(pts, video.lastDTS)
		jumped := video.haveAudio && (source-video.lastAudio > maxFrameGapTicks || video.lastAudio-source > maxFrameGapTicks/10)
		if jumped && !video.audioJumped && !(video.collapsedAt != 0 && source >= video.collapsedAt-maxFrameGapTicks) {
			video.audioJumped = true
		}
		video.lastAudio, video.haveAudio = source, true
		if video.audioJumped {
			held = append(held, early{pts: pts, frames: copyFrames(frames)})
			if first := unwrapNear(held[0].pts, video.lastDTS); source-first > audioHoldTicks || len(held) >= 512 {
				release()
			}
			return
		}
		if at, ok := video.audioTime(pts); ok {
			encoder.feed(at, frames)
		}
	}
	reader.OnDataH264(videoTrack, func(pts, dts int64, au [][]byte) error {
		started := video.started
		video.write(pts, dts, au)
		r.timeline.observe(video.end)
		if !started && video.started && encoder != nil {
			for _, item := range waiting {
				feed(item.pts, item.frames)
			}
			waiting = nil
		}
		if video.collapsed {
			video.collapsed = false
			release()
		}
		return nil
	})
	if encoder != nil {
		switch audioTrack.Codec.(type) {
		case *tscodecs.MPEG4Audio:
			reader.OnDataMPEG4Audio(audioTrack, func(pts int64, aus [][]byte) error {
				feed(pts, aus)
				return nil
			})
		case *tscodecs.Opus:
			reader.OnDataOpus(audioTrack, func(pts int64, packets [][]byte) error {
				feed(pts, packets)
				return nil
			})
		case *tscodecs.MPEG1Audio:
			reader.OnDataMPEG1Audio(audioTrack, func(pts int64, frames [][]byte) error {
				feed(pts, frames)
				return nil
			})
		}
	}
	var err error
	for err == nil && ctx.Err() == nil {
		err = reader.Read()
	}
	video.finish()
	if encoder != nil {
		encoder.stop()
	}
	if audio != nil {
		audio.finish()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (r *Role) nodeName() string {
	if r.node == nil {
		return ""
	}
	return r.node.NodeID
}

func copyFrames(frames [][]byte) [][]byte {
	copied := make([][]byte, len(frames))
	for i, frame := range frames {
		copied[i] = append([]byte(nil), frame...)
	}
	return copied
}

type sourceVideo struct {
	out       *output
	audio     *sourceAudio
	segmentMS int64
	partTicks int64
	origin    int64

	started     bool
	audioSource bool
	audioJumped bool
	collapsed   bool
	lastAudio   int64
	haveAudio   bool
	collapsedAt int64
	shift       int64
	lastDTS     int64
	lastOut     int64
	paramSPS    []byte
	paramPPS    []byte
	sps, pps    []byte
	accepted    bool
	segIndex    int64
	sequence    uint32
	pending     *pendingSample
	samples     []*fmp4.Sample
	base        int64
	duration    int64
	lastDur     int64
	end         int64
}

func (v *sourceVideo) timeIndex(t int64) int64 {
	return floorDiv(4*t+v.segmentMS*90, 4*v.segmentMS*90)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func (v *sourceVideo) audioTime(raw int64) (int64, bool) {
	if !v.started {
		return 0, false
	}
	at := unwrapNear(raw, v.lastDTS) + v.shift
	return at, at >= v.origin
}

func (v *sourceVideo) write(rawPTS, rawDTS int64, au [][]byte) {
	filtered := au[:0:0]
	slices := false
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		switch typ := h264.NALUType(nalu[0] & 0x1f); {
		case typ == h264.NALUTypeAccessUnitDelimiter:
			continue
		case typ == h264.NALUTypeSPS:
			v.paramSPS = append([]byte(nil), nalu...)
		case typ == h264.NALUTypePPS:
			v.paramPPS = append([]byte(nil), nalu...)
		case typ >= h264.NALUTypeNonIDR && typ <= h264.NALUTypeIDR:
			slices = true
		}
		filtered = append(filtered, nalu)
	}
	if !slices {
		return
	}
	key := hasIDR(filtered)
	ref := v.lastDTS
	if !v.started {
		ref = rawDTS
	}
	dts := unwrapNear(rawDTS, ref)
	pts := unwrapNear(rawPTS, dts)
	if !v.started {
		if !key || v.paramSPS == nil || v.paramPPS == nil {
			return
		}
		v.started = true
		v.shift = v.origin - dts
	} else if gap := dts - v.lastDTS; gap <= 0 || (gap > maxFrameGapTicks &&
		(!v.audioSource || v.audioJumped || !v.haveAudio || v.lastAudio < dts-maxFrameGapTicks)) {
		step := max(v.lastDur, 1)
		log.Printf("media transcode source: source timeline jumped by %d ms; collapsed to %d ms", gap/90, step/90)
		v.shift = v.lastOut + step - dts
		v.collapsed, v.collapsedAt = true, dts
	}
	v.lastDTS = dts
	shift := v.shift
	t, p := dts+shift, pts+shift
	v.lastOut = t
	newParameters := key && (!bytes.Equal(v.paramSPS, v.sps) || !bytes.Equal(v.paramPPS, v.pps))
	first := v.pending == nil && len(v.samples) == 0 && !v.accepted
	boundary := key && (first || v.timeIndex(t) > v.segIndex || newParameters)
	if v.pending != nil {
		previous := v.pending
		duration := t - previous.dts
		if duration <= 0 {
			duration = max(v.lastDur, 1)
		}
		previous.sample.Duration = uint32(duration)
		v.lastDur = duration
		if len(v.samples) > 0 && v.duration+duration > v.partTicks {
			v.flush()
		}
		if len(v.samples) == 0 {
			v.base = previous.dts
		}
		v.samples = append(v.samples, previous.sample)
		v.duration += duration
		v.pending = nil
		if boundary {
			v.flush()
		}
	}
	if newParameters {
		init, err := marshalInit(&fmp4.InitTrack{ID: 1, TimeScale: 90000, Codec: &mp4codecs.H264{SPS: v.paramSPS, PPS: v.paramPPS}})
		if err != nil {
			log.Printf("media transcode source: init segment: %v", err)
			v.sps, v.pps, v.accepted = nil, nil, false
			return
		}
		v.sps, v.pps = v.paramSPS, v.paramPPS
		v.out.reinit(init)
		v.accepted = true
	}
	if !v.accepted {
		return
	}
	if boundary {
		v.segIndex = v.timeIndex(t)
		if v.audio != nil {
			v.audio.cut(t)
		}
	}
	payload, err := h264.AVCC(filtered).Marshal()
	if err != nil {
		return
	}
	v.pending = &pendingSample{dts: t, key: key,
		sample: &fmp4.Sample{PTSOffset: int32(p - t), IsNonSyncSample: !key, Payload: payload}}
	v.end = max(v.end, t)
	if v.audio != nil {
		v.audio.advance(t)
	}
}

func (v *sourceVideo) flush() {
	if len(v.samples) == 0 {
		return
	}
	v.sequence++
	data, err := marshalPart(v.sequence, uint64(max(v.base, 0)), v.samples)
	independent := !v.samples[0].IsNonSyncSample
	v.samples, v.duration = nil, 0
	if err == nil && v.accepted {
		v.out.push(Fragment{Data: data, Independent: independent})
	}
}

func (v *sourceVideo) finish() {
	if v.pending != nil {
		duration := max(v.lastDur, 1)
		v.pending.sample.Duration = uint32(duration)
		if len(v.samples) > 0 && v.duration+duration > v.partTicks {
			v.flush()
		}
		if len(v.samples) == 0 {
			v.base = v.pending.dts
		}
		v.samples = append(v.samples, v.pending.sample)
		v.end = v.pending.dts + duration
		v.pending = nil
	}
	v.flush()
}
