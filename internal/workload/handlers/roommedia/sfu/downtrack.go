package sfu

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type DownTrack struct {
	conn *subscriberConn
	id   string
	kind webrtc.RTPCodecType

	bindMu    sync.Mutex
	bound     bool
	ssrc      uint32
	writer    webrtc.TrackLocalWriter
	payloads  map[string]webrtc.PayloadType
	clockRate uint32
	idDD      uint8
	idAudio   uint8

	mu  sync.Mutex
	mid string

	track    *Track
	fixedRID string
	fixed    bool
	prefs    slotPrefs

	target         int
	targetTemporal int
	targetReason   string
	current        int
	source         *Source

	started      bool
	highestIn    uint16
	seqOffset    uint16
	lastSeqOut   uint16
	tsOffset     uint32
	lastTSOut    uint32
	lastOutNanos int64

	vp8Started   bool
	vp8Munge     bool
	vp8PicOffset uint16
	vp8PicOut    uint16
	vp8TL0Offset uint8
	vp8TL0Out    uint8

	writeFailed        bool
	receiverKeyframeAt int64
	keyframeSuppressed int64

	ddStarted   bool
	ddOffset    uint16
	ddFrameOut  uint16
	lastLayer   [2]int
	lastForward int64
	forwards    [2]trackForwards

	lastPT     uint8
	frameEnded bool
	padPoints  [padPointsKept]padPoint
	padCount   int
	padFloor   uint16
	padFloored bool
}

type padPoint struct {
	boundary uint16
	base     uint16
}

const padPointsKept = 8

func (d *DownTrack) seqOffsetFor(seq uint16) (uint16, bool) {
	if d.padFloored && !seqNewer(seq, d.padFloor) {
		return 0, false
	}
	for _, point := range d.padPoints[:d.padCount] {
		if !seqNewer(seq, point.boundary) {
			return point.base, true
		}
	}
	return d.seqOffset, true
}

func (d *DownTrack) notePadding() {
	if d.padCount == 0 || d.padPoints[d.padCount-1].boundary != d.highestIn {
		if d.padCount == padPointsKept {
			d.padFloor, d.padFloored = d.padPoints[0].boundary, true
			copy(d.padPoints[:], d.padPoints[1:])
			d.padCount--
		}
		d.padPoints[d.padCount] = padPoint{boundary: d.highestIn, base: d.seqOffset}
		d.padCount++
	}
	d.seqOffset--
}

type slotPrefs struct {
	maxSpatial  int
	maxTemporal int
	width       int64
	height      int64
}

func noPrefs() slotPrefs { return slotPrefs{maxSpatial: -1, maxTemporal: -1} }

func newDownTrack(conn *subscriberConn, id string, kind webrtc.RTPCodecType) *DownTrack {
	return &DownTrack{conn: conn, id: id, kind: kind, target: -1, current: -1, targetTemporal: 2, lastLayer: [2]int{-1, -1}, prefs: noPrefs()}
}

func (d *DownTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codecs := ctx.CodecParameters()
	if len(codecs) == 0 {
		return webrtc.RTPCodecParameters{}, errors.New("slot has no negotiated codec")
	}
	payloads := map[string]webrtc.PayloadType{}
	chosen := -1
	for index, codec := range codecs {
		mime := strings.ToLower(codec.MimeType)
		if mime == strings.ToLower(webrtc.MimeTypeRTX) {
			continue
		}
		if _, seen := payloads[mime]; !seen {
			payloads[mime] = codec.PayloadType
		}
		preferred := strings.ToLower(webrtc.MimeTypeVP8)
		if d.kind == webrtc.RTPCodecTypeAudio {
			preferred = strings.ToLower(webrtc.MimeTypeOpus)
		}
		if chosen < 0 || mime == preferred && strings.ToLower(codecs[chosen].MimeType) != preferred {
			chosen = index
		}
	}
	if chosen < 0 {
		return webrtc.RTPCodecParameters{}, errors.New("slot has no media codec")
	}
	var idDD, idAudio uint8
	for _, extension := range ctx.HeaderExtensions() {
		switch extension.URI {
		case DependencyDescriptorURI:
			idDD = uint8(extension.ID)
		case AudioLevelURI:
			idAudio = uint8(extension.ID)
		}
	}
	if d.conn != nil {
		d.conn.api.repairStream(uint32(ctx.SSRCRetransmission()), uint32(ctx.SSRC()))
	}
	d.bindMu.Lock()
	defer d.bindMu.Unlock()
	d.bound, d.ssrc, d.writer, d.payloads = true, uint32(ctx.SSRC()), ctx.WriteStream(), payloads
	d.clockRate = codecs[chosen].ClockRate
	d.idDD, d.idAudio = idDD, idAudio
	return codecs[chosen], nil
}

