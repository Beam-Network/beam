package packager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
)

type vodRole struct {
	env         *broadcast.Env
	node        *broadcast.Node
	generations []vodGeneration
	renditions  []string
	master      []byte
	viewers     broadcast.Viewers
	players     *broadcast.Players
	cache       objectCache

	playlistsMu sync.Mutex
	playlists   map[string][]byte
}

type vodGeneration struct {
	number     int64
	renditions []string
}

func runVOD(ctx context.Context, env *broadcast.Env) error {
	recording := env.Node.Spec.Details.Recording
	if recording == nil || recording.Generations == nil {
		return errors.New("a vod package node needs its recording and generation count")
	}
	if env.Bucket == nil {
		return fmt.Errorf("recording bucket: %w", env.BucketErr)
	}
	role := &vodRole{env: env, node: env.Node, playlists: map[string][]byte{},
		cache:   objectCache{limit: max(env.Config.CacheMemoryBytes/4, 256<<20), entries: map[string][]byte{}},
		players: broadcast.NewPlayers(env.Usage.Reporter(), env.Node, env.StateDir)}
	defer role.players.Start(ctx)()
	if err := role.load(ctx, *recording); err != nil {
		return err
	}
	remove := env.Router.Handle(&broadcast.Route{Kind: broadcast.RouteHLS, SessionID: role.node.SessionID, Node: role.node,
		Handler: http.HandlerFunc(role.serve)})
	defer remove()
	announce := func() {
		env.Announce([]mc.TopologyEndpoint{{Kind: "hls", URL: env.URL("/v1/hls/" + role.node.SessionID)}})
	}
	announce()
	env.Players = role.viewers.Players
	go env.RunLoad(ctx, 2*time.Second, announce)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-role.node.Changed():
			if _, present := role.node.Self(); !present {
				return nil
			}
		}
	}
}

func (v *vodRole) load(ctx context.Context, recording mc.NodeRecording) error {
	seen := map[string]bool{}
	var found []string
	for generation := int64(1); generation <= *recording.Generations; generation++ {
		lookupCtx, cancel := context.WithTimeout(ctx, time.Minute)
		raw, err := v.env.Bucket.Get(lookupCtx, mc.RecordingGenerationPrefix(recording.BucketPrefix, generation)+"recording.json")
		cancel()
		if errors.Is(err, broadcast.ErrObjectNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("g%d/recording.json: %w", generation, err)
		}
		var file mc.RecordingFile
		if err := mc.UnmarshalStrict(raw, &file); err != nil || mc.ValidateRecordingFile(file) != nil ||
			file.RecordingID != recording.RecordingID || file.Generation != generation {
			log.Printf("media vod %s: g%d/recording.json is invalid; generation skipped", recording.RecordingID, generation)
			continue
		}
		v.generations = append(v.generations, vodGeneration{number: generation, renditions: file.Renditions})
		for _, rendition := range file.Renditions {
			if !seen[rendition] {
				seen[rendition] = true
				found = append(found, rendition)
			}
		}
	}
	if len(v.generations) == 0 {
		return errors.New("the recording has no playable generation (no recording.json)")
	}
	if ladder := v.node.Topology().Ladder; ladder != nil {
		for _, video := range ladder.Video {
			if seen[video.RenditionID] {
				v.renditions = append(v.renditions, video.RenditionID)
				delete(seen, video.RenditionID)
			}
		}
		for _, audio := range ladder.Audio {
			if seen[audio.RenditionID] {
				v.renditions = append(v.renditions, audio.RenditionID)
				delete(seen, audio.RenditionID)
			}
		}
	}
	for _, rendition := range found {
		if seen[rendition] {
			v.renditions = append(v.renditions, rendition)
		}
	}
	var masters [][]byte
	for index := len(v.generations) - 1; index >= 0; index-- {
		body, err := v.get(ctx, fmt.Sprintf("g%d/%s", v.generations[index].number, broadcast.MultivariantName))
		if err == nil {
			masters = append(masters, body)
		}
	}
	master, err := mergeMasters(masters, v.renditions)
	if err != nil {
		return fmt.Errorf("recording master playlist: %w", err)
	}
	v.master = master
	return nil
}

