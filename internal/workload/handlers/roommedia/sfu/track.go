package sfu

import (
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const keyframeInterval = 500 * time.Millisecond

func spatialOfRID(rid string, position int) int {
	switch strings.ToLower(rid) {
	case "q", "l", "low", "0":
		return 0
	case "h", "m", "mid", "medium", "1":
		return 1
	case "f", "full", "high", "2":
		return 2
	}
	return min(max(position, 0), 2)
}

type Track struct {
	session  *Session
	identity string
	kind     webrtc.RTPCodecType

	mu         sync.RWMutex
	info       mediacontracts.Track
	layers     []*Layer
	owner      *publisherConn
	detached   time.Time
	lostFrom   *publisherConn
	serverMute bool

	level levelMeter
}

func (t *Track) Info() mediacontracts.Track {
	t.mu.RLock()
	defer t.mu.RUnlock()
	info := t.info
	if info.GraceUntilMS != nil {
		value := *info.GraceUntilMS
		info.GraceUntilMS = &value
	}
	info.Layers = append([]mediacontracts.SimulcastLayer(nil), t.info.Layers...)
	if info.Layers == nil {
		info.Layers = []mediacontracts.SimulcastLayer{}
	}
	return info
}

func (t *Track) ID() string { return t.info.TrackID }

func (t *Track) muted() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.info.Muted
}

func (t *Track) attach(owner *publisherConn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasDetached := t.info.State == mediacontracts.TrackDetached
	t.owner, t.detached, t.lostFrom = owner, time.Time{}, nil
	t.info.State, t.info.GraceUntilMS = "", nil
	return wasDetached
}

func (t *Track) detach(owner *publisherConn, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.owner == owner {
		t.owner, t.detached, t.lostFrom = nil, now, owner
		t.info.State = mediacontracts.TrackDetached
	}
}

func (t *Track) detachedFrom(conn *publisherConn) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.owner == nil && !t.detached.IsZero() && t.lostFrom == conn
}

func (t *Track) detachedUntil(untilMS int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.info.GraceUntilMS != nil && *t.info.GraceUntilMS == untilMS {
		return false
	}
	t.info.State, t.info.GraceUntilMS = mediacontracts.TrackDetached, &untilMS
	return true
}

func (t *Track) mirrorState(state string, graceUntilMS *int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if state != mediacontracts.TrackDetached || graceUntilMS == nil {
		state, graceUntilMS = "", nil
	}
	if t.info.State == state && sameMS(t.info.GraceUntilMS, graceUntilMS) {
		return false
	}
	t.info.State = state
	if graceUntilMS != nil {
		value := *graceUntilMS
		graceUntilMS = &value
	}
	t.info.GraceUntilMS = graceUntilMS
	return true
}

func (t *Track) stateEvent() mediacontracts.TrackStateEvent {
	t.mu.RLock()
	defer t.mu.RUnlock()
	event := mediacontracts.TrackStateEvent{EventHeader: header("track_state"), TrackID: t.info.TrackID,
		ParticipantID: t.info.ParticipantID, State: mediacontracts.TrackLive}
	if t.info.State == mediacontracts.TrackDetached {
		event.State = mediacontracts.TrackDetached
		if t.info.GraceUntilMS != nil {
			value := *t.info.GraceUntilMS
			event.GraceUntilMS = &value
		}
	}
	return event
}

func sameMS(left, right *int64) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func (t *Track) ownedBy(owner *publisherConn) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.owner == owner
}

func (t *Track) waitsForPublisher() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.owner == nil && !t.detached.IsZero()
}

func (t *Track) setMuted(muted, server bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := t.info.Muted != muted
	t.info.Muted = muted
	if server {
		t.serverMute = muted
	}
	return changed
}

func (t *Track) mirror(muted bool, layers []mediacontracts.SimulcastLayer) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := t.info.Muted != muted
	t.info.Muted, t.info.Layers = muted, layers
	return changed
}

func (t *Track) Layers() []*Layer {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]*Layer(nil), t.layers...)
}

