package ingest

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortmplib"
	rtmpcodecs "github.com/bluenviron/gortmplib/pkg/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	srt "github.com/datarhei/gosrt"

	"github.com/Beam-Network/beam/internal/worker/planeapi/shared"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
)

var listeners struct {
	sync.Mutex
	started bool
	up      map[string]bool
}

const (
	handshakeTimeout = 10 * time.Second
	readTimeout      = 15 * time.Second
	listenerRetry    = 30 * time.Second
)

func listenerUp(protocol string) bool {
	listeners.Lock()
	defer listeners.Unlock()
	return listeners.up[protocol]
}

func ensureListeners(env *broadcast.Env) {
	listeners.Lock()
	defer listeners.Unlock()
	if listeners.started {
		return
	}
	listeners.started = true
	listeners.up = map[string]bool{}
	config := env.Config
	if config.RTMPPort > 0 {
		shared.Go(func() {
			keepListening(ProtocolRTMP, func() error {
				listener, err := net.Listen("tcp", fmt.Sprintf(":%d", config.RTMPPort))
				if err != nil {
					return err
				}
				markUp(ProtocolRTMP)
				acceptRTMP(listener, ProtocolRTMP)
				return nil
			})
		})
	}
	if config.RTMPSPort > 0 {
		shared.Go(func() {
			keepListening(ProtocolRTMPS, func() error {
				listener, err := net.Listen("tcp", fmt.Sprintf(":%d", config.RTMPSPort))
				if err != nil {
					return err
				}
				markUp(ProtocolRTMPS)
				acceptRTMP(tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: env.GetCertificate}), ProtocolRTMPS)
				return nil
			})
		})
	}
	if config.SRTPort > 0 {
		shared.Go(func() {
			keepListening(ProtocolSRT, func() error {
				srtConfig := srt.DefaultConfig()
				srtConfig.StreamId = ""
				listener, err := srt.Listen("srt", fmt.Sprintf(":%d", config.SRTPort), srtConfig)
				if err != nil {
					return err
				}
				markUp(ProtocolSRT)
				acceptSRT(listener)
				return nil
			})
		})
	}
}

func markUp(protocol string) {
	listeners.Lock()
	listeners.up[protocol] = true
	listeners.Unlock()
}

func keepListening(protocol string, serve func() error) {
	for {
		err := serve()
		listeners.Lock()
		listeners.up[protocol] = false
		listeners.Unlock()
		if err != nil {
			log.Printf("media ingest %s listener: %v (retrying)", protocol, err)
		}
		time.Sleep(listenerRetry)
	}
}

func acceptRTMP(listener net.Listener, protocol string) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go handleRTMP(conn, protocol)
	}
}