func (d *DownTrack) Unbind(ctx webrtc.TrackLocalContext) error {
	if d.conn != nil {
		d.conn.api.forgetStream(uint32(ctx.SSRCRetransmission()), uint32(ctx.SSRC()))
	}
	d.bindMu.Lock()
	defer d.bindMu.Unlock()
	d.bound, d.writer = false, nil
	return nil
}

func (d *DownTrack) ID() string                { return d.id }
func (d *DownTrack) RID() string               { return "" }
func (d *DownTrack) StreamID() string          { return "beam" }
func (d *DownTrack) Kind() webrtc.RTPCodecType { return d.kind }

func (d *DownTrack) MID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mid
}

func (d *DownTrack) setMID(mid string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mid = mid
}

func (d *DownTrack) adaptation() (bool, slotPrefs) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fixed, d.prefs
}

func (d *DownTrack) ssrcValue() uint32 {
	d.bindMu.Lock()
	defer d.bindMu.Unlock()
	return d.ssrc
}

func (d *DownTrack) assigned() *Track {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.track
}

func (d *DownTrack) assign(track *Track, rid *string, prefs slotPrefs) {
	previous := d.bindTrack(track, rid, prefs)
	if previous != nil && previous != track {
		previous.removeSinkFromLayers(d)
	}
	d.attachLayers(track)
	d.requestKeyframe()
}

func (d *DownTrack) bindTrack(track *Track, rid *string, prefs slotPrefs) *Track {
	d.mu.Lock()
	defer d.mu.Unlock()
	previous := d.track
	d.track = track
	d.fixed = rid != nil
	d.fixedRID = ""
	if rid != nil {
		d.fixedRID = *rid
	}
	d.prefs = prefs
	d.current, d.source = -1, nil
	d.target = -1
	d.targetTemporal = 2
	if d.kind == webrtc.RTPCodecTypeAudio {
		d.target = 0
	}
	if d.fixed {
		d.target = -1
		if layer := track.Layer(d.fixedRID); layer != nil {
			d.target = layer.spatial
		}
	}
	return previous
}

func (d *DownTrack) attachLayers(track *Track) {
	if track == nil {
		return
	}
	track.addSinkToLayers(d)
}

func (d *DownTrack) release() *Track {
	track := func() *Track {
		d.mu.Lock()
		defer d.mu.Unlock()
		track := d.track
		d.track, d.current, d.source, d.target = nil, -1, nil, -1
		return track
	}()
	if track != nil {
		track.removeSinkFromLayers(d)
	}
	return track
}

func (d *DownTrack) setTarget(spatial, temporal int, reason string) {
	adaptive, changed, track, current := func() (bool, bool, *Track, int) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.fixed || d.kind != webrtc.RTPCodecTypeVideo {
			return false, false, nil, 0
		}
		changed := spatial != d.target
		d.target, d.targetTemporal, d.targetReason = spatial, temporal, reason
		return true, changed, d.track, d.current
	}()
	if !adaptive {
		return
	}
	if changed && spatial >= 0 && spatial != current && track != nil {
		if layer := track.layerAt(spatial); layer != nil {
			layer.RequestKeyframe()
		}
	}
	if spatial < 0 {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.current >= 0 {
			d.current = -1
			d.reportLayerLocked(-1, -1, reason)
		}
	}
}

func (d *DownTrack) requestKeyframe() {
	track, spatial := func() (*Track, int) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.current < 0 {
			return d.track, d.target
		}
		return d.track, d.current
	}()
	if track == nil || spatial < 0 || d.kind != webrtc.RTPCodecTypeVideo {
		return
	}
	if layer := track.layerAt(spatial); layer != nil {
		layer.RequestKeyframe()
	}
}

const receiverKeyframeInterval = time.Second

