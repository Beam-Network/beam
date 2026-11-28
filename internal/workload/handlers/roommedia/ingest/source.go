package ingest

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/sdp/v3"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const (
	maxSourceLevelIDC  = 52
	fpsWindowTicks     = 2 * 90000
	bitrateWindowTicks = 10 * 90000
)

func sourceRefusal(tracks []TrackInfo) string {
	video := false
	for _, track := range tracks {
		if !track.Video {
			continue
		}
		if track.Codec != CodecH264 {
			return "video codec " + track.Codec + " (H.264 only)"
		}
		video = true
	}
	if !video {
		return "no video track"
	}
	return ""
}

func spsRefusal(raw []byte, ladder *mc.TranscodeLadder) string {
	var sps h264.SPS
	if err := sps.Unmarshal(raw); err != nil {
		return "unreadable H.264 SPS"
	}
	switch sps.ProfileIdc {
	case 66:
		if !sps.ConstraintSet1Flag {
			return "H.264 Baseline profile without the constrained flag (Constrained Baseline, Main or High only)"
		}
	case 77, 100:
	default:
		return "H.264 profile_idc " + strconv.Itoa(int(sps.ProfileIdc)) + " (Constrained Baseline, Main or High only)"
	}
	if sps.ProfileIdc == 100 && (sps.ChromaFormatIdc != 1 || sps.BitDepthLumaMinus8 != 0 || sps.BitDepthChromaMinus8 != 0) {
		return "H.264 High profile stream that is not 8-bit 4:2:0"
	}
	if sps.LevelIdc > maxSourceLevelIDC {
		return fmt.Sprintf("H.264 level %d.%d (5.2 or lower)", sps.LevelIdc/10, sps.LevelIdc%10)
	}
	if len(ladder.Video) == 0 {
		return ""
	}
	caps := ladder.Video[0]
	width, height := int64(sps.Width()), int64(sps.Height())
	if max(width, height) > max(caps.Width, caps.Height) || min(width, height) > min(caps.Width, caps.Height) {
		return fmt.Sprintf("frame size %dx%d above the ladder's %dx%d (either orientation)", width, height, caps.Width, caps.Height)
	}
	return ""
}

type sourceCheck struct {
	ladder *mc.TranscodeLadder
	sps    []byte

	started     bool
	last        int64
	lastKey     int64
	fpsStart    int64
	frames      int64
	bytesStart  int64
	bytes       int64
	haveWindows bool
	keyAt       atomic.Int64
	videoAt     atomic.Int64
}

func (c *sourceCheck) lastVideoAt() time.Time {
	if at := c.videoAt.Load(); at != 0 {
		return time.Unix(0, at)
	}
	return time.Time{}
}

func (c *sourceCheck) lastKeyAt() time.Time {
	if at := c.keyAt.Load(); at != 0 {
		return time.Unix(0, at)
	}
	return time.Time{}
}

func newSourceCheck(ladder *mc.TranscodeLadder) *sourceCheck {
	return &sourceCheck{ladder: ladder}
}

func (c *sourceCheck) video(unit Unit) string {
	c.videoAt.Store(time.Now().UnixNano())
	key := false
	for _, nalu := range unit.AU {
		if len(nalu) == 0 {
			continue
		}
		switch h264.NALUType(nalu[0] & 0x1f) {
		case h264.NALUTypeIDR:
			key = true
		case h264.NALUTypeSPS:
			if c.sps == nil || string(c.sps) != string(nalu) {
				if refusal := spsRefusal(nalu, c.ladder); refusal != "" {
					return refusal
				}
				c.sps = append([]byte(nil), nalu...)
			}
		}
	}
	dts := unit.DTS
	if !c.started || dts < c.last || dts-c.last > 10*90000 {
		c.started, c.haveWindows = true, false
		c.lastKey = dts
	}
	c.last = dts
	if key {
		c.lastKey = dts
		c.keyAt.Store(time.Now().UnixNano())
	} else if limit := 2 * c.ladder.GOPMS * 90; dts-c.lastKey > limit {
		return fmt.Sprintf("no keyframe for %d ms (one at least every %d ms)", (dts-c.lastKey)/90, c.ladder.GOPMS)
	}
	if !c.haveWindows {
		c.haveWindows = true
		c.fpsStart, c.frames = dts, 0
		c.bytesStart, c.bytes = dts, 0
	}
	c.frames++
	c.bytes += int64(unit.size())
	caps := c.ladder.Video[0]
	if span := dts - c.fpsStart; span >= fpsWindowTicks {
		fps := float64(c.frames-1) * 90000 / float64(span)
		if fps > float64(caps.FPSMax)*1.1 {
			return fmt.Sprintf("frame rate %.1f above fps_max %d", fps, caps.FPSMax)
		}
		c.fpsStart, c.frames = dts, 1
	}
	if span := dts - c.bytesStart; span >= bitrateWindowTicks {
		bps := c.bytes * 8 * 90000 / span
		if bps > 2*caps.BitrateBPS {
			return fmt.Sprintf("video bitrate %d bit/s over 10 s above 2 x bitrate_bps (%d)", bps, caps.BitrateBPS)
		}
		c.bytesStart, c.bytes = dts, 0
	}
	return ""
}

func whipSourceRefusal(offer, answer *sdp.SessionDescription) string {
	for _, media := range answer.MediaDescriptions {
		if media.MediaName.Media != "video" || media.MediaName.Port.Value == 0 {
			continue
		}
		if _, inactive := media.Attribute("inactive"); inactive {
			continue
		}
		mid, _ := media.Attribute("mid")
		for _, format := range media.MediaName.Formats {
			codec, err := answer.GetCodecForPayloadType(payloadType(format))
			if err != nil || !strings.EqualFold(codec.Name, "H264") {
				continue
			}
			return profileRefusal(offeredFmtp(offer, mid, format))
		}
	}
	return "no H.264 video accepted"
}

func payloadType(format string) uint8 {
	value, _ := strconv.ParseUint(format, 10, 8)
	return uint8(value)
}

func offeredFmtp(offer *sdp.SessionDescription, mid, format string) string {
	for _, media := range offer.MediaDescriptions {
		if value, _ := media.Attribute("mid"); value != mid {
			continue
		}
		for _, attribute := range media.Attributes {
			if attribute.Key == "fmtp" && strings.HasPrefix(attribute.Value, format+" ") {
				return strings.TrimPrefix(attribute.Value, format+" ")
			}
		}
	}
	return ""
}

func profileRefusal(fmtp string) string {
	for _, parameter := range strings.Split(fmtp, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(parameter), "=")
		if !strings.EqualFold(name, "profile-level-id") {
			continue
		}
		if len(value) != 6 {
			return "malformed H.264 profile-level-id"
		}
		profile, err1 := strconv.ParseUint(value[0:2], 16, 8)
		level, err2 := strconv.ParseUint(value[4:6], 16, 8)
		if err1 != nil || err2 != nil {
			return "malformed H.264 profile-level-id"
		}
		switch profile {
		case 66, 77, 100:
		default:
			return "H.264 profile " + value + " (Constrained Baseline, Main or High only)"
		}
		if level > maxSourceLevelIDC {
			return "H.264 level above 5.2 (" + value + ")"
		}
		return ""
	}
	return ""
}
