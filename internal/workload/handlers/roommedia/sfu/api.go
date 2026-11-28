package sfu

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/Beam-Network/beam/internal/worker/planeapi/shared"
)

type Network struct {
	PublicIP   string
	UDPPortMin uint16
	UDPPortMax uint16
	ReceiveMTU uint
	CPU        func() float64
}

const (
	defaultReceiveMTU    = 1500
	eventMessageMaxBytes = 262_144
	nackBufferPackets    = 1024
	gccInitialBitrate    = 1_000_000
	gccMinBitrate        = 100_000
	gccMaxBitrate        = 30_000_000
)

const (
	ptOpus = 111
	ptRED  = 63
	ptVP8  = 96
	ptVP8R = 97
	ptVP9  = 98
	ptVP9R = 99
	ptH264 = 102
	ptH26R = 103
	ptAV1  = 45
	ptAV1R = 46
	ptH265 = 49
	ptH25R = 50
)

const (
	opusFmtp = "minptime=10;useinbandfec=1;usedtx=1"
	redFmtp  = "111/111"
)

type APIFactory struct {
	network Network
	logger  logging.LoggerFactory

	mu        sync.Mutex
	mux       ice.UDPMux
	conn      net.PacketConn
	publisher map[bool]*webrtc.API
	upstream  map[bool]*webrtc.API
	padding   *paddingBudget
}

func NewAPIFactory(network Network) (*APIFactory, error) {
	if (network.UDPPortMin == 0) != (network.UDPPortMax == 0) || network.UDPPortMin > network.UDPPortMax {
		return nil, errors.New("sfu: invalid UDP port range")
	}
	if network.ReceiveMTU == 0 {
		network.ReceiveMTU = defaultReceiveMTU
	}
	return &APIFactory{network: network, logger: logging.NewDefaultLoggerFactory(),
		publisher: map[bool]*webrtc.API{}, upstream: map[bool]*webrtc.API{}, padding: newPaddingBudget(nodePaddingBPS, network.CPU)}, nil
}

func (f *APIFactory) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mux != nil {
		err := f.mux.Close()
		f.mux, f.conn = nil, nil
		return err
	}
	return nil
}

func (f *APIFactory) settings() (webrtc.SettingEngine, error) {
	settings := webrtc.SettingEngine{}
	settings.SetReceiveMTU(f.network.ReceiveMTU)
	settings.SetSCTPMaxMessageSize(eventMessageMaxBytes)
	if f.network.PublicIP != "" {
		if err := settings.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{External: []string{f.network.PublicIP},
			AsCandidateType: webrtc.ICECandidateTypeHost}); err != nil {
			return settings, err
		}
	}
	if f.network.UDPPortMin != 0 {
		mux, err := f.udpMux()
		if err != nil {
			return settings, err
		}
		settings.SetICEUDPMux(mux)
	}
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	return settings, nil
}

func (f *APIFactory) Bind() error {
	if f.network.UDPPortMin == 0 {
		return nil
	}
	_, err := f.udpMux()
	return err
}

func (f *APIFactory) udpMux() (ice.UDPMux, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mux != nil {
		return f.mux, nil
	}
	var lastErr error
	for port := int(f.network.UDPPortMax); port >= int(f.network.UDPPortMin); port-- {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
		if err != nil {
			lastErr = err
			continue
		}
		f.conn = conn
		shared.Do(func() { f.mux = webrtc.NewICEUDPMux(f.logger.NewLogger("ice"), conn) })
		return f.mux, nil
	}
	return nil, fmt.Errorf("sfu: no free UDP port in %d-%d: %w", f.network.UDPPortMin, f.network.UDPPortMax, lastErr)
}

