package packager

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

func (r *rendition) setInit(data []byte) error {
	var init fmp4.Init
	if err := init.Unmarshal(bytes.NewReader(data)); err != nil {
		return err
	}
	if len(init.Tracks) != 1 {
		return errors.New("a rendition init holds one track")
	}
	track := init.Tracks[0]
	codecString, width, height, h265 := "", 0, 0, false
	fps := 0.0
	switch codec := track.Codec.(type) {
	case *mp4codecs.H264:
		codecString = "avc1." + hex.EncodeToString(codec.SPS[1:4])
		var sps h264.SPS
		if sps.Unmarshal(codec.SPS) == nil {
			width, height = sps.Width(), sps.Height()
			fps = sps.FPS()
		}
	case *mp4codecs.H265:
		h265 = true
		codecString = h265CodecString(codec.SPS)
	case *mp4codecs.MPEG4Audio:
		codecString = "mp4a.40." + strconv.Itoa(int(codec.Config.Type))
	case *mp4codecs.Opus:
		codecString = "opus"
	default:
		return errors.New("unsupported rendition codec")
	}
	r.mu.Lock()
	current := r.currentInit()
	r.mu.Unlock()
	if current != nil && bytes.Equal(current.clear, data) {
		return nil
	}
	served := data
	if r.key != nil {
		protected, err := encryptInit(data, r.key, r.video)
		if err != nil {
			return err
		}
		served = protected
	}
	version := &initVersion{uri: mc.SegmentInitURI(served), clear: append([]byte(nil), data...), data: served, sha: sha256Hex(served)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.follow != nil {
		r.drain(true)
	}
	if open := r.current(); open != nil {
		if len(open.parts) > 0 {
			r.complete(open)
		} else {
			r.segments = r.segments[:len(r.segments)-1]
		}
	}
	first := len(r.inits) == 0
	r.inits = append(r.inits, version)
	r.newInit = !first
	r.timescale = track.TimeScale
	r.codec, r.h265 = codecString, h265
	if width > 0 {
		r.width, r.height = width, height
	}
	r.fps = fps
	r.pruneInits()
	if first {
		close(r.ready)
	} else {
		log.Printf("media package rendition %s: new init %s (encoder change)", r.id, version.uri)
	}
	r.notify()
	return nil
}

func (r *rendition) newRun(data []byte) error {
	r.mu.Lock()
	current := r.currentInit()
	r.mu.Unlock()
	if current == nil || !bytes.Equal(current.clear, data) {
		return r.setInit(data)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.follow != nil || r.last() == nil {
		return nil
	}
	if open := r.current(); open != nil {
		if len(open.parts) > 0 {
			r.complete(open)
		} else {
			r.segments = r.segments[:len(r.segments)-1]
		}
	}
	r.restarted = true
	log.Printf("media package rendition %s: new encoder run: the next segment starts with EXT-X-DISCONTINUITY", r.id)
	r.notify()
	return nil
}

func (r *rendition) otherParent(parent string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.parent != "" && r.parent != parent
}

func (r *rendition) fedBy(parent string) {
	r.mu.Lock()
	r.parent = parent
	r.mu.Unlock()
}

func (r *rendition) reconnected() {
	r.mu.Lock()
	r.resync = r.last() != nil
	r.mu.Unlock()
}

const resyncGapS = 0.001

func (r *rendition) currentInit() *initVersion {
	if len(r.inits) == 0 {
		return nil
	}
	return r.inits[len(r.inits)-1]
}

func (r *rendition) pruneInits() {
	used := map[*initVersion]bool{r.currentInit(): true}
	for _, seg := range r.segments {
		used[seg.init] = true
	}
	kept := r.inits[:0]
	for _, version := range r.inits {
		if used[version] {
			kept = append(kept, version)
		}
	}
	r.inits = kept
}

func (r *rendition) startAt(msn, manifestSequence int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextMSN = max(r.nextMSN, msn)
	r.manifestSeq = max(r.manifestSeq, manifestSequence)
}

func h265CodecString(sps []byte) string {
	if len(sps) < 3 {
		return "hvc1.1.6.L93.B0"
	}
	data := h264.EmulationPreventionRemove(sps[2:])
	if len(data) < 13 {
		return "hvc1.1.6.L93.B0"
	}
	space := []string{"", "A", "B", "C"}[data[1]>>6]
	tier := "L"
	if data[1]>>5&1 == 1 {
		tier = "H"
	}
	profile := data[1] & 0x1f
	compat := uint32(data[2])<<24 | uint32(data[3])<<16 | uint32(data[4])<<8 | uint32(data[5])
	var reversed uint32
	for i := range 32 {
		reversed |= (compat >> i & 1) << (31 - i)
	}
	result := fmt.Sprintf("hvc1.%s%d.%X.%s%d", space, profile, reversed, tier, data[12])
	constraints := data[6:12]
	last := len(constraints)
	for last > 0 && constraints[last-1] == 0 {
		last--
	}
	for _, value := range constraints[:last] {
		result += fmt.Sprintf(".%X", value)
	}
	return result
}