func mergeMasters(bodies [][]byte, order []string) ([]byte, error) {
	var merged *playlist.Multivariant
	variants := map[string]bool{}
	renditions := map[string]bool{}
	for _, body := range bodies {
		var parsed playlist.Multivariant
		if parsed.Unmarshal(body) != nil {
			continue
		}
		if merged == nil {
			merged = &parsed
			for _, variant := range parsed.Variants {
				variants[variant.URI] = true
			}
			for _, rendition := range parsed.Renditions {
				if rendition.URI != nil {
					renditions[*rendition.URI] = true
				}
			}
			continue
		}
		for _, variant := range parsed.Variants {
			if !variants[variant.URI] {
				variants[variant.URI] = true
				merged.Variants = append(merged.Variants, variant)
			}
		}
		for _, rendition := range parsed.Renditions {
			if rendition.URI != nil && !renditions[*rendition.URI] {
				renditions[*rendition.URI] = true
				merged.Renditions = append(merged.Renditions, rendition)
			}
		}
	}
	if merged == nil {
		return nil, errors.New("no generation has a master playlist")
	}
	rank := func(uri string) int {
		id, _, _ := strings.Cut(uri, "/")
		if index := slices.Index(order, id); index >= 0 {
			return index
		}
		return len(order)
	}
	slices.SortStableFunc(merged.Variants, func(a, b *playlist.MultivariantVariant) int { return rank(a.URI) - rank(b.URI) })
	return merged.Marshal()
}

func (v *vodRole) get(ctx context.Context, key string) ([]byte, error) {
	if body, ok := v.cache.get(key); ok {
		return body, nil
	}
	body, err := v.env.Bucket.Get(ctx, v.node.Spec.Details.RecordingPrefix(true)+key)
	if err != nil {
		return nil, err
	}
	v.cache.put(key, body)
	return body, nil
}

