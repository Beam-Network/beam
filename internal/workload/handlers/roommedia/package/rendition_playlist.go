package packager

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
)

func (r *rendition) targetDuration() int {
	return r.target
}

func (r *rendition) playlist(skip bool, reports []*playlist.MediaRenditionReport) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started() {
		return nil, errNotReady
	}
	target := r.targetDuration()
	partTarget := time.Duration(r.partMS) * time.Millisecond
	holdBack := 3 * partTarget
	skipUntil := time.Duration(6*target) * time.Second
	media := &playlist.Media{Version: 9, IndependentSegments: !r.source, TargetDuration: target,
		ServerControl: &playlist.MediaServerControl{CanBlockReload: true, PartHoldBack: &holdBack, CanSkipUntil: &skipUntil},
		PartInf:       &playlist.MediaPartInf{PartTarget: partTarget}}
	var key *playlist.MediaKey
	if r.key != nil {
		key = &playlist.MediaKey{Method: playlist.MediaKeyMethodSampleAES, KeyFormat: "org.w3.clearkey", KeyFormatVersions: "1",
			URI: "data:text/plain;base64," + base64.StdEncoding.EncodeToString(r.key.kid)}
	}
	segments := r.segments
	if len(segments) > 0 {
		media.MediaSequence = int(segments[0].msn)
	}
	discontinuities := r.discSeq
	if skip {
		var tail float64
		skipped := 0
		for i := len(segments) - 1; i >= 0; i-- {
			tail += segments[i].duration
			if tail > skipUntil.Seconds() {
				skipped = i
				break
			}
		}
		for _, seg := range segments[:skipped] {
			if seg.discontinuity {
				discontinuities++
			}
		}
		if skipped > 0 {
			media.Skip = &playlist.MediaSkip{SkippedSegments: skipped}
			segments = segments[skipped:]
		}
	}
	media.DiscontinuitySequence = &discontinuities
	media.Map = &playlist.MediaMap{URI: segments[0].init.uri}
	partsFrom := len(segments) - partRetentionSegments - 1
	var open *segment
	var listed []initMark
	for i, seg := range segments {
		if !seg.complete {
			open = seg
			continue
		}
		entry := &playlist.MediaSegment{Duration: time.Duration(seg.duration * float64(time.Second)),
			URI: fmt.Sprintf("seg_%d.m4s", seg.msn), Discontinuity: seg.discontinuity, Key: key}
		if i >= partsFrom {
			entry.Parts = r.partEntries(seg)
		}
		media.Segments = append(media.Segments, entry)
		listed = append(listed, initMark{uri: seg.init.uri})
	}
	nextMSN, nextPart := r.nextMSN, 0
	if open != nil {
		media.Parts = r.partEntries(open)
		nextMSN, nextPart = open.msn, len(open.parts)
		listed = append(listed, initMark{uri: open.init.uri, open: true, discontinuity: open.discontinuity})
	}
	media.PreloadHint = &playlist.MediaPreloadHint{URI: fmt.Sprintf("seg_%d.%d.m4s", nextMSN, nextPart)}
	media.RenditionReport = reports
	body, err := media.Marshal()
	if err != nil {
		return nil, err
	}
	return insertInitMaps(body, listed), nil
}

func (r *rendition) partEntries(seg *segment) []*playlist.MediaPart {
	entries := make([]*playlist.MediaPart, 0, len(seg.parts))
	for _, p := range seg.parts {
		entries = append(entries, &playlist.MediaPart{Duration: time.Duration(p.duration * float64(time.Second)),
			URI: fmt.Sprintf("seg_%d.%d.m4s", seg.msn, p.index), Independent: p.independent})
	}
	return entries
}

func (r *rendition) report() *playlist.MediaRenditionReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	last := r.last()
	if last == nil {
		return nil
	}
	lastMSN, lastPart := last.msn, len(last.parts)-1
	if last.complete {
		lastPart = -1
	}
	report := &playlist.MediaRenditionReport{URI: "../" + r.id + "/" + broadcast.PlaylistName, LastMSN: int(lastMSN)}
	if lastPart >= 0 {
		value := lastPart
		report.LastPart = &value
	}
	return report
}