func (t *Track) Layer(rid string) *Layer {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, layer := range t.layers {
		if layer.rid == rid {
			return layer
		}
	}
	layer := &Layer{track: t, rid: rid, spatial: spatialOfRID(rid, len(t.layers))}
	if rid == "" {
		layer.spatial = 0
	}
	if len(t.layers) > 0 {
		if sinks := t.layers[0].sinks.Load(); sinks != nil {
			copied := append([]*DownTrack(nil), (*sinks)...)
			layer.sinks.Store(&copied)
		}
	}
	t.layers = append(t.layers, layer)
	return layer
}

func (t *Track) addSinkToLayers(sink *DownTrack) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, layer := range t.layers {
		layer.addSink(sink)
	}
}

func (t *Track) removeSinkFromLayers(sink *DownTrack) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, layer := range t.layers {
		layer.removeSink(sink)
	}
}

func (t *Track) layerAt(spatial int) *Layer {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, layer := range t.layers {
		if layer.spatial == spatial {
			return layer
		}
	}
	return nil
}

func (t *Track) activeSpatials(now time.Time) []int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var result []int
	for spatial := range 3 {
		for _, layer := range t.layers {
			if layer.spatial == spatial && layer.active(now) {
				result = append(result, spatial)
				break
			}
		}
	}
	return result
}

type Source struct {
	generation      uint64
	requestKeyframe func()
}

var sourceGenerations atomic.Uint64

func NewSource(requestKeyframe func()) *Source {
	return &Source{generation: sourceGenerations.Add(1), requestKeyframe: requestKeyframe}
}

type ExtensionIDs struct {
	DependencyDescriptor uint8
	AudioLevel           uint8
}

type Layer struct {
	track   *Track
	rid     string
	spatial int

	source    atomic.Pointer[Source]
	sinks     atomic.Pointer[[]*DownTrack]
	sinkMu    sync.Mutex
	templates atomic.Pointer[ddTemplates]

	lastPacket                  atomic.Int64
	lastKeyframe                atomic.Int64
	lastPLI                     atomic.Int64
	pliPending                  atomic.Bool
	width                       atomic.Int32
	height                      atomic.Int32
	meteredWidth, meteredHeight atomic.Int32
	rate                        rateMeter
	temporal                    atomic.Bool
}

func (l *Layer) RID() string { return l.rid }

func (l *Layer) Spatial() int { return l.spatial }

func (l *Layer) active(now time.Time) bool {
	last := l.lastPacket.Load()
	return last != 0 && now.UnixNano()-last < int64(2*time.Second)
}

func (l *Layer) addSink(sink *DownTrack) {
	l.sinkMu.Lock()
	defer l.sinkMu.Unlock()
	current := l.sinks.Load()
	next := make([]*DownTrack, 0, 8)
	if current != nil {
		for _, existing := range *current {
			if existing == sink {
				return
			}
			next = append(next, existing)
		}
	}
	next = append(next, sink)
	l.sinks.Store(&next)
}

func (l *Layer) removeSink(sink *DownTrack) {
	l.sinkMu.Lock()
	defer l.sinkMu.Unlock()
	current := l.sinks.Load()
	if current == nil {
		return
	}
	next := make([]*DownTrack, 0, len(*current))
	for _, existing := range *current {
		if existing != sink {
			next = append(next, existing)
		}
	}
	l.sinks.Store(&next)
}

func (l *Layer) RequestKeyframe() {
	if l.track.kind != webrtc.RTPCodecTypeVideo {
		return
	}
	now := time.Now().UnixNano()
	last := l.lastPLI.Load()
	if now-last >= int64(keyframeInterval) && l.lastPLI.CompareAndSwap(last, now) {
		if source := l.source.Load(); source != nil && source.requestKeyframe != nil {
			source.requestKeyframe()
		}
		return
	}
	if l.pliPending.CompareAndSwap(false, true) {
		wait := time.Duration(int64(keyframeInterval) - (now - last))
		time.AfterFunc(max(wait, time.Millisecond), func() {
			l.pliPending.Store(false)
			l.RequestKeyframe()
		})
	}
}

type PacketMeta struct {
	Keyframe     bool
	StartOfFrame bool
	Temporal     int
	DD           []byte
	FrameNumber  uint16
	VP8          *vp8Descriptor
	AudioLevel   int
	Voice        bool
	Mime         string
}

