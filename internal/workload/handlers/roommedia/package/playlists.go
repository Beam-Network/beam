package packager

import (
	"bytes"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type initMark struct {
	uri           string
	open          bool
	discontinuity bool
}

func insertInitMaps(body []byte, marks []initMark) []byte {
	changes := false
	for i := range marks {
		changes = changes || (i > 0 && marks[i].uri != marks[i-1].uri) || (marks[i].open && marks[i].discontinuity)
	}
	if !changes {
		return body
	}
	lines := bytes.SplitAfter(body, []byte("\n"))
	var out bytes.Buffer
	out.Grow(len(body) + 64*len(marks))
	segment := 0
	var pending *initMark
	writeMap := false
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if pending != nil && len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("#EXT-X-DISCONTINUITY")) &&
			!bytes.HasPrefix(trimmed, []byte("#EXT-X-KEY:")) {
			if pending.open && pending.discontinuity {
				out.WriteString("#EXT-X-DISCONTINUITY\n")
			}
			if writeMap {
				out.WriteString("#EXT-X-MAP:URI=" + strconv.Quote(pending.uri) + "\n")
			}
			pending = nil
		}
		out.Write(line)
		if len(trimmed) == 0 || trimmed[0] == '#' {
			continue
		}
		next := segment + 1
		segment++
		if next < len(marks) {
			writeMap = marks[next].uri != marks[next-1].uri
			if writeMap || (marks[next].open && marks[next].discontinuity) {
				pending = &marks[next]
			}
		}
	}
	return out.Bytes()
}

type vodEntry struct {
	uri           string
	init          string
	duration      float64
	discontinuity bool
	key           string
}

func vodPlaylist(mediaSequence int64, entries []vodEntry, independent bool) []byte {
	target := 1
	for _, entry := range entries {
		target = max(target, int(math.Ceil(entry.duration)))
	}
	var out strings.Builder
	out.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	if independent {
		out.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	}
	out.WriteString("#EXT-X-TARGETDURATION:" + strconv.Itoa(target) + "\n")
	out.WriteString("#EXT-X-MEDIA-SEQUENCE:" + strconv.FormatInt(mediaSequence, 10) + "\n")
	out.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	init, key := "", ""
	for _, entry := range entries {
		if entry.discontinuity && init != "" {
			out.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if entry.init != init {
			out.WriteString("#EXT-X-MAP:URI=" + strconv.Quote(entry.init) + "\n")
			init = entry.init
		}
		if entry.key != key {
			if entry.key != "" {
				out.WriteString(entry.key + "\n")
			}
			key = entry.key
		}
		out.WriteString("#EXTINF:" + strconv.FormatFloat(entry.duration, 'f', 5, 64) + ",\n")
		out.WriteString(entry.uri + "\n")
	}
	out.WriteString("#EXT-X-ENDLIST\n")
	return []byte(out.String())
}

func parseVODPlaylist(body []byte) ([]vodEntry, error) {
	var entries []vodEntry
	init, key := "", ""
	discontinuity := false
	duration := -1.0
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			value, ok := quotedAttribute(line, "URI")
			if !ok || !mc.InitURIPattern.MatchString(value) {
				return nil, errors.New("invalid EXT-X-MAP in a recording playlist")
			}
			init = value
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			key = line
		case line == "#EXT-X-DISCONTINUITY":
			discontinuity = true
		case strings.HasPrefix(line, "#EXTINF:"):
			value, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil || parsed < 0 {
				return nil, errors.New("invalid EXTINF in a recording playlist")
			}
			duration = parsed
		case strings.HasPrefix(line, "#"):
		default:
			if duration < 0 || init == "" || !recordedSegmentPattern.MatchString(line) {
				return nil, errors.New("invalid segment in a recording playlist")
			}
			entries = append(entries, vodEntry{uri: line, init: init, duration: duration, discontinuity: discontinuity, key: key})
			discontinuity, duration = false, -1
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("a recording playlist without segments")
	}
	return entries, nil
}

var recordedSegmentPattern = regexp.MustCompile(`^seg_[0-9]{1,20}\.m4s$`)

func quotedAttribute(line, name string) (string, bool) {
	index := strings.Index(line, name+"=\"")
	if index < 0 {
		return "", false
	}
	rest := line[index+len(name)+2:]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}