func (d *DownTrack) receiverKeyframeRequest(now time.Time) bool {
	track, spatial := func() (*Track, int) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.track == nil || d.kind != webrtc.RTPCodecTypeVideo {
			return nil, -1
		}
		nanos := now.UnixNano()
		if d.current < 0 || d.target != d.current ||
			d.receiverKeyframeAt != 0 && nanos-d.receiverKeyframeAt < int64(receiverKeyframeInterval) {
			d.keyframeSuppressed++
			return nil, -1
		}
		d.receiverKeyframeAt = nanos
		return d.track, d.current
	}()
	if track == nil {
		return false
	}
	layer := track.layerAt(spatial)
	if layer == nil {
		return false
	}
	layer.RequestKeyframe()
	return true
}

func (d *DownTrack) takeSuppressedKeyframes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	count := d.keyframeSuppressed
	d.keyframeSuppressed = 0
	return count
}

func (d *DownTrack) forwarding(now time.Time) (spatial, layers int, video, active bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.track == nil {
		return -1, 0, d.kind == webrtc.RTPCodecTypeVideo, false
	}
	active = now.UnixNano()-d.lastForward < int64(2*time.Second)
	return d.current, len(d.track.Layers()), d.kind == webrtc.RTPCodecTypeVideo, active
}

func seqNewer(value, than uint16) bool { return int16(value-than) > 0 }

func (d *DownTrack) write(layer *Layer, source *Source, packet *rtp.Packet, meta *PacketMeta) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.track != layer.track {
		return
	}
	if layer.track.mutedServer() {
		return
	}
	if d.conn != nil && d.conn.suspended.Load() {
		d.current, d.source = -1, nil
		return
	}
	now := time.Now()
	if d.kind == webrtc.RTPCodecTypeVideo {
		want := d.target
		if want < 0 {
			return
		}
		if layer.spatial != d.current || source != d.source {
			if layer.spatial != want {
				return
			}
			if !meta.Keyframe || !meta.StartOfFrame {
				layer.RequestKeyframe()
				return
			}
			d.anchorLocked(layer, source, packet, meta, now)
		} else if d.targetTemporal >= 0 && meta.Temporal > d.targetTemporal {
			d.skipLocked(packet, meta)
			return
		}
	} else if source != d.source {
		d.anchorLocked(layer, source, packet, meta, now)
	}
	d.forwardLocked(packet, meta, now)
}

func (d *DownTrack) anchorLocked(layer *Layer, source *Source, packet *rtp.Packet, meta *PacketMeta, now time.Time) {
	if d.started {
		d.seqOffset = packet.SequenceNumber - (d.lastSeqOut + 1)
		step := uint32(1)
		if d.clockRate > 0 && d.lastOutNanos > 0 {
			step = max(uint32(int64(d.clockRate)*(now.UnixNano()-d.lastOutNanos)/int64(time.Second)), 1)
		}
		d.tsOffset = packet.Timestamp - (d.lastTSOut + step)
	}
	d.highestIn = packet.SequenceNumber - 1
	d.padCount, d.padFloored, d.frameEnded = 0, false, false
	if meta.VP8 != nil && meta.VP8.pictureLen > 0 && d.vp8Started {
		d.vp8PicOffset = meta.VP8.pictureID - (d.vp8PicOut + 1)
		d.vp8Munge = true
		if meta.VP8.hasTL0 {
			d.vp8TL0Offset = meta.VP8.tl0 - (d.vp8TL0Out + 1)
		}
	}
	if meta.DD != nil && d.ddStarted {
		d.ddOffset = meta.FrameNumber - (d.ddFrameOut + 1)
	}
	previous := d.current
	d.current, d.source = layer.spatial, source
	if d.kind == webrtc.RTPCodecTypeVideo && previous != layer.spatial {
		reason := d.targetReason
		if reason == "" {
			reason = "bandwidth"
		}
		temporal := d.targetTemporal
		if temporal < 0 {
			temporal = 2
		}
		d.reportLayerLocked(layer.spatial, temporal, reason)
	}
}

func (d *DownTrack) reportLayerLocked(spatial, temporal int, reason string) {
	if d.lastLayer == [2]int{spatial, temporal} || d.track == nil {
		return
	}
	d.lastLayer = [2]int{spatial, temporal}
	info := d.track.info
	go d.conn.layerChanged(info.TrackID, info.ParticipantID, spatial, temporal, reason)
}

