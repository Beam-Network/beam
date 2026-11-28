package transcode

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
)

type mkvWriter struct {
	w           io.Writer
	cluster     int64
	haveCluster bool
}

type mkvTrack struct {
	number       uint64
	video        bool
	codecID      string
	codecPrivate []byte
	width        uint64
	height       uint64
	channels     uint64
}

var (
	idEBML               = []byte{0x1A, 0x45, 0xDF, 0xA3}
	idEBMLVersion        = []byte{0x42, 0x86}
	idEBMLReadVersion    = []byte{0x42, 0xF7}
	idEBMLMaxIDLength    = []byte{0x42, 0xF2}
	idEBMLMaxSizeLength  = []byte{0x42, 0xF3}
	idDocType            = []byte{0x42, 0x82}
	idDocTypeVersion     = []byte{0x42, 0x87}
	idDocTypeReadVersion = []byte{0x42, 0x85}
	idSegment            = []byte{0x18, 0x53, 0x80, 0x67}
	idInfo               = []byte{0x15, 0x49, 0xA9, 0x66}
	idTimestampScale     = []byte{0x2A, 0xD7, 0xB1}
	idMuxingApp          = []byte{0x4D, 0x80}
	idWritingApp         = []byte{0x57, 0x41}
	idTracks             = []byte{0x16, 0x54, 0xAE, 0x6B}
	idTrackEntry         = []byte{0xAE}
	idTrackNumber        = []byte{0xD7}
	idTrackUID           = []byte{0x73, 0xC5}
	idTrackType          = []byte{0x83}
	idFlagLacing         = []byte{0x9C}
	idCodecID            = []byte{0x86}
	idCodecPrivate       = []byte{0x63, 0xA2}
	idVideo              = []byte{0xE0}
	idPixelWidth         = []byte{0xB0}
	idPixelHeight        = []byte{0xBA}
	idAudio              = []byte{0xE1}
	idSamplingFrequency  = []byte{0xB5}
	idChannels           = []byte{0x9F}
	idCluster            = []byte{0x1F, 0x43, 0xB6, 0x75}
	idClusterTimestamp   = []byte{0xE7}
	idSimpleBlock        = []byte{0xA3}
)

var unknownSize = []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

func vintSize(size uint64) []byte {
	for length := 1; length <= 8; length++ {
		if size < (uint64(1)<<(7*length))-1 {
			out := make([]byte, length)
			value := size | uint64(1)<<(7*length)
			for i := length - 1; i >= 0; i-- {
				out[i] = byte(value)
				value >>= 8
			}
			return out
		}
	}
	return unknownSize
}

func element(id []byte, payload ...[]byte) []byte {
	var body bytes.Buffer
	for _, part := range payload {
		body.Write(part)
	}
	out := append([]byte(nil), id...)
	out = append(out, vintSize(uint64(body.Len()))...)
	return append(out, body.Bytes()...)
}

func uintElement(id []byte, value uint64) []byte {
	buffer := make([]byte, 8)
	binary.BigEndian.PutUint64(buffer, value)
	start := 0
	for start < 7 && buffer[start] == 0 {
		start++
	}
	return element(id, buffer[start:])
}

func floatElement(id []byte, value float64) []byte {
	buffer := make([]byte, 8)
	binary.BigEndian.PutUint64(buffer, math.Float64bits(value))
	return element(id, buffer)
}

func (m *mkvWriter) writeHeader(tracks []mkvTrack) error {
	header := element(idEBML, uintElement(idEBMLVersion, 1), uintElement(idEBMLReadVersion, 1),
		uintElement(idEBMLMaxIDLength, 4), uintElement(idEBMLMaxSizeLength, 8), element(idDocType, []byte("matroska")),
		uintElement(idDocTypeVersion, 4), uintElement(idDocTypeReadVersion, 2))
	info := element(idInfo, uintElement(idTimestampScale, 1_000_000), element(idMuxingApp, []byte("beam-media")),
		element(idWritingApp, []byte("beam-media")))
	var entries [][]byte
	for _, track := range tracks {
		fields := [][]byte{uintElement(idTrackNumber, track.number), uintElement(idTrackUID, track.number),
			uintElement(idFlagLacing, 0), element(idCodecID, []byte(track.codecID))}
		if track.video {
			fields = append(fields, uintElement(idTrackType, 1),
				element(idVideo, uintElement(idPixelWidth, track.width), uintElement(idPixelHeight, track.height)))
		} else {
			fields = append(fields, uintElement(idTrackType, 2),
				element(idAudio, floatElement(idSamplingFrequency, 48000), uintElement(idChannels, track.channels)))
		}
		if len(track.codecPrivate) > 0 {
			fields = append(fields, element(idCodecPrivate, track.codecPrivate))
		}
		entries = append(entries, element(idTrackEntry, fields...))
	}
	out := append(header, idSegment...)
	out = append(out, unknownSize...)
	out = append(out, info...)
	out = append(out, element(idTracks, entries...)...)
	_, err := m.w.Write(out)
	return err
}

func (m *mkvWriter) writeBlock(track uint64, ms int64, key bool, data []byte) error {
	var out []byte
	if !m.haveCluster || ms-m.cluster > 30_000 || ms < m.cluster || (key && track == 1) {
		m.cluster, m.haveCluster = ms, true
		out = append(out, idCluster...)
		out = append(out, unknownSize...)
		out = append(out, uintElement(idClusterTimestamp, uint64(max(ms, 0)))...)
	}
	block := make([]byte, 0, len(data)+4)
	block = append(block, vintSize(track)...)
	relative := int16(ms - m.cluster)
	block = append(block, byte(uint16(relative)>>8), byte(uint16(relative)))
	flags := byte(0)
	if key {
		flags = 0x80
	}
	block = append(block, flags)
	block = append(block, data...)
	out = append(out, element(idSimpleBlock, block)...)
	_, err := m.w.Write(out)
	return err
}

func opusHead(channels int) []byte {
	head := []byte("OpusHead")
	head = append(head, 1, byte(channels))
	head = binary.LittleEndian.AppendUint16(head, 3840)
	head = binary.LittleEndian.AppendUint32(head, 48000)
	head = binary.LittleEndian.AppendUint16(head, 0)
	return append(head, 0)
}

func avcConfiguration(sps, pps []byte) []byte {
	if len(sps) < 4 {
		return nil
	}
	out := []byte{1, sps[1], sps[2], sps[3], 0xFF, 0xE1}
	out = binary.BigEndian.AppendUint16(out, uint16(len(sps)))
	out = append(out, sps...)
	out = append(out, 1)
	out = binary.BigEndian.AppendUint16(out, uint16(len(pps)))
	return append(out, pps...)
}