func (r *rendition) object(name string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mc.InitURIPattern.MatchString(name) {
		for _, version := range r.inits {
			if version.uri == name {
				return version.data, true
			}
		}
		return nil, false
	}
	var msn, index int64 = -1, -1
	if _, err := fmt.Sscanf(name, "seg_%d.%d.m4s", &msn, &index); err == nil && fmt.Sprintf("seg_%d.%d.m4s", msn, index) == name {
		for _, seg := range r.segments {
			if seg.msn == msn && index < int64(len(seg.parts)) {
				return seg.parts[index].data, true
			}
		}
		return nil, false
	}
	if _, err := fmt.Sscanf(name, "seg_%d.m4s", &msn); err == nil && fmt.Sprintf("seg_%d.m4s", msn) == name {
		for _, seg := range r.segments {
			if seg.msn == msn && seg.complete {
				if seg.data != nil {
					return seg.data, true
				}
				data, err := os.ReadFile(seg.path)
				return data, err == nil
			}
		}
	}
	return nil, false
}

func (r *rendition) manifest(node *broadcast.Node, msn int64) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	last := r.last()
	if last == nil {
		return nil, errors.New("no segment yet")
	}
	if msn < 0 || msn > last.msn {
		msn = last.msn
	}
	end := -1
	for i, seg := range r.segments {
		if seg.msn <= msn {
			end = i
		}
	}
	if end < 0 {
		return nil, errors.New("the requested segment left the window")
	}
	init := r.segments[end].init
	msn = r.segments[end].msn
	start := end
	for start > 0 && r.segments[start-1].init == init {
		start--
	}
	first := max(0, msn-(mc.ManifestLiveWindowSegments-1), r.segments[start].msn)
	var entries []mc.SegmentEntry
	for _, seg := range r.segments {
		if seg.msn < first || seg.msn > msn || seg.init != init {
			continue
		}
		if seg.complete {
			entries = append(entries, mc.SegmentEntry{URI: fmt.Sprintf("seg_%d.m4s", seg.msn), MSN: seg.msn, SHA256: seg.sha, Bytes: seg.bytes})
		}
		for _, p := range seg.parts {
			index := int64(p.index)
			entries = append(entries, mc.SegmentEntry{URI: fmt.Sprintf("seg_%d.%d.m4s", seg.msn, p.index), MSN: seg.msn,
				Part: &index, SHA256: p.sha, Bytes: int64(len(p.data))})
		}
	}
	if len(entries) > mc.ManifestMaxEntries {
		entries = entries[len(entries)-mc.ManifestMaxEntries:]
	}
	cacheKey := fmt.Sprintf("%d/%d/%d/%s", first, msn, len(entries), init.uri)
	if cacheKey == r.manifestKey && r.manifestCache != nil {
		return r.manifestCache, nil
	}
	r.manifestSeq++
	if r.onSequence != nil {
		r.onSequence(r, r.manifestSeq)
	}
	manifest := mc.SegmentManifest{SchemaVersion: mc.SegmentManifestSchema, SessionID: node.SessionID, NodeID: node.NodeID,
		RenditionID: r.id, TopologyEpoch: node.Topology().Epoch, Sequence: r.manifestSeq, IssuedAtMS: time.Now().UnixMilli(),
		Window: mc.SegmentWindow{FirstMSN: first, LastMSN: msn}, Init: mc.SegmentInit{URI: init.uri, SHA256: init.sha},
		Entries: entries}
	payload, err := mc.MarshalWire(manifest)
	if err != nil {
		return nil, err
	}
	encoded, err := mc.MarshalWire(node.Sign(mc.SegmentManifestSchema, payload))
	if err != nil {
		return nil, err
	}
	r.manifestKey, r.manifestCache = cacheKey, encoded
	return encoded, nil
}
