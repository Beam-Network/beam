package packager

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
)

var ErrRecordingStateLost = errors.New("recording_state_lost: the restarted packager lost its local recording index")

type recordedSegment struct {
	msn           int64
	duration      float64
	sha           string
	bytes         int64
	discontinuity bool
	init          string
	initSHA       string
}

type upload struct {
	key       string
	body      []byte
	rendition string
	init      *recordedInit
	segment   *recordedSegment
}

type recordedInit struct {
	uri string
	sha string
}

type pageState struct {
	first int64
	count int
}

type recorder struct {
	node        *broadcast.Node
	emitter     *broadcast.Emitter
	bucket      broadcast.Bucket
	bucketErr   error
	recordingID string
	generation  int64
	prefix      string
	every       time.Duration
	order       []string
	index       *recordingIndex
	key         string
	master      func() ([]byte, error)

	queue       chan upload
	done        chan struct{}
	queueMu     sync.Mutex
	queueClosed bool

	checkpointMu sync.Mutex
	stopLoop     context.CancelFunc
	loopDone     chan struct{}

	mu        sync.Mutex
	segments  map[string][]recordedSegment
	inits     map[string]bool
	resumed   map[string]bool
	bytes     int64
	failure   string
	pages     map[string]map[int]pageState
	masterSHA string
	sequence  int64
}

func newRecorder(env *broadcast.Env, recording mc.NodeRecording, order []string) *recorder {
	return &recorder{node: env.Node, emitter: env.Emitter, bucket: env.Bucket, bucketErr: env.BucketErr,
		recordingID: recording.RecordingID, generation: recording.RecordingGeneration(),
		prefix: mc.RecordingGenerationPrefix(recording.BucketPrefix, recording.RecordingGeneration()),
		every:  env.Config.RecordingCheckpoint, order: order, queue: make(chan upload, 4096), done: make(chan struct{}),
		segments: map[string][]recordedSegment{}, inits: map[string]bool{}, resumed: map[string]bool{},
		pages: map[string]map[int]pageState{}}
}

func (r *recorder) open(ctx context.Context, env *broadcast.Env) error {
	if r.bucket == nil {
		return nil
	}
	if env.StateDir == "" {
		log.Printf("media recording %s g%d: no Worker state directory: a restarted Worker cannot resume this generation",
			r.recordingID, r.generation)
		return nil
	}
	index, err := openRecordingIndex(filepath.Join(env.StateDir, fmt.Sprintf("recording-index-g%d.jsonl", r.generation)),
		env.Resumed)
	if errors.Is(err, ErrRecordingStateLost) {
		log.Printf("media recording %s g%d: the local index is lost: the generation ends here", r.recordingID, r.generation)
		r.emitter.FinalEvent(ctx, mc.RecordingFailedBody{Kind: "recording_failed", RecordingID: r.recordingID, Reason: "recording_state_lost"})
	}
	if err != nil {
		return err
	}
	r.index = index
	if index.resumed {
		r.restore(index.entries)
	}
	return nil
}

func (r *recorder) start(ctx context.Context) bool {
	if r.bucket == nil {
		log.Printf("media recording %s g%d: no bucket: %v", r.recordingID, r.generation, r.bucketErr)
		r.emitter.Event(mc.RecordingFailedBody{Kind: "recording_failed", RecordingID: r.recordingID, Reason: "bucket_unavailable"})
		close(r.done)
		return false
	}
	if r.index == nil || !r.index.resumed {
		r.emitter.Event(mc.RecordingStartedBody{Kind: "recording_started", RecordingID: r.recordingID})
	}
	go r.run(ctx)
	loopCtx, cancel := context.WithCancel(ctx)
	r.stopLoop, r.loopDone = cancel, make(chan struct{})
	go r.checkpoints(loopCtx)
	return true
}

func (r *recorder) restore(entries []indexEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range entries {
		switch entry.Kind {
		case "init":
			r.inits[entry.Rendition+"/"+entry.URI] = true
		case "segment":
			r.segments[entry.Rendition] = append(r.segments[entry.Rendition], recordedSegment{msn: entry.MSN, duration: entry.Duration,
				sha: entry.SHA256, bytes: entry.Bytes, discontinuity: entry.Discontinuity, init: entry.URI, initSHA: entry.InitSHA256})
		}
		r.bytes += entry.Bytes
	}
	for rendition := range r.segments {
		r.resumed[rendition] = true
	}
	log.Printf("media recording %s g%d: resumed with %d bytes stored", r.recordingID, r.generation, r.bytes)
}

