package sfu

import (
	"errors"
)

const (
	DependencyDescriptorURI = "https://aomediacodec.github.io/av1-rtp-spec/#dependency-descriptor-rtp-header-extension"
	AudioLevelURI           = "urn:ietf:params:rtp-hdrext:ssrc-audio-level"
	AbsSendTimeURI          = "http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time"
)

type vp8Descriptor struct {
	start       bool
	keyframe    bool
	hasTID      bool
	tid         int
	layerSync   bool
	pictureID   uint16
	pictureLen  int
	pictureAt   int
	hasTL0      bool
	tl0         uint8
	tl0At       int
	headerBytes int
	width       int
	height      int
}

var errVP8Descriptor = errors.New("vp8 payload descriptor is truncated")

func parseVP8(payload []byte) (vp8Descriptor, error) {
	var d vp8Descriptor
	if len(payload) < 1 {
		return d, errVP8Descriptor
	}
	index := 0
	first := payload[index]
	extended := first&0x80 != 0
	d.start = first&0x10 != 0 && first&0x07 == 0
	index++
	if extended {
		if len(payload) <= index {
			return d, errVP8Descriptor
		}
		flags := payload[index]
		index++
		hasPicture := flags&0x80 != 0
		hasTL0 := flags&0x40 != 0
		hasTID := flags&0x20 != 0
		hasKeyIdx := flags&0x10 != 0
		if hasPicture {
			if len(payload) <= index {
				return d, errVP8Descriptor
			}
			d.pictureAt = index
			if payload[index]&0x80 != 0 {
				if len(payload) <= index+1 {
					return d, errVP8Descriptor
				}
				d.pictureID = uint16(payload[index]&0x7f)<<8 | uint16(payload[index+1])
				d.pictureLen = 2
				index += 2
			} else {
				d.pictureID = uint16(payload[index] & 0x7f)
				d.pictureLen = 1
				index++
			}
		}
		if hasTL0 {
			if len(payload) <= index {
				return d, errVP8Descriptor
			}
			d.hasTL0 = true
			d.tl0 = payload[index]
			d.tl0At = index
			index++
		}
		if hasTID || hasKeyIdx {
			if len(payload) <= index {
				return d, errVP8Descriptor
			}
			if hasTID {
				d.hasTID = true
				d.tid = int(payload[index] >> 6)
				d.layerSync = payload[index]&0x20 != 0
			}
			index++
		}
	}
	d.headerBytes = index
	if d.start && len(payload) > index {
		frame := payload[index:]
		if frame[0]&0x01 == 0 && len(frame) >= 10 && frame[3] == 0x9d && frame[4] == 0x01 && frame[5] == 0x2a {
			d.keyframe = true
			d.width = int(uint16(frame[6])|uint16(frame[7])<<8) & 0x3fff
			d.height = int(uint16(frame[8])|uint16(frame[9])<<8) & 0x3fff
		}
	}
	return d, nil
}

func rewriteVP8(payload []byte, d vp8Descriptor, pictureID uint16, tl0 uint8) []byte {
	if d.pictureLen == 0 && !d.hasTL0 {
		return payload
	}
	out := make([]byte, len(payload))
	copy(out, payload)
	switch d.pictureLen {
	case 1:
		out[d.pictureAt] = byte(pictureID & 0x7f)
	case 2:
		out[d.pictureAt] = 0x80 | byte((pictureID>>8)&0x7f)
		out[d.pictureAt+1] = byte(pictureID)
	}
	if d.hasTL0 {
		out[d.tl0At] = tl0
	}
	return out
}

type ddTemplates struct {
	offset   int
	spatial  []int
	temporal []int
}

type ddFrame struct {
	startOfFrame bool
	endOfFrame   bool
	templateID   int
	frameNumber  uint16
	structure    *ddTemplates
}

type bitReader struct {
	data []byte
	bit  int
}

var errDDTruncated = errors.New("dependency descriptor is truncated")

func (r *bitReader) read(bits int) (int, error) {
	value := 0
	for range bits {
		index := r.bit / 8
		if index >= len(r.data) {
			return 0, errDDTruncated
		}
		value = value<<1 | int(r.data[index]>>(7-uint(r.bit%8))&1)
		r.bit++
	}
	return value, nil
}

func parseDD(data []byte) (ddFrame, error) {
	var frame ddFrame
	if len(data) < 3 {
		return frame, errDDTruncated
	}
	frame.startOfFrame = data[0]&0x80 != 0
	frame.endOfFrame = data[0]&0x40 != 0
	frame.templateID = int(data[0] & 0x3f)
	frame.frameNumber = uint16(data[1])<<8 | uint16(data[2])
	if len(data) == 3 {
		return frame, nil
	}
	reader := &bitReader{data: data, bit: 24}
	structurePresent, err := reader.read(1)
	if err != nil {
		return frame, err
	}
	if _, err := reader.read(4); err != nil {
		return frame, err
	}
	if structurePresent == 0 {
		return frame, nil
	}
	offset, err := reader.read(6)
	if err != nil {
		return frame, err
	}
	if _, err := reader.read(5); err != nil {
		return frame, err
	}
	templates := &ddTemplates{offset: offset}
	spatial, temporal := 0, 0
	for {
		templates.spatial = append(templates.spatial, spatial)
		templates.temporal = append(templates.temporal, temporal)
		if len(templates.spatial) > 64 {
			return frame, errors.New("dependency descriptor has too many templates")
		}
		next, err := reader.read(2)
		if err != nil {
			return frame, err
		}
		switch next {
		case 1:
			temporal++
		case 2:
			temporal = 0
			spatial++
		case 3:
			frame.structure = templates
			return frame, nil
		}
	}
}

func (t *ddTemplates) layerOf(templateID int) (int, int, bool) {
	if t == nil || len(t.spatial) == 0 {
		return 0, 0, false
	}
	index := (templateID + 64 - t.offset) % 64
	if index >= len(t.spatial) {
		return 0, 0, false
	}
	return t.spatial[index], t.temporal[index], true
}

func rewriteDDFrameNumber(data []byte, frameNumber uint16) []byte {
	out := make([]byte, len(data))
	copy(out, data)
	if len(out) >= 3 {
		out[1] = byte(frameNumber >> 8)
		out[2] = byte(frameNumber)
	}
	return out
}

func redPrimaryData(payload []byte) ([]byte, bool) {
	index := 0
	redundant := 0
	for {
		if index >= len(payload) {
			return nil, false
		}
		header := payload[index]
		if header&0x80 == 0 {
			index++
			break
		}
		if index+4 > len(payload) {
			return nil, false
		}
		length := int(payload[index+2]&0x03)<<8 | int(payload[index+3])
		redundant += length
		index += 4
	}
	if index+redundant > len(payload) {
		return nil, false
	}
	return payload[index+redundant:], true
}

func audioLevel(data []byte) (int, bool, bool) {
	if len(data) < 1 {
		return 0, false, false
	}
	return int(data[0] & 0x7f), data[0]&0x80 != 0, true
}
