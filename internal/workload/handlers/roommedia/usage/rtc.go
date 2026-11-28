package usage

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func (c *Connection) FollowICE(pc *webrtc.PeerConnection, also func(connected bool)) {
	follow := func(state webrtc.ICEConnectionState) {
		connected := connectedICE(state)
		if connected {
			c.Connect()
		} else {
			c.Disconnect()
		}
		if also != nil {
			also(connected)
		}
	}
	pc.OnICEConnectionStateChange(follow)
	if state := pc.ICEConnectionState(); connectedICE(state) {
		follow(state)
	}
}

func connectedICE(state webrtc.ICEConnectionState) bool {
	return state == webrtc.ICEConnectionStateConnected || state == webrtc.ICEConnectionStateCompleted
}

const rtxHeaderBytes = 2

type Stream struct {
	started bool
	highest uint16
	seen    [streamWindow / 64]uint64
}

const streamWindow = 8192

func (s *Stream) first(sequence uint16) bool {
	if !s.started {
		s.started, s.highest = true, sequence
		s.set(sequence)
		return true
	}
	if ahead := int16(sequence - s.highest); ahead > 0 {
		for range min(int(ahead), streamWindow) {
			s.highest++
			s.seen[s.highest%streamWindow/64] &^= 1 << (s.highest % 64)
		}
		s.highest = sequence
		s.set(sequence)
		return true
	} else if -int(ahead) >= streamWindow {
		return false
	}
	if s.seen[sequence%streamWindow/64]&(1<<(sequence%64)) != 0 {
		return false
	}
	s.set(sequence)
	return true
}

func (s *Stream) set(sequence uint16) { s.seen[sequence%streamWindow/64] |= 1 << (sequence % 64) }

func (s *Stream) PacketBytes(packet *rtp.Packet, attributes interceptor.Attributes) (media, overhead int) {
	size := packet.MarshalSize()
	first := s.first(packet.SequenceNumber)
	original := 0
	if attributes.Get(webrtc.AttributeRtxSsrc) != nil {
		original = rtxHeaderBytes
	}
	if !first || len(packet.Payload) == 0 {
		return 0, size + original
	}
	padding := size - packet.Header.MarshalSize() - len(packet.Payload)
	return size - padding, padding + original
}

func (c *Connection) Received(stream *Stream, packet *rtp.Packet, attributes interceptor.Attributes) {
	media, overhead := stream.PacketBytes(packet, attributes)
	c.AddRTP(packet.MarshalSize(), media, overhead)
}