func (r *recorder) segment(rend *rendition, seg *segment) {
	if seg.data == nil || seg.init == nil {
		return
	}
	initKey := rend.id + "/" + seg.init.uri
	r.mu.Lock()
	needInit := !r.inits[initKey]
	r.inits[initKey] = true
	discontinuity := seg.discontinuity || r.resumed[rend.id]
	delete(r.resumed, rend.id)
	r.mu.Unlock()
	if needInit {
		r.enqueue(upload{key: r.prefix + initKey, body: seg.init.data, rendition: rend.id,
			init: &recordedInit{uri: seg.init.uri, sha: seg.init.sha}})
	}
	record := &recordedSegment{msn: seg.msn, duration: seg.duration, sha: seg.sha, bytes: seg.bytes, discontinuity: discontinuity,
		init: seg.init.uri, initSHA: seg.init.sha}
	r.enqueue(upload{key: fmt.Sprintf("%s%s/seg_%d.m4s", r.prefix, rend.id, seg.msn), body: seg.data, rendition: rend.id, segment: record})
}

func (r *recorder) enqueue(item upload) {
	r.queueMu.Lock()
	defer r.queueMu.Unlock()
	if r.queueClosed {
		return
	}
	select {
	case r.queue <- item:
	default:
		r.fail("upload_backlog")
	}
}

func (r *recorder) fail(reason string) {
	r.mu.Lock()
	if r.failure == "" {
		r.failure = reason
	}
	r.mu.Unlock()
}

func (r *recorder) run(ctx context.Context) {
	defer close(r.done)
	for item := range r.queue {
		if err := r.put(ctx, item.key, item.body); err != nil {
			log.Printf("media recording %s g%d: %s: %v", r.recordingID, r.generation, item.key, err)
			r.fail("upload_failed")
			r.mu.Lock()
			if item.init != nil {
				delete(r.inits, item.rendition+"/"+item.init.uri)
			}
			if item.segment != nil {
				r.resumed[item.rendition] = true
			}
			r.mu.Unlock()
			continue
		}
		entry := indexEntry{Rendition: item.rendition, Bytes: int64(len(item.body))}
		if item.segment != nil {
			entry.Kind, entry.MSN, entry.Duration, entry.SHA256 = "segment", item.segment.msn, item.segment.duration, item.segment.sha
			entry.Discontinuity, entry.URI, entry.InitSHA256 = item.segment.discontinuity, item.segment.init, item.segment.initSHA
		} else if item.init != nil {
			entry.Kind, entry.URI, entry.SHA256 = "init", item.init.uri, item.init.sha
		}
		if r.index != nil {
			if err := r.index.append(entry); err != nil {
				log.Printf("media recording %s g%d: local index: %v", r.recordingID, r.generation, err)
			}
		}
		r.mu.Lock()
		r.bytes += int64(len(item.body))
		if item.segment != nil {
			r.segments[item.rendition] = append(r.segments[item.rendition], *item.segment)
		}
		r.mu.Unlock()
	}
}

func (r *recorder) put(ctx context.Context, key string, body []byte) error {
	contentType := broadcast.ContentTypeMP4
	switch {
	case strings.HasSuffix(key, ".m3u8"):
		contentType = broadcast.ContentTypePlaylist
	case strings.HasSuffix(key, ".json"):
		contentType = broadcast.ContentTypeJSON
	}
	var err error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		putCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err = r.bucket.Put(putCtx, key, body, contentType)
		cancel()
		if err == nil {
			return nil
		}
	}
	return err
}

func (r *recorder) checkpoints(ctx context.Context) {
	defer close(r.loopDone)
	ticker := time.NewTicker(r.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, _, _, err := r.checkpoint(ctx, false, r.master); err != nil && ctx.Err() == nil {
				log.Printf("media recording %s g%d: checkpoint: %v", r.recordingID, r.generation, err)
			}
		}
	}
}

