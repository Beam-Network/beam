package packager

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"errors"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

var w3cCommonSystemID = []byte{0x10, 0x77, 0xef, 0xec, 0xc0, 0xb2, 0x4d, 0x02, 0xac, 0xe3, 0x3c, 0x1e, 0x52, 0xe2, 0xfb, 0x4b}

type cbcsKey struct {
	kid   []byte
	key   []byte
	iv    []byte
	block cipher.Block
}

func newCBCSKey(kid, key, iv []byte) (*cbcsKey, error) {
	if len(kid) != 16 || len(key) != 16 || len(iv) != 16 {
		return nil, errors.New("cbcs keys, KIDs and IVs are 16 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return &cbcsKey{kid: kid, key: key, iv: iv, block: block}, nil
}

func box(kind string, payload ...[]byte) []byte {
	size := 8
	for _, part := range payload {
		size += len(part)
	}
	out := make([]byte, 0, size)
	out = binary.BigEndian.AppendUint32(out, uint32(size))
	out = append(out, kind...)
	for _, part := range payload {
		out = append(out, part...)
	}
	return out
}

func fullBox(kind string, version byte, flags uint32, payload ...[]byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(kind, append([][]byte{head}, payload...)...)
}

type rawBox struct {
	kind    string
	header  int
	payload []byte
}

func readBoxes(data []byte) ([]rawBox, error) {
	var boxes []rawBox
	for len(data) > 0 {
		if len(data) < 8 {
			return nil, errors.New("truncated box")
		}
		size := uint64(binary.BigEndian.Uint32(data))
		header := 8
		if size == 1 {
			if len(data) < 16 {
				return nil, errors.New("truncated box")
			}
			size, header = binary.BigEndian.Uint64(data[8:]), 16
		} else if size == 0 {
			size = uint64(len(data))
		}
		if size < uint64(header) || size > uint64(len(data)) {
			return nil, errors.New("invalid box size")
		}
		boxes = append(boxes, rawBox{kind: string(data[4:8]), header: header, payload: data[header:size]})
		data = data[size:]
	}
	return boxes, nil
}

func encryptInit(init []byte, key *cbcsKey, video bool) ([]byte, error) {
	boxes, err := readBoxes(init)
	if err != nil {
		return nil, err
	}
	var out []byte
	found := false
	for _, item := range boxes {
		if item.kind != "moov" {
			out = append(out, box(item.kind, item.payload)...)
			continue
		}
		moov, err := rewriteContainer(item.payload, key, video, &found)
		if err != nil {
			return nil, err
		}
		pssh := fullBox("pssh", 1, 0, w3cCommonSystemID, binary.BigEndian.AppendUint32(nil, 1), key.kid, binary.BigEndian.AppendUint32(nil, 0))
		out = append(out, box("moov", moov, pssh)...)
	}
	if !found {
		return nil, errors.New("init segment has no sample entry to protect")
	}
	return out, nil
}

var containers = map[string]bool{"trak": true, "mdia": true, "minf": true, "stbl": true}

func rewriteContainer(payload []byte, key *cbcsKey, video bool, found *bool) ([]byte, error) {
	children, err := readBoxes(payload)
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, child := range children {
		switch {
		case containers[child.kind]:
			rewritten, err := rewriteContainer(child.payload, key, video, found)
			if err != nil {
				return nil, err
			}
			out = append(out, box(child.kind, rewritten)...)
		case child.kind == "stsd":
			rewritten, err := rewriteSTSD(child.payload, key, video, found)
			if err != nil {
				return nil, err
			}
			out = append(out, box("stsd", rewritten)...)
		default:
			out = append(out, box(child.kind, child.payload)...)
		}
	}
	return out, nil
}

func rewriteSTSD(payload []byte, key *cbcsKey, video bool, found *bool) ([]byte, error) {
	if len(payload) < 8 {
		return nil, errors.New("truncated stsd")
	}
	entries, err := readBoxes(payload[8:])
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), payload[:8]...)
	for _, entry := range entries {
		protectedKind := "enca"
		if video {
			protectedKind = "encv"
		}
		var crypt, skip byte
		if video {
			crypt, skip = 1, 9
		}
		tenc := fullBox("tenc", 1, 0, []byte{0, crypt<<4 | skip, 1, 0}, key.kid, []byte{16}, key.iv)
		schm := fullBox("schm", 0, 0, []byte("cbcs"), binary.BigEndian.AppendUint32(nil, 0x00010000))
		sinf := box("sinf", box("frma", []byte(entry.kind)), schm, box("schi", tenc))
		out = append(out, box(protectedKind, entry.payload, sinf)...)
		*found = true
	}
	return out, nil
}

