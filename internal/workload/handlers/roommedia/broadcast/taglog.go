package broadcast

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const tagLogFile = "playback-tags"

type tagEntry struct {
	Nonce    string `json:"n"`
	Identity string `json:"i"`
	SeenMS   int64  `json:"t"`
}

type tagLog struct {
	path      string
	queue     chan tagEntry
	compactAt int
	lost      atomic.Int64
}

const (
	tagQueue     = 4096
	tagCompactAt = 2 * maxTags
	tagLineMax   = 4 << 10
)

func openTagLog(stateDir string) *tagLog {
	if stateDir == "" {
		return &tagLog{}
	}
	return &tagLog{path: filepath.Join(stateDir, tagLogFile), queue: make(chan tagEntry, tagQueue), compactAt: tagCompactAt}
}

func (l *tagLog) load(at int64) []tagEntry {
	if l.path == "" {
		return nil
	}
	entries, _ := readTags(l.path, at)
	_ = writeTags(l.path, entries)
	return entries
}

func readTags(path string, at int64) ([]tagEntry, int) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer file.Close()
	latest := map[string]tagEntry{}
	lines := 0
	reader := bufio.NewReaderSize(file, tagLineMax)
	for {
		line, err := reader.ReadSlice('\n')
		long := errors.Is(err, bufio.ErrBufferFull)
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = reader.ReadSlice('\n')
		}
		var entry tagEntry
		if lines++; !long && json.Unmarshal(line, &entry) == nil && entry.Nonce != "" {
			entry.SeenMS = min(entry.SeenMS, at)
			if at-entry.SeenMS < tagTTL.Milliseconds() && entry.SeenMS >= latest[entry.Nonce].SeenMS {
				latest[entry.Nonce] = entry
			}
		}
		if err != nil {
			break
		}
	}
	entries := make([]tagEntry, 0, len(latest))
	for _, entry := range latest {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].SeenMS < entries[j].SeenMS })
	if len(entries) > maxTags {
		entries = entries[len(entries)-maxTags:]
	}
	return entries, lines
}

func writeTags(path string, entries []tagEntry) error {
	var body strings.Builder
	for _, entry := range entries {
		encoded, _ := json.Marshal(entry)
		body.Write(encoded)
		body.WriteByte('\n')
	}
	return WriteFileDurable(path, []byte(body.String()))
}

func (l *tagLog) append(entry tagEntry) {
	if l.queue == nil {
		return
	}
	select {
	case l.queue <- entry:
	default:
		l.lost.Add(1)
	}
}

func (l *tagLog) run(ctx context.Context, now func() int64) {
	if l.queue == nil {
		return
	}
	var file *os.File
	defer func() {
		if file != nil {
			_ = file.Close()
		}
	}()
	lines := 0
	write := func(entry tagEntry) {
		if file == nil {
			var err error
			if file, err = os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
				l.lost.Add(1)
				return
			}
		}
		encoded, _ := json.Marshal(entry)
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			l.lost.Add(1)
		}
		if lines++; lines >= l.compactAt {
			_ = file.Close()
			file, lines = nil, 0
			entries, _ := readTags(l.path, now())
			_ = writeTags(l.path, entries)
		}
	}
	for {
		select {
		case entry := <-l.queue:
			write(entry)
		case <-ctx.Done():
			for {
				select {
				case entry := <-l.queue:
					write(entry)
				default:
					return
				}
			}
		}
	}
}

var variantResolution = regexp.MustCompile(`[:,]RESOLUTION=([0-9]{1,5})x([0-9]{1,5})`)

func variantSizes(body []byte) map[string]mc.VideoSize {
	sizes := map[string]mc.VideoSize{}
	var size *mc.VideoSize
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			size = nil
			if match := variantResolution.FindStringSubmatch(line); match != nil {
				width, _ := strconv.ParseInt(match[1], 10, 64)
				height, _ := strconv.ParseInt(match[2], 10, 64)
				size = &mc.VideoSize{Width: width, Height: height}
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rendition, _, ok := strings.Cut(line, "/"); ok && size != nil {
			sizes[rendition] = *size
		}
		size = nil
	}
	return sizes
}