func (r *recorder) checkpoint(ctx context.Context, complete bool, master func() ([]byte, error)) (int64, int64, []string, error) {
	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()
	if err := r.index.sync(); err != nil {
		log.Printf("media recording %s g%d: sync the local index: %v", r.recordingID, r.generation, err)
	}
	r.mu.Lock()
	snapshot := make(map[string][]recordedSegment, len(r.segments))
	for rendition, segments := range r.segments {
		snapshot[rendition] = append([]recordedSegment(nil), segments...)
	}
	total := r.bytes
	r.mu.Unlock()
	var durationMS int64
	var ids []string
	for _, rendition := range r.renditionOrder(snapshot) {
		segments := snapshot[rendition]
		sort.Slice(segments, func(i, j int) bool { return segments[i].msn < segments[j].msn })
		var seconds float64
		for _, seg := range segments {
			seconds += seg.duration
		}
		durationMS = max(durationMS, int64(seconds*1000))
		ids = append(ids, rendition)
		if err := r.writeRendition(ctx, rendition, segments); err != nil {
			return 0, 0, nil, err
		}
	}
	if len(ids) == 0 {
		return 0, total, nil, nil
	}
	if body, err := master(); err == nil {
		if digest := sha256Hex(body); digest != r.masterSHA {
			if err := r.put(ctx, r.prefix+broadcast.MultivariantName, body); err != nil {
				return 0, 0, nil, err
			}
			r.masterSHA = digest
		}
	} else if complete {
		return 0, 0, nil, err
	}
	if len(ids) > 12 {
		ids = ids[:12]
	}
	file := mc.RecordingFile{SchemaVersion: mc.RecordingFileSchema, RecordingID: r.recordingID, SessionID: r.node.SessionID,
		Generation: r.generation, PackagerNodeID: r.node.NodeID, PackagerNodeKey: r.node.PublicKey(), Complete: complete,
		DurationMS: durationMS, Bytes: total, Renditions: ids, UpdatedAtMS: time.Now().UnixMilli()}
	if err := mc.ValidateRecordingFile(file); err != nil {
		return 0, 0, nil, err
	}
	encoded, err := mc.MarshalWire(file)
	if err != nil {
		return 0, 0, nil, err
	}
	if err := r.put(ctx, r.prefix+"recording.json", encoded); err != nil {
		return 0, 0, nil, err
	}
	return durationMS, total, ids, nil
}

