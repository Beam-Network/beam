package usage

import (
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/av1"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
)

func H264Size(nalu []byte) (width, height int) {
	if len(nalu) < 4 || nalu[0]&0x1f != byte(h264.NALUTypeSPS) {
		return 0, 0
	}
	var sps h264.SPS
	if sps.Unmarshal(nalu) != nil {
		return 0, 0
	}
	return sps.Width(), sps.Height()
}

func H265Size(nalu []byte) (width, height int) {
	if len(nalu) < 4 || nalu[0]>>1&0x3f != byte(h265.NALUType_SPS_NUT) {
		return 0, 0
	}
	var sps h265.SPS
	if sps.Unmarshal(nalu) != nil {
		return 0, 0
	}
	return sps.Width(), sps.Height()
}

func AV1Size(obu []byte) (width, height int) {
	if len(obu) < 4 || obu[0]>>3&0x0f != byte(av1.OBUTypeSequenceHeader) {
		return 0, 0
	}
	var header av1.SequenceHeader
	if header.Unmarshal(obu) != nil {
		return 0, 0
	}
	return header.Width(), header.Height()
}

func RTPFrameSize(codec string, payload []byte) (width, height int) {
	if len(payload) < 2 {
		return 0, 0
	}
	switch codec {
	case "video/h264":
		switch payload[0] & 0x1f {
		case byte(h264.NALUTypeSPS):
			return H264Size(payload)
		case byte(h264.NALUTypeSTAPA):
			return aggregated(payload[1:], H264Size)
		}
	case "video/h265":
		switch payload[0] >> 1 & 0x3f {
		case byte(h265.NALUType_SPS_NUT):
			return H265Size(payload)
		case byte(h265.NALUType_AggregationUnit):
			return aggregated(payload[2:], H265Size)
		}
	case "video/av1":
		if payload[0]&0x08 == 0 {
			return 0, 0
		}
		return av1Elements(payload[1:], int(payload[0]>>4&0x03))
	}
	return 0, 0
}

func aggregated(payload []byte, size func([]byte) (int, int)) (int, int) {
	for len(payload) > 2 {
		length := int(payload[0])<<8 | int(payload[1])
		payload = payload[2:]
		if length == 0 || length > len(payload) {
			return 0, 0
		}
		if width, height := size(payload[:length]); width > 0 {
			return width, height
		}
		payload = payload[length:]
	}
	return 0, 0
}

func av1Elements(payload []byte, count int) (int, int) {
	for index := 1; len(payload) > 0; index++ {
		element := payload
		if count == 0 || index < count {
			var length av1.LEB128
			read, err := length.Unmarshal(payload)
			if err != nil || int(length) > len(payload)-read {
				return 0, 0
			}
			element, payload = payload[read:read+int(length)], payload[read+int(length):]
		} else {
			payload = nil
		}
		if width, height := AV1Size(element); width > 0 {
			return width, height
		}
	}
	return 0, 0
}