func handleRTMP(conn net.Conn, protocol string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	server := &gortmplib.ServerConn{RW: conn}
	if err := server.Initialize(); err != nil {
		return
	}
	if err := server.AcceptConn(); err != nil {
		return
	}
	if !server.Publish || server.URL == nil {
		_ = server.RejectAction()
		return
	}
	key := path.Base(server.URL.Path)
	route, _ := routeKey("", key)
	role, keyID, keySHA := route.role, route.keyID, route.keySHA
	if role == nil || !role.allows(protocol) {
		_ = server.RejectAction()
		return
	}
	reader := &gortmplib.Reader{Conn: server}
	if err := reader.Initialize(); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	var tracks []TrackInfo
	type binding struct {
		track *gortmplib.Track
		index int
	}
	var bindings []binding
	for _, track := range reader.Tracks() {
		var info TrackInfo
		switch codec := track.Codec.(type) {
		case *rtmpcodecs.H264:
			info = TrackInfo{Video: true, Codec: CodecH264}
		case *rtmpcodecs.H265:
			info = TrackInfo{Video: true, Codec: CodecH265}
		case *rtmpcodecs.MPEG4Audio:
			config := *codec.Config
			info = TrackInfo{Codec: CodecAAC, AAC: &config}
		case *rtmpcodecs.Opus:
			channels := 2
			if codec.IDHeader != nil {
				channels = int(codec.IDHeader.ChannelCount)
			}
			info = TrackInfo{Codec: CodecOpus, OpusChannels: channels}
		case *rtmpcodecs.MPEG1Audio:
			info = TrackInfo{Codec: CodecMP3}
		default:
			continue
		}
		bindings = append(bindings, binding{track: track, index: len(tracks)})
		tracks = append(tracks, info)
	}
	if len(tracks) == 0 {
		return
	}
	publisher, _ := role.startPublisher(protocol, keyID, keySHA, admission{}, func(string) { _ = conn.Close() }, tracks, nil)
	for _, bound := range bindings {
		index := bound.index
		switch tracks[index].Codec {
		case CodecH264:
			parameters := &parameterSets{}
			if codec, ok := bound.track.Codec.(*rtmpcodecs.H264); ok {
				parameters.sps, parameters.pps = codec.SPS, codec.PPS
			}
			reader.OnDataH264(bound.track, func(pts, dts time.Duration, au [][]byte) {
				role.write(publisher, Unit{Track: index, PTS: ticks(pts), DTS: ticks(dts), Key: h264.IsRandomAccess(au),
					AU: parameters.apply(au)})
			})
		case CodecH265:
			reader.OnDataH265(bound.track, func(pts, dts time.Duration, au [][]byte) {
				role.write(publisher, Unit{Track: index, PTS: ticks(pts), DTS: ticks(dts), Key: h265.IsRandomAccess(au), AU: au})
			})
		case CodecAAC:
			reader.OnDataMPEG4Audio(bound.track, func(pts time.Duration, au []byte) {
				role.write(publisher, Unit{Track: index, PTS: ticks(pts), DTS: ticks(pts), Key: true, AU: [][]byte{au}})
			})
		case CodecOpus:
			reader.OnDataOpus(bound.track, func(pts time.Duration, packet []byte) {
				role.write(publisher, Unit{Track: index, PTS: ticks(pts), DTS: ticks(pts), Key: true, AU: [][]byte{packet}})
			})
		case CodecMP3:
			reader.OnDataMPEG1Audio(bound.track, func(pts time.Duration, frame []byte) {
				role.write(publisher, Unit{Track: index, PTS: ticks(pts), DTS: ticks(pts), Key: true, AU: [][]byte{frame}})
			})
		}
	}
	var err error
	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		if err = reader.Read(); err != nil {
			break
		}
	}
	role.endPublisher(publisher, err)
}

func ticks(value time.Duration) int64 { return int64(value) * 90000 / int64(time.Second) }

func acceptSRT(listener srt.Listener) {
	for {
		request, err := listener.Accept2()
		if err != nil {
			if errors.Is(err, srt.ErrListenerClosed) {
				return
			}
			continue
		}
		go handleSRTRequest(request)
	}
}

func parseStreamID(streamID string) (session, mode, key string) {
	if !strings.HasPrefix(streamID, "#!::") {
		return "", "publish", streamID
	}
	mode = "request"
	for _, item := range strings.Split(strings.TrimPrefix(streamID, "#!::"), ",") {
		name, value, _ := strings.Cut(item, "=")
		switch strings.TrimSpace(name) {
		case "r":
			session = value
		case "m":
			mode = value
		case "s":
			key = value
		}
	}
	return session, mode, key
}