func mediaEngine(e2ee bool) (*webrtc.MediaEngine, error) {
	engine := &webrtc.MediaEngine{}
	videoFeedback := []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBGoogREMB}, {Type: webrtc.TypeRTCPFBTransportCC},
		{Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"}, {Type: webrtc.TypeRTCPFBNACK}, {Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"}}
	audioFeedback := []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBTransportCC}}
	audio := []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: opusFmtp, RTCPFeedback: audioFeedback}, PayloadType: ptOpus},
	}
	if !e2ee {
		audio = append(audio, webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mimeTypeRED,
			ClockRate: 48000, Channels: 2, SDPFmtpLine: redFmtp, RTCPFeedback: audioFeedback}, PayloadType: ptRED})
	}
	for _, codec := range audio {
		if err := engine.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, err
		}
	}
	type videoCodec struct {
		mime string
		fmtp string
		pt   webrtc.PayloadType
		rtx  webrtc.PayloadType
	}
	videos := []videoCodec{{webrtc.MimeTypeVP8, "", ptVP8, ptVP8R}}
	if !e2ee {
		videos = append(videos,
			videoCodec{webrtc.MimeTypeVP9, "profile-id=0", ptVP9, ptVP9R},
			videoCodec{webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", ptH264, ptH26R},
			videoCodec{webrtc.MimeTypeAV1, "", ptAV1, ptAV1R},
			videoCodec{webrtc.MimeTypeH265, "", ptH265, ptH25R})
	}
	for _, video := range videos {
		if err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: video.mime, ClockRate: 90000, SDPFmtpLine: video.fmtp, RTCPFeedback: videoFeedback}, PayloadType: video.pt},
			webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
		if err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: fmt.Sprintf("apt=%d", video.pt)}, PayloadType: video.rtx},
			webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}
	extensions := []struct {
		uri  string
		kind webrtc.RTPCodecType
	}{
		{sdp.SDESMidURI, webrtc.RTPCodecTypeAudio}, {sdp.SDESMidURI, webrtc.RTPCodecTypeVideo},
		{sdp.SDESRTPStreamIDURI, webrtc.RTPCodecTypeVideo}, {sdp.SDESRepairRTPStreamIDURI, webrtc.RTPCodecTypeVideo},
		{AudioLevelURI, webrtc.RTPCodecTypeAudio}, {DependencyDescriptorURI, webrtc.RTPCodecTypeVideo},
		{AbsSendTimeURI, webrtc.RTPCodecTypeVideo},
	}
	for _, extension := range extensions {
		if err := engine.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: extension.uri}, extension.kind); err != nil {
			return nil, err
		}
	}
	return engine, nil
}

const mimeTypeRED = "audio/red"

func receivingInterceptors(engine *webrtc.MediaEngine, registry *interceptor.Registry) error {
	if err := webrtc.ConfigureNack(engine, registry); err != nil {
		return err
	}
	if err := webrtc.ConfigureRTCPReports(registry); err != nil {
		return err
	}
	return webrtc.ConfigureTWCCSender(engine, registry)
}

func (f *APIFactory) Publisher(e2ee bool) (*webrtc.API, error) {
	return f.receiving(e2ee, f.publisher)
}

func (f *APIFactory) Upstream(e2ee bool) (*webrtc.API, error) {
	return f.receiving(e2ee, f.upstream)
}

func (f *APIFactory) receiving(e2ee bool, cache map[bool]*webrtc.API) (*webrtc.API, error) {
	api := func() *webrtc.API {
		f.mu.Lock()
		defer f.mu.Unlock()
		return cache[e2ee]
	}()
	if api != nil {
		return api, nil
	}
	engine, err := mediaEngine(e2ee)
	if err != nil {
		return nil, err
	}
	registry := &interceptor.Registry{}
	if err := receivingInterceptors(engine, registry); err != nil {
		return nil, err
	}
	settings, err := f.settings()
	if err != nil {
		return nil, err
	}
	api = webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry), webrtc.WithSettingEngine(settings))
	f.mu.Lock()
	defer f.mu.Unlock()
	if cached := cache[e2ee]; cached != nil {
		return cached, nil
	}
	cache[e2ee] = api
	return api, nil
}

type SubscriberAPI struct {
	API       *webrtc.API
	estimator cc.BandwidthEstimator
	pacer     *repairPacer
	nack      *nackLimiter
	padding   *paddingBudget
	wire      atomic.Int64
	mu        sync.Mutex
}