func encryptFragment(fragment []byte, key *cbcsKey, video, h265 bool) ([]byte, error) {
	var parts fmp4.Parts
	if err := parts.Unmarshal(fragment); err != nil {
		return nil, err
	}
	if len(parts) != 1 || len(parts[0].Tracks) != 1 {
		return nil, errors.New("a protected fragment holds one part of one track")
	}
	part, track := parts[0], parts[0].Tracks[0]
	type subsample struct {
		clear     uint16
		protected uint32
	}
	samples := track.Samples
	payloads := make([][]byte, len(samples))
	subsamples := make([][]subsample, len(samples))
	for i, sample := range samples {
		payload := append([]byte(nil), sample.Payload...)
		if video {
			var entries []subsample
			var clearRun int
			for offset := 0; offset < len(payload); {
				if offset+4 > len(payload) {
					clearRun += len(payload) - offset
					break
				}
				size := int(binary.BigEndian.Uint32(payload[offset:]))
				total := 4 + size
				if size <= 0 || offset+total > len(payload) {
					clearRun += len(payload) - offset
					break
				}
				nalu := payload[offset+4 : offset+total]
				protected := 0
				if vcl(nalu, h265) {
					const clearPrefix = 4 + 64
					if total > clearPrefix {
						protected = (total - clearPrefix) / 16 * 16
					}
				}
				clearBytes := total - protected
				if protected > 0 {
					encryptPattern(key, payload[offset+clearBytes:offset+total], 1, 9)
					clearRun += clearBytes
					for clearRun > 0xffff {
						entries = append(entries, subsample{clear: 0xffff})
						clearRun -= 0xffff
					}
					entries = append(entries, subsample{clear: uint16(clearRun), protected: uint32(protected)})
					clearRun = 0
				} else {
					clearRun += clearBytes
				}
				offset += total
			}
			for clearRun > 0 {
				chunk := min(clearRun, 0xffff)
				entries = append(entries, subsample{clear: uint16(chunk)})
				clearRun -= chunk
			}
			subsamples[i] = entries
		} else {
			encryptPattern(key, payload[:len(payload)/16*16], 0, 0)
		}
		payloads[i] = payload
	}
	count := uint32(len(samples))
	var sencBody []byte
	sencBody = binary.BigEndian.AppendUint32(sencBody, count)
	infoSizes := make([]byte, len(samples))
	sencFlags := uint32(0)
	if video {
		sencFlags = 0x2
	}
	for i := range samples {
		if !video {
			continue
		}
		start := len(sencBody)
		sencBody = binary.BigEndian.AppendUint16(sencBody, uint16(len(subsamples[i])))
		for _, entry := range subsamples[i] {
			sencBody = binary.BigEndian.AppendUint16(sencBody, entry.clear)
			sencBody = binary.BigEndian.AppendUint32(sencBody, entry.protected)
		}
		infoSizes[i] = byte(len(sencBody) - start)
	}
	saizBody := []byte{0}
	saizBody = binary.BigEndian.AppendUint32(saizBody, count)
	saizBody = append(saizBody, infoSizes...)
	saiz := fullBox("saiz", 0, 0, saizBody)
	senc := fullBox("senc", 0, sencFlags, sencBody)
	mfhd := fullBox("mfhd", 0, 0, binary.BigEndian.AppendUint32(nil, part.SequenceNumber))
	tfhd := fullBox("tfhd", 0, 0x020000, binary.BigEndian.AppendUint32(nil, uint32(track.ID)))
	tfdt := fullBox("tfdt", 1, 0, binary.BigEndian.AppendUint64(nil, track.BaseTime))
	trunFlags := uint32(0x1 | 0x100 | 0x200 | 0x400 | 0x800)
	trunBody := func(dataOffset int32) []byte {
		body := binary.BigEndian.AppendUint32(nil, count)
		body = binary.BigEndian.AppendUint32(body, uint32(dataOffset))
		for i, sample := range samples {
			body = binary.BigEndian.AppendUint32(body, sample.Duration)
			body = binary.BigEndian.AppendUint32(body, uint32(len(payloads[i])))
			flags := uint32(0x02000000)
			if sample.IsNonSyncSample {
				flags = 0x01010000
			}
			body = binary.BigEndian.AppendUint32(body, flags)
			body = binary.BigEndian.AppendUint32(body, uint32(sample.PTSOffset))
		}
		return body
	}
	saioPlaceholder := fullBox("saio", 0, 0, binary.BigEndian.AppendUint32(nil, 1), binary.BigEndian.AppendUint32(nil, 0))
	trun := fullBox("trun", 1, trunFlags, trunBody(0))
	traf := box("traf", tfhd, tfdt, trun, saiz, saioPlaceholder, senc)
	moof := box("moof", mfhd, traf)
	moofSize := len(moof)
	sencDataOffset := 8 + len(mfhd) + 8 + len(tfhd) + len(tfdt) + len(trun) + len(saiz) + len(saioPlaceholder) + 16
	saio := fullBox("saio", 0, 0, binary.BigEndian.AppendUint32(nil, 1), binary.BigEndian.AppendUint32(nil, uint32(sencDataOffset)))
	var mdatBody []byte
	for _, payload := range payloads {
		mdatBody = append(mdatBody, payload...)
	}
	trun = fullBox("trun", 1, trunFlags, trunBody(int32(moofSize+8)))
	moof = box("moof", mfhd, box("traf", tfhd, tfdt, trun, saiz, saio, senc))
	return append(moof, box("mdat", mdatBody)...), nil
}

func vcl(nalu []byte, h265 bool) bool {
	if len(nalu) == 0 {
		return false
	}
	if h265 {
		return (nalu[0]>>1)&0x3f < 32
	}
	kind := nalu[0] & 0x1f
	return kind >= 1 && kind <= 5
}

func encryptPattern(key *cbcsKey, data []byte, crypt, skip int) {
	encrypter := cipher.NewCBCEncrypter(key.block, key.iv)
	blocks := len(data) / 16
	for i := range blocks {
		if crypt > 0 && i%(crypt+skip) >= crypt {
			continue
		}
		encrypter.CryptBlocks(data[i*16:(i+1)*16], data[i*16:(i+1)*16])
	}
}

func (k *cbcsKey) playlistKeyLine() string {
	return `#EXT-X-KEY:METHOD=SAMPLE-AES,URI="data:text/plain;base64,` + base64.StdEncoding.EncodeToString(k.kid) +
		`",KEYFORMAT="org.w3.clearkey",KEYFORMATVERSIONS="1"`
}