func (r *recorder) renditionOrder(snapshot map[string][]recordedSegment) []string {
	var ids []string
	for _, id := range r.order {
		if len(snapshot[id]) > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func (r *recorder) finish(ctx context.Context, master func() ([]byte, error)) {
	if r.bucket == nil || r.stopLoop == nil {
		return
	}
	r.queueMu.Lock()
	r.queueClosed = true
	close(r.queue)
	r.queueMu.Unlock()
	select {
	case <-r.done:
	case <-ctx.Done():
		r.fail("upload_timeout")
	}
	r.stopLoop()
	<-r.loopDone
	durationMS, total, ids, err := r.checkpoint(ctx, true, master)
	r.mu.Lock()
	failure := r.failure
	r.mu.Unlock()
	switch {
	case err != nil:
		log.Printf("media recording %s g%d: final checkpoint: %v", r.recordingID, r.generation, err)
		failure = "upload_failed"
	case failure == "" && len(ids) == 0:
		failure = "empty"
	}
	if failure != "" {
		r.emitter.FinalEvent(ctx, mc.RecordingFailedBody{Kind: "recording_failed", RecordingID: r.recordingID, Reason: failure})
		return
	}
	log.Printf("media recording %s g%d: finalised, %d ms, %d bytes", r.recordingID, r.generation, durationMS, total)
	r.emitter.FinalEvent(ctx, mc.RecordingReadyBody{Kind: "recording_ready", RecordingID: r.recordingID, DurationMS: durationMS,
		Bytes: total, ManifestPath: r.prefix + broadcast.MultivariantName})
}

func (r *recorder) writeRendition(ctx context.Context, rendition string, segments []recordedSegment) error {
	entries := make([]vodEntry, 0, len(segments))
	for _, seg := range segments {
		entries = append(entries, vodEntry{uri: fmt.Sprintf("seg_%d.m4s", seg.msn), init: seg.init, duration: seg.duration,
			discontinuity: seg.discontinuity, key: r.keyLine()})
	}
	if err := r.put(ctx, r.prefix+rendition+"/"+broadcast.PlaylistName, vodPlaylist(segments[0].msn, entries, independentSegments(r.node.Topology().Ladder))); err != nil {
		return err
	}
	pages := pageRuns(segments)
	written := r.pages[rendition]
	if written == nil {
		written = map[int]pageState{}
		r.pages[rendition] = written
	}
	for index, chunk := range pages {
		state := pageState{first: chunk[0].msn, count: len(chunk)}
		if written[index] == state {
			continue
		}
		manifestEntries := make([]mc.SegmentEntry, 0, len(chunk))
		for _, seg := range chunk {
			manifestEntries = append(manifestEntries, mc.SegmentEntry{URI: fmt.Sprintf("seg_%d.m4s", seg.msn), MSN: seg.msn, SHA256: seg.sha, Bytes: seg.bytes})
		}
		manifest := mc.SegmentManifest{SchemaVersion: mc.SegmentManifestSchema, SessionID: r.node.SessionID, NodeID: r.node.NodeID,
			RenditionID: rendition, TopologyEpoch: r.node.Topology().Epoch, Sequence: r.nextSequence(),
			IssuedAtMS: time.Now().UnixMilli(), Ended: true,
			Window: mc.SegmentWindow{FirstMSN: chunk[0].msn, LastMSN: chunk[len(chunk)-1].msn},
			Page:   &mc.SegmentPage{Index: int64(index), Count: int64(len(pages))},
			Init:   mc.SegmentInit{URI: chunk[0].init, SHA256: chunk[0].initSHA}, Entries: manifestEntries}
		payload, err := mc.MarshalWire(manifest)
		if err != nil {
			return err
		}
		envelope, err := mc.MarshalWire(r.node.Sign(mc.SegmentManifestSchema, payload))
		if err != nil {
			return err
		}
		if err := r.put(ctx, fmt.Sprintf("%s%s/manifest-%d.json", r.prefix, rendition, index), envelope); err != nil {
			return err
		}
		written[index] = state
	}
	return nil
}

func (r *recorder) keyLine() string { return r.key }

func pageRuns(segments []recordedSegment) [][]recordedSegment {
	var pages [][]recordedSegment
	for start := 0; start < len(segments); {
		end := start
		for end < len(segments) && segments[end].init == segments[start].init {
			end++
		}
		for chunk := start; chunk < end; chunk += mc.ManifestVODPageSegments {
			pages = append(pages, segments[chunk:min(end, chunk+mc.ManifestVODPageSegments)])
		}
		start = end
	}
	return pages
}

func (r *recorder) nextSequence() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sequence++
	return r.sequence
}

type indexEntry struct {
	Kind          string  `json:"kind"`
	Rendition     string  `json:"rendition"`
	URI           string  `json:"uri,omitempty"`
	MSN           int64   `json:"msn,omitempty"`
	Duration      float64 `json:"duration,omitempty"`
	SHA256        string  `json:"sha256,omitempty"`
	InitSHA256    string  `json:"init_sha256,omitempty"`
	Bytes         int64   `json:"bytes"`
	Discontinuity bool    `json:"discontinuity,omitempty"`
}

type recordingIndex struct {
	mu      sync.Mutex
	file    *os.File
	entries []indexEntry
	resumed bool
}

func openRecordingIndex(path string, mustExist bool) (*recordingIndex, error) {
	index := &recordingIndex{resumed: mustExist}
	if mustExist {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, ErrRecordingStateLost
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			var entry indexEntry
			if json.Unmarshal(scanner.Bytes(), &entry) == nil && (entry.Kind == "init" || entry.Kind == "segment") {
				index.entries = append(index.entries, entry)
			}
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("recording index: %w", err)
		}
		if len(data) > 0 && data[len(data)-1] != '\n' {
			if _, err := file.Write([]byte{'\n'}); err != nil {
				_ = file.Close()
				return nil, fmt.Errorf("recording index: %w", err)
			}
		}
		index.file = file
		return index, nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("recording index: %w", err)
	}
	index.file = file
	return index, nil
}

func (i *recordingIndex) append(entry indexEntry) error {
	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	_, err = i.file.Write(append(encoded, '\n'))
	return err
}

func (i *recordingIndex) sync() error {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.file.Sync()
}

func (i *recordingIndex) close() {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	_ = i.file.Close()
}