func (s *SubscriberAPI) setRTT(rtt time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	limiter := s.nack
	s.mu.Unlock()
	if limiter != nil {
		limiter.setRTT(rtt)
	}
}

func (s *SubscriberAPI) forgetStream(rtx, media uint32) {
	if s == nil {
		return
	}
	s.mu.Lock()
	pacer := s.pacer
	s.mu.Unlock()
	if pacer != nil {
		pacer.forget(rtx, media)
	}
}

func (s *SubscriberAPI) repairStream(rtx, media uint32) {
	if s == nil || rtx == 0 {
		return
	}
	s.mu.Lock()
	pacer := s.pacer
	s.mu.Unlock()
	if pacer != nil {
		pacer.alias(rtx, media)
	}
}

type repairPacer struct {
	mu      sync.Mutex
	writers map[uint32]interceptor.RTPWriter
	repair  map[uint32]uint32
}

func newRepairPacer() *repairPacer {
	return &repairPacer{writers: map[uint32]interceptor.RTPWriter{}, repair: map[uint32]uint32{}}
}

func (p *repairPacer) AddStream(ssrc uint32, writer interceptor.RTPWriter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writers[ssrc] = writer
}

func (p *repairPacer) alias(rtx, media uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.repair[rtx] = media
}

func (p *repairPacer) forget(rtx, media uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.writers, media)
	if rtx != 0 {
		delete(p.repair, rtx)
	}
}

func (p *repairPacer) SetTargetBitrate(int) {}

func (p *repairPacer) Write(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
	p.mu.Lock()
	writer, ok := p.writers[header.SSRC]
	if !ok {
		if media, repairs := p.repair[header.SSRC]; repairs {
			writer, ok = p.writers[media]
		}
	}
	p.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("%w: %d", gcc.ErrUnknownStream, header.SSRC)
	}
	return writer.Write(header, payload, attributes)
}

func (p *repairPacer) Close() error { return nil }

func (s *SubscriberAPI) Estimator() cc.BandwidthEstimator {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.estimator
}

func (f *APIFactory) Subscriber(e2ee bool) (*SubscriberAPI, error) {
	engine, err := mediaEngine(e2ee)
	if err != nil {
		return nil, err
	}
	registry, result, err := subscriberInterceptors(engine)
	if err != nil {
		return nil, err
	}
	result.padding = f.padding
	settings, err := f.settings()
	if err != nil {
		return nil, err
	}
	result.API = webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry), webrtc.WithSettingEngine(settings))
	return result, nil
}

func subscriberInterceptors(engine *webrtc.MediaEngine) (*interceptor.Registry, *SubscriberAPI, error) {
	registry := &interceptor.Registry{}
	result := &SubscriberAPI{}
	registry.Add(wireCounterFactory{total: &result.wire})
	registry.Add(nackLimiterFactory{api: result})
	responder, err := nack.NewResponderInterceptor(nack.ResponderSize(nackBufferPackets))
	if err != nil {
		return nil, nil, err
	}
	engine.RegisterFeedback(webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBNACK}, webrtc.RTPCodecTypeVideo)
	engine.RegisterFeedback(webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"}, webrtc.RTPCodecTypeVideo)
	congestion, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		pacer := newRepairPacer()
		result.mu.Lock()
		result.pacer = pacer
		result.mu.Unlock()
		return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(gccInitialBitrate), gcc.SendSideBWEMinBitrate(gccMinBitrate),
			gcc.SendSideBWEMaxBitrate(gccMaxBitrate), gcc.SendSideBWEPacer(pacer))
	})
	if err != nil {
		return nil, nil, err
	}
	congestion.OnNewPeerConnection(func(_ string, estimator cc.BandwidthEstimator) {
		result.mu.Lock()
		defer result.mu.Unlock()
		result.estimator = estimator
	})
	registry.Add(congestion)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(engine, registry); err != nil {
		return nil, nil, err
	}
	registry.Add(responder)
	if err := webrtc.ConfigureRTCPReports(registry); err != nil {
		return nil, nil, err
	}
	return registry, result, nil
}