func (d *DownTrack) skipLocked(packet *rtp.Packet, meta *PacketMeta) {
	if seqNewer(packet.SequenceNumber, d.highestIn) {
		d.highestIn = packet.SequenceNumber
		d.frameEnded = packet.Marker
		d.seqOffset++
		if meta.StartOfFrame && meta.VP8 != nil && meta.VP8.pictureLen > 0 && meta.DD == nil {
			d.vp8PicOffset++
			d.vp8Munge = true
		}
	}
}

func (d *DownTrack) forwardLocked(packet *rtp.Packet, meta *PacketMeta, now time.Time) {
	bound, writer, ssrc, payloads, idDD, idAudio := func() (bool, webrtc.TrackLocalWriter, uint32, map[string]webrtc.PayloadType, uint8, uint8) {
		d.bindMu.Lock()
		defer d.bindMu.Unlock()
		return d.bound, d.writer, d.ssrc, d.payloads, d.idDD, d.idAudio
	}()
	if !bound || writer == nil {
		return
	}
	payload := packet.Payload
	payloadType, ok := payloads[meta.Mime]
	if !ok && meta.Mime == mimeTypeRED {
		primary, valid := redPrimaryData(payload)
		opus, haveOpus := payloads[strings.ToLower(webrtc.MimeTypeOpus)]
		if !valid || !haveOpus {
			return
		}
		payload, payloadType, ok = primary, opus, true
	}
	if !ok {
		return
	}
	offset, numbered := d.seqOffsetFor(packet.SequenceNumber)
	if !numbered {
		return
	}
	newer := seqNewer(packet.SequenceNumber, d.highestIn)
	if newer {
		d.highestIn = packet.SequenceNumber
		d.frameEnded = packet.Marker
	}
	header := rtp.Header{Version: 2, Marker: packet.Marker, PayloadType: uint8(payloadType),
		SequenceNumber: packet.SequenceNumber - offset, Timestamp: packet.Timestamp - d.tsOffset, SSRC: ssrc}
	if meta.VP8 != nil && d.vp8Munge && meta.VP8.pictureLen > 0 {
		mask := uint16(0x7fff)
		if meta.VP8.pictureLen == 1 {
			mask = 0x7f
		}
		picture := (meta.VP8.pictureID - d.vp8PicOffset) & mask
		tl0 := meta.VP8.tl0 - d.vp8TL0Offset
		payload = rewriteVP8(payload, *meta.VP8, picture, tl0)
		d.vp8PicOut, d.vp8TL0Out = picture, tl0
	} else if meta.VP8 != nil && meta.VP8.pictureLen > 0 {
		d.vp8PicOut, d.vp8TL0Out = meta.VP8.pictureID, meta.VP8.tl0
	}
	if meta.VP8 != nil && meta.VP8.pictureLen > 0 {
		d.vp8Started = true
	}
	if idDD != 0 && meta.DD != nil {
		frame := meta.FrameNumber - d.ddOffset
		descriptor := meta.DD
		if d.ddOffset != 0 {
			descriptor = rewriteDDFrameNumber(meta.DD, frame)
		}
		_ = header.SetExtension(idDD, descriptor)
		d.ddFrameOut, d.ddStarted = frame, true
	}
	if idAudio != 0 && meta.AudioLevel >= 0 {
		level := byte(meta.AudioLevel & 0x7f)
		if meta.Voice {
			level |= 0x80
		}
		_ = header.SetExtension(idAudio, []byte{level})
	}
	written, err := writer.WriteRTP(&header, payload)
	if err != nil {
		if !d.writeFailed {
			d.writeFailed = true
			s := d.conn.session
			s.opts.Logf("sfu: session %s: slot %s cannot forward: %v", s.opts.SessionID, d.id, err)
		}
		return
	}
	if newer || !d.started {
		d.lastSeqOut, d.lastTSOut = header.SequenceNumber, header.Timestamp
	}
	d.started = true
	d.lastPT = header.PayloadType
	d.lastOutNanos = now.UnixNano()
	d.lastForward = d.lastOutNanos
	d.noteForwarded(d.lastOutNanos)
	d.conn.sent(now, written, header.MarshalSize()+len(payload))
}

func (d *DownTrack) readRTCP(sender *webrtc.RTPSender) {
	for {
		packets, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, packet := range packets {
			switch value := packet.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				d.receiverKeyframeRequest(time.Now())
			case *rtcp.ReceiverReport:
				ssrc := d.ssrcValue()
				for _, report := range value.Reports {
					if report.SSRC == ssrc {
						d.conn.receiverReport(report)
					}
				}
			}
		}
	}
}