func handleSRTRequest(request srt.ConnRequest) {
	session, mode, key := parseStreamID(request.StreamId())
	if mode != "publish" {
		request.Reject(srt.REJX_BAD_MODE)
		return
	}
	if request.IsEncrypted() {
		request.Reject(srt.REJ_UNSECURE)
		return
	}
	route, matched := routeKey(session, key)
	role, keyID, keySHA := route.role, route.keyID, route.keySHA
	if role == nil && matched {
		request.Reject(srt.REJX_DOWN)
		return
	}
	if role == nil || !role.allows(ProtocolSRT) {
		request.Reject(srt.REJX_UNAUTHORIZED)
		return
	}
	conn, err := request.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	reader := &mpegts.Reader{R: conn}
	if err := reader.Initialize(); err != nil {
		return
	}
	reader.OnDecodeError(func(error) {})
	var tracks []TrackInfo
	type binding struct {
		track *mpegts.Track
		index int
	}
	var bindings []binding
	for _, track := range reader.Tracks() {
		var info TrackInfo
		switch codec := track.Codec.(type) {
		case *tscodecs.H264:
			info = TrackInfo{Video: true, Codec: CodecH264}
		case *tscodecs.H265:
			info = TrackInfo{Video: true, Codec: CodecH265}
		case *tscodecs.MPEG4Audio:
			config := codec.Config
			info = TrackInfo{Codec: CodecAAC, AAC: &config}
		case *tscodecs.Opus:
			channels := 2
			if codec.Desc != nil && codec.Desc.ChannelConfigCode > 0 && codec.Desc.ChannelConfigCode <= 8 {
				channels = int(codec.Desc.ChannelConfigCode)
			}
			info = TrackInfo{Codec: CodecOpus, OpusChannels: channels}
		case *tscodecs.MPEG1Audio:
			info = TrackInfo{Codec: CodecMP3}
		default:
			continue
		}
		bindings = append(bindings, binding{track: track, index: len(tracks)})
		tracks = append(tracks, info)
	}
	if len(tracks) == 0 {
		return
	}
	publisher, _ := role.startPublisher(ProtocolSRT, keyID, keySHA, admission{}, func(string) { _ = conn.Close() }, tracks, nil)
	for _, bound := range bindings {
		index := bound.index
		switch tracks[index].Codec {
		case CodecH264:
			reader.OnDataH264(bound.track, func(pts, dts int64, au [][]byte) error {
				role.write(publisher, Unit{Track: index, PTS: pts, DTS: dts, Key: h264.IsRandomAccess(au), AU: au})
				return nil
			})
		case CodecH265:
			reader.OnDataH265(bound.track, func(pts, dts int64, au [][]byte) error {
				role.write(publisher, Unit{Track: index, PTS: pts, DTS: dts, Key: h265.IsRandomAccess(au), AU: au})
				return nil
			})
		case CodecAAC:
			reader.OnDataMPEG4Audio(bound.track, func(pts int64, aus [][]byte) error {
				role.write(publisher, Unit{Track: index, PTS: pts, DTS: pts, Key: true, AU: aus})
				return nil
			})
		case CodecOpus:
			reader.OnDataOpus(bound.track, func(pts int64, packets [][]byte) error {
				role.write(publisher, Unit{Track: index, PTS: pts, DTS: pts, Key: true, AU: packets})
				return nil
			})
		case CodecMP3:
			reader.OnDataMPEG1Audio(bound.track, func(pts int64, frames [][]byte) error {
				role.write(publisher, Unit{Track: index, PTS: pts, DTS: pts, Key: true, AU: frames})
				return nil
			})
		}
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		if err = reader.Read(); err != nil {
			break
		}
	}
	role.endPublisher(publisher, err)
}

type parameterSets struct{ sps, pps []byte }

func (p *parameterSets) apply(au [][]byte) [][]byte {
	idr, hasSPS := false, false
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		switch h264.NALUType(nalu[0] & 0x1f) {
		case h264.NALUTypeSPS:
			p.sps, hasSPS = append([]byte(nil), nalu...), true
		case h264.NALUTypePPS:
			p.pps = append([]byte(nil), nalu...)
		case h264.NALUTypeIDR:
			idr = true
		}
	}
	if !idr || hasSPS || len(p.sps) == 0 || len(p.pps) == 0 {
		return au
	}
	at := 0
	if len(au[0]) > 0 && h264.NALUType(au[0][0]&0x1f) == h264.NALUTypeAccessUnitDelimiter {
		at = 1
	}
	out := make([][]byte, 0, len(au)+2)
	out = append(out, au[:at]...)
	out = append(out, p.sps, p.pps)
	return append(out, au[at:]...)
}