func (v *vodRole) playlist(ctx context.Context, rendition string) ([]byte, error) {
	v.playlistsMu.Lock()
	cached := v.playlists[rendition]
	v.playlistsMu.Unlock()
	if cached != nil {
		return cached, nil
	}
	var entries []vodEntry
	for _, generation := range v.generations {
		if !slices.Contains(generation.renditions, rendition) {
			continue
		}
		body, err := v.get(ctx, fmt.Sprintf("g%d/%s/%s", generation.number, rendition, broadcast.PlaylistName))
		if errors.Is(err, broadcast.ErrObjectNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		parsed, err := parseVODPlaylist(body)
		if err != nil {
			log.Printf("media vod %s: g%d/%s: %v; generation skipped", v.node.SessionID, generation.number, rendition, err)
			continue
		}
		directory := fmt.Sprintf("g%d/", generation.number)
		for index, entry := range parsed {
			entry.uri, entry.init = directory+entry.uri, directory+entry.init
			entry.discontinuity = entry.discontinuity || (index == 0 && len(entries) > 0)
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil, broadcast.ErrObjectNotFound
	}
	var first int64
	_, _ = fmt.Sscanf(entries[0].uri[strings.IndexByte(entries[0].uri, '/')+1:], "seg_%d.m4s", &first)
	body := vodPlaylist(first, entries, independentSegments(v.node.Topology().Ladder))
	v.playlistsMu.Lock()
	v.playlists[rendition] = body
	v.playlistsMu.Unlock()
	return body, nil
}

func (v *vodRole) generationObject(rendition, rest string, page int64) string {
	directory, name, ok := strings.Cut(rest, "/")
	if !ok || strings.Contains(name, "/") || !strings.HasPrefix(directory, "g") {
		return ""
	}
	number, err := strconv.ParseInt(directory[1:], 10, 64)
	if err != nil || "g"+strconv.FormatInt(number, 10) != directory {
		return ""
	}
	known := false
	for _, generation := range v.generations {
		known = known || (generation.number == number && slices.Contains(generation.renditions, rendition))
	}
	switch {
	case !known:
		return ""
	case name == broadcast.ManifestName:
		return fmt.Sprintf("%s/%s/manifest-%d.json", directory, rendition, max(page, 0))
	case mc.InitURIPattern.MatchString(name) || recordedSegmentPattern.MatchString(name):
		return directory + "/" + rendition + "/" + name
	}
	return ""
}

func (v *vodRole) serve(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/v1/hls/"+v.node.SessionID+"/"+broadcast.ReceiptName {
		v.players.ServeReceipt(response, request)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "GET only")
		return
	}
	broadcast.Hold(response, time.Minute)
	rest := strings.TrimPrefix(request.URL.Path, "/v1/hls/"+v.node.SessionID+"/")
	now := time.Now()
	var body []byte
	var err error
	contentType, cache := broadcast.ContentTypeMP4, broadcast.CacheImmutable
	var access broadcast.HLSAccess
	var answer *broadcast.Answer
	written, arrived := 0, v.players.Now()
	defer func() { answer.Done(written) }()
	switch {
	case rest == broadcast.MultivariantName:
		var status int
		var code string
		access, status, code = broadcast.AuthorizeMultivariant(v.node, request, now)
		if status != 0 {
			broadcast.WriteError(response, status, code, "")
			return
		}
		answer = v.players.Multivariant(&access, arrived)
		v.players.LearnVariants(v.master)
		body, err = broadcast.RewriteMultivariant(v.master, access.Credential)
		contentType, cache = broadcast.ContentTypePlaylist, broadcast.CachePrivate
		if access.Child != "" {
			cache = "public, max-age=2"
		}
	default:
		renditionID, name, ok := strings.Cut(rest, "/")
		if !ok || !slices.Contains(v.renditions, renditionID) {
			broadcast.WriteError(response, http.StatusNotFound, "not_found", "")
			return
		}
		if name == broadcast.PlaylistName {
			var status int
			var code string
			access, status, code = broadcast.AuthorizeMediaPlaylist(v.node, request, now)
			if status != 0 {
				broadcast.WriteError(response, status, code, "")
				return
			}
			if answer, code = v.players.Playlist(access, renditionID, arrived); code != "" {
				broadcast.WriteError(response, http.StatusForbidden, code, "")
				return
			}
			body, err = v.playlist(request.Context(), renditionID)
			if err == nil {
				body = v.players.Tag(body, access)
			}
			contentType = broadcast.ContentTypePlaylist
			break
		}
		blocking, parseErr := broadcast.ParseBlocking(request.URL.Query())
		if parseErr != nil {
			broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", parseErr.Error())
			return
		}
		key := v.generationObject(renditionID, name, blocking.Page)
		if key == "" {
			broadcast.WriteError(response, http.StatusNotFound, "not_found", "")
			return
		}
		if strings.HasSuffix(key, ".json") {
			contentType = broadcast.ContentTypeJSON
		}
		access = v.childAccess(request, now)
		answer = v.players.Object(request, access, renditionID, strings.HasSuffix(key, ".m4s"), arrived)
		body, err = v.get(request.Context(), key)
	}
	if errors.Is(err, broadcast.ErrObjectNotFound) {
		broadcast.WriteError(response, http.StatusNotFound, "not_found", "")
		return
	}
	if err != nil {
		broadcast.WriteError(response, http.StatusServiceUnavailable, "not_found", "bucket unavailable")
		return
	}
	connection := v.viewers.Connection(v.env.Usage, request, access)
	written = broadcast.ServeBytes(response, request, body, contentType, cache)
	connection.Add(written)
	v.env.EgressB.Add(int64(written))
}

func (v *vodRole) childAccess(request *http.Request, now time.Time) broadcast.HLSAccess {
	if bearer := broadcast.Bearer(request); bearer != "" {
		if child, err := v.node.VerifyChild(bearer, now); err == nil {
			return broadcast.HLSAccess{Child: child}
		}
	}
	return broadcast.HLSAccess{}
}

type objectCache struct {
	mu      sync.Mutex
	limit   int64
	size    int64
	order   []string
	entries map[string][]byte
}

func (c *objectCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, ok := c.entries[key]
	return body, ok
}

func (c *objectCache) put(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists || int64(len(body)) > c.limit {
		return
	}
	c.entries[key] = body
	c.order = append(c.order, key)
	c.size += int64(len(body))
	for c.size > c.limit && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		c.size -= int64(len(c.entries[oldest]))
		delete(c.entries, oldest)
	}
}