func (l *Layer) Feed(source *Source, packet *rtp.Packet, ids ExtensionIDs, mime string) {
	current := l.source.Load()
	if current != source {
		if current != nil && current.generation > source.generation {
			return
		}
		if !l.source.CompareAndSwap(current, source) {
			return
		}
		l.templates.Store(nil)
	}
	now := time.Now()
	l.lastPacket.Store(now.UnixNano())
	size := packet.MarshalSize()
	l.rate.add(now, size)
	l.track.session.ingress.add(now, size)
	meta := l.inspect(packet, ids, mime)
	if meta.Temporal > 0 && !l.temporal.Load() {
		l.temporal.Store(true)
	}
	if meta.Keyframe {
		l.lastKeyframe.Store(now.UnixNano())
	}
	if meta.AudioLevel >= 0 {
		l.track.level.observe(now, meta.AudioLevel, meta.Voice)
	}
	sinks := l.sinks.Load()
	if sinks == nil {
		return
	}
	for _, sink := range *sinks {
		l.writeSink(sink, source, packet, &meta)
	}
}

func (l *Layer) writeSink(sink *DownTrack, source *Source, packet *rtp.Packet, meta *PacketMeta) {
	defer func() {
		value := recover()
		if value == nil {
			return
		}
		l.removeSink(sink)
		s := l.track.session
		s.opts.Logf("sfu: session %s: recovered panic forwarding to slot %s: %v\n%s", s.opts.SessionID, sink.id, value, debug.Stack())
		if sink.conn != nil {
			go func() {
				defer s.guard("slot close", nil)
				sink.conn.abort()
			}()
		}
	}()
	sink.write(l, source, packet, meta)
}

func (l *Layer) inspect(packet *rtp.Packet, ids ExtensionIDs, mime string) PacketMeta {
	meta := PacketMeta{Temporal: -1, AudioLevel: -1, Mime: mime}
	if l.track.kind == webrtc.RTPCodecTypeAudio {
		meta.Keyframe, meta.StartOfFrame = true, true
		if ids.AudioLevel != 0 {
			if level, voice, ok := audioLevel(packet.GetExtension(ids.AudioLevel)); ok {
				meta.AudioLevel, meta.Voice = level, voice
			}
		}
		return meta
	}
	if ids.DependencyDescriptor != 0 {
		if raw := packet.GetExtension(ids.DependencyDescriptor); len(raw) >= 3 {
			if frame, err := parseDD(raw); err == nil {
				meta.DD = raw
				meta.FrameNumber = frame.frameNumber
				meta.StartOfFrame = frame.startOfFrame
				if frame.structure != nil {
					l.templates.Store(frame.structure)
					meta.Keyframe = frame.startOfFrame
				}
				if _, temporal, ok := l.templates.Load().layerOf(frame.templateID); ok {
					meta.Temporal = temporal
				}
			}
		}
	}
	if mime == strings.ToLower(webrtc.MimeTypeVP8) {
		if descriptor, err := parseVP8(packet.Payload); err == nil {
			meta.VP8 = &descriptor
			if meta.DD == nil {
				meta.StartOfFrame = descriptor.start
				if descriptor.hasTID {
					meta.Temporal = descriptor.tid
				}
			}
			if descriptor.keyframe {
				meta.Keyframe = true
				if descriptor.width > 0 && descriptor.height > 0 {
					l.width.Store(int32(descriptor.width))
					l.height.Store(int32(descriptor.height))
				}
			}
		}
	}
	if width, height := usage.RTPFrameSize(mime, packet.Payload); width > 0 && height > 0 {
		l.meteredWidth.Store(int32(width))
		l.meteredHeight.Store(int32(height))
	}
	return meta
}

type rateMeter struct {
	mu          sync.Mutex
	windowStart int64
	windowBytes int64
	bps         int64
}

func (m *rateMeter) add(now time.Time, bytes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	nanos := now.UnixNano()
	if m.windowStart == 0 {
		m.windowStart = nanos
	}
	if elapsed := nanos - m.windowStart; elapsed >= int64(time.Second) {
		current := m.windowBytes * 8 * int64(time.Second) / elapsed
		if m.bps == 0 {
			m.bps = current
		} else {
			m.bps = (m.bps*3 + current) / 4
		}
		m.windowStart, m.windowBytes = nanos, 0
	}
	m.windowBytes += int64(bytes)
}

func (m *rateMeter) value(now time.Time) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.windowStart != 0 && now.UnixNano()-m.windowStart > int64(3*time.Second) {
		return 0
	}
	return m.bps
}
