package sfu

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
)

const (
	nackResendFloor   = 20 * time.Millisecond
	nackResendDefault = 100 * time.Millisecond
	nackResendCeiling = time.Second
	nackMaxResends    = 5
	nackHistory       = nackBufferPackets
)

type nackLimiterFactory struct {
	api *SubscriberAPI
}

func (f nackLimiterFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	limiter := newNACKLimiter()
	if f.api != nil {
		f.api.mu.Lock()
		f.api.nack = limiter
		f.api.mu.Unlock()
	}
	return limiter, nil
}

func newNACKLimiter() *nackLimiter {
	return &nackLimiter{streams: map[uint32]*nackStream{}, now: time.Now}
}

type nackLimiter struct {
	interceptor.NoOp
	mu      sync.Mutex
	streams map[uint32]*nackStream
	now     func() time.Time
	rttNano atomic.Int64
}

type nackStream struct {
	entries [nackHistory]nackEntry
}

type nackEntry struct {
	used  bool
	seq   uint16
	last  int64
	count uint8
}

func (n *nackLimiter) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.streams[info.SSRC] == nil {
		n.streams[info.SSRC] = &nackStream{}
	}
	return writer
}

func (n *nackLimiter) UnbindLocalStream(info *interceptor.StreamInfo) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.streams, info.SSRC)
}

func (n *nackLimiter) setRTT(rtt time.Duration) {
	if rtt > 0 {
		n.rttNano.Store(int64(rtt))
	}
}

func (n *nackLimiter) interval() time.Duration {
	rtt := time.Duration(n.rttNano.Load())
	if rtt <= 0 {
		return nackResendDefault
	}
	return min(max(rtt, nackResendFloor), nackResendCeiling)
}

func (n *nackLimiter) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		i, attributes, err := reader.Read(b, a)
		if err != nil {
			return 0, nil, err
		}
		if attributes == nil {
			attributes = make(interceptor.Attributes)
		}
		packets, err := attributes.GetRTCPPackets(b[:i])
		if err != nil {
			return i, attributes, nil
		}
		for _, packet := range packets {
			if nack, ok := packet.(*rtcp.TransportLayerNack); ok {
				n.filter(nack)
			}
		}
		return i, attributes, nil
	})
}

func (n *nackLimiter) filter(nack *rtcp.TransportLayerNack) {
	now := n.now().UnixNano()
	interval := int64(n.interval())
	n.mu.Lock()
	defer n.mu.Unlock()
	stream := n.streams[nack.MediaSSRC]
	if stream == nil {
		nack.Nacks = nil
		return
	}
	var kept []uint16
	for _, pair := range nack.Nacks {
		for _, seq := range pair.PacketList() {
			entry := &stream.entries[int(seq)%nackHistory]
			if !entry.used || entry.seq != seq {
				*entry = nackEntry{used: true, seq: seq}
			}
			if entry.count >= nackMaxResends || entry.count > 0 && now-entry.last < interval {
				continue
			}
			entry.count++
			entry.last = now
			kept = append(kept, seq)
		}
	}
	nack.Nacks = rtcp.NackPairsFromSequenceNumbers(kept)
}
