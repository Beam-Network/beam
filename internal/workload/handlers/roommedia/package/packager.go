package packager

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
)

type Role struct {
	env        *broadcast.Env
	node       *broadcast.Node
	ladder     *mc.TranscodeLadder
	packaging  mc.TopologyPackaging
	renditions map[string]*rendition
	order      []*rendition
	key        *cbcsKey
	recorder   *recorder
	viewers    broadcast.Viewers
	players    *broadcast.Players
	holds      broadcast.HoldLimiter
	maxPart    int64
}

func Run(ctx context.Context, env *broadcast.Env) error {
	topology := env.Node.Topology()
	if topology.DeliveryProfile == mc.DeliveryVOD {
		return runVOD(ctx, env)
	}
	role := &Role{env: env, node: env.Node, ladder: topology.Ladder, packaging: *topology.Packaging, renditions: map[string]*rendition{},
		maxPart: broadcast.MaxPartIndex(topology), players: broadcast.NewPlayers(env.Usage.Reporter(), env.Node, env.StateDir)}
	defer role.players.Start(ctx)()
	if keys := env.Node.Spec.Details.ContentKeys; len(keys) > 0 {
		kid, _ := mc.DecodeHex(keys[0].KeyID)
		key, _ := mc.DecodeHex(keys[0].Key)
		iv, _ := mc.DecodeHex(keys[0].IV)
		cbcs, err := newCBCSKey(kid, key, iv)
		if err != nil {
			return err
		}
		role.key = cbcs
	}
	baseDir := ""
	if env.Config.CacheDir != "" {
		baseDir = filepath.Join(broadcast.PackageStoreRoot(env.Config.CacheDir), dvrStoreName(role.node))
		defer os.RemoveAll(baseDir)
	}
	checkDVRStore(role.node.NodeID, baseDir, env.Config.CacheDir, role.ladder, role.packaging)
	var order []string
	for _, video := range role.ladder.Video {
		order = append(order, video.RenditionID)
	}
	for _, audio := range role.ladder.Audio {
		order = append(order, audio.RenditionID)
	}
	if recording := env.Node.Spec.Details.Recording; recording != nil {
		role.recorder = newRecorder(env, *recording, order)
		role.recorder.master = role.master
		if role.key != nil {
			role.recorder.key = role.key.playlistKeyLine()
		}
		if err := role.recorder.open(ctx, env); err != nil {
			return err
		}
	}
	sequences, err := openMSNStore(env)
	if err != nil {
		if role.recorder != nil {
			role.recorder.index.close()
		}
		return err
	}
	sessionStart := sessionStartMSN(env, role.node.StartedAt)
	target := int(mc.HLSTargetDurationS(role.ladder, role.packaging))
	var starts *segmentStarts
	if mc.IsSourceLadder(role.ladder) {
		starts = &segmentStarts{}
	}
	recordCtx, stopRecording := context.WithCancel(context.Background())
	defer stopRecording()
	if role.recorder != nil && !role.recorder.start(recordCtx) {
		role.recorder = nil
	}
	add := func(id string, video bool) error {
		dir := ""
		if baseDir != "" {
			dir = filepath.Join(baseDir, id)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
		rend := newRendition(id, video, role.packaging.SegmentMS, role.packaging.PartMS, role.packaging.DVRWindowS, target, dir, role.key)
		if starts != nil {
			rend.source = true
			if video {
				rend.starts = starts
			} else {
				rend.follow = starts
			}
		}
		rend.startAt(sequences.start(id, sessionStart), sequences.sequenceStart(id))
		rend.published = role.published
		rend.onMSN = func(rend *rendition, msn int64) { sequences.useMSN(rend.id, msn) }
		rend.onSequence = func(rend *rendition, sequence int64) { sequences.useSequence(rend.id, sequence) }
		if role.recorder != nil {
			rend.onSegment = role.recorder.segment
		}
		role.renditions[id], role.order = rend, append(role.order, rend)
		return nil
	}
	for _, video := range role.ladder.Video {
		if err := add(video.RenditionID, true); err != nil {
			return err
		}
	}
	for _, audio := range role.ladder.Audio {
		if err := add(audio.RenditionID, false); err != nil {
			return err
		}
	}
	remove := env.Router.Handle(&broadcast.Route{Kind: broadcast.RouteHLS, SessionID: role.node.SessionID, Node: role.node,
		Handler: http.HandlerFunc(role.serve)})
	defer remove()
	role.announce()
	env.Players = role.viewers.Players
	go env.RunLoad(ctx, 2*time.Second, role.announce)
	for _, rend := range role.order {
		go role.pullLoop(ctx, rend)
	}
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-role.node.Changed():
			if _, present := role.node.Self(); !present {
				role.stop()
				return nil
			}
		}
	}
	role.stop()
	return nil
}

func (r *Role) published(rend *rendition, duration time.Duration) {
	var size *mc.VideoSize
	if rend.source && rend.video && rend.width > 0 && rend.height > 0 {
		size = &mc.VideoSize{Width: int64(rend.width), Height: int64(rend.height)}
	}
	r.env.Usage.Reporter().AddRendition(rend.id, mc.RenditionClass(r.ladder, rend.id, size), duration)
}

func (r *Role) stop() {
	for _, rend := range r.order {
		rend.finish()
	}
	if r.recorder != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		r.recorder.finish(ctx, r.master)
		r.recorder.index.close()
	}
}

func dvrStoreName(node *broadcast.Node) string {
	name := node.SessionID + "-" + node.NodeID
	if dvrStoreNamePattern.MatchString(node.WorkloadID) {
		name = node.WorkloadID
	}
	if node.Spec != nil {
		name += fmt.Sprintf("-e%da%d", node.Spec.Identity.Epoch, node.Spec.Identity.Attempt)
	}
	return name
}

var dvrStoreNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,199}$`)

const dvrStoreMargin = 1 << 30

func checkDVRStore(nodeID, dir, cacheDir string, ladder *mc.TranscodeLadder, packaging mc.TopologyPackaging) {
	memoryS := int64(memoryWindowSegments) * packaging.SegmentMS / 1000
	if packaging.DVRWindowS <= memoryS {
		return
	}
	if dir == "" {
		log.Printf("media package %s: dvr_window_s %d needs a DVR store on disk; this Worker has none (--media-cache-dir), "+
			"so the window is about %d s", nodeID, packaging.DVRWindowS, memoryS)
		return
	}
	need := dvrWindowBytes(ladder, packaging.DVRWindowS)
	if free, known := broadcast.FreeDiskBytes(cacheDir); known && free < need+dvrStoreMargin {
		log.Printf("media package %s: the DVR store %s has %d bytes free; dvr_window_s %d needs about %d at the ladder's bitrates: "+
			"when the disk fills, the window falls back to %d s in memory", nodeID, cacheDir, free, packaging.DVRWindowS, need, memoryS)
	}
}

func dvrWindowBytes(ladder *mc.TranscodeLadder, windowS int64) int64 {
	if ladder == nil {
		return 0
	}
	var bps int64
	for _, video := range ladder.Video {
		bps += video.BitrateBPS
	}
	for _, audio := range ladder.Audio {
		bps += audio.BitrateBPS
	}
	return bps / 8 * windowS
}

func (r *Role) master() ([]byte, error) { return buildMaster(r.ladder, r.renditions) }

func (r *Role) announce() {
	r.env.Announce([]mc.TopologyEndpoint{{Kind: "hls", URL: r.env.URL("/v1/hls/" + r.node.SessionID)}})
}

var errNoParentEndpoint = errors.New("no transcode parent with a pull endpoint")

func (r *Role) pullLoop(ctx context.Context, rend *rendition) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := r.pull(ctx, rend)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errNoParentEndpoint) {
			r.node.NoteParentWait("pull")
		} else if err != nil {
			log.Printf("media package %s/%s: %v", r.node.NodeID, rend.id, err)
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-r.node.Changed():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

func (r *Role) pull(ctx context.Context, rend *rendition) error {
	parent, ok := r.node.Parent()
	endpoint := broadcast.Endpoint(parent, "pull")
	if !ok || endpoint == "" {
		return errNoParentEndpoint
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case <-r.node.Changed():
				current, ok := r.node.Parent()
				if !ok || current.NodeID != parent.NodeID || broadcast.Endpoint(current, "pull") != endpoint {
					cancel()
					return
				}
			}
		}
	}()
	request, err := http.NewRequestWithContext(runCtx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/"+rend.id, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+r.node.ServiceToken())
	response, err := broadcast.UpstreamClient(r.env.Config, parent.TLSCertificateSHA256).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("transcode pull: HTTP %d", response.StatusCode)
	}
	upstream := r.env.Usage.Open(nil, broadcast.StringPtr(mc.NodeIdentityPrefix+parent.NodeID), broadcast.DirectionIngress, broadcast.TransportUpstream)
	defer upstream.Close()
	return consumeCMAF(bufio.NewReaderSize(response.Body, 1<<20), rend, parent.NodeID, func(n int) {
		upstream.Add(n)
		r.env.IngressB.Add(int64(n))
	}, func(err error) {
		log.Printf("media package %s/%s: fragment: %v", r.node.NodeID, rend.id, err)
	})
}

func consumeCMAF(reader *bufio.Reader, rend *rendition, parent string, count func(int), fragmentFailed func(error)) error {
	var pending []byte
	initDone := false
	newParent := rend.otherParent(parent)
	for {
		kind, data, err := readBox(reader)
		if err != nil {
			return err
		}
		count(len(data))
		pending = append(pending, data...)
		switch {
		case kind == "moov":
			if initDone || newParent {
				err = rend.newRun(pending)
			} else {
				err = rend.setInit(pending)
				if err == nil {
					rend.reconnected()
				}
			}
			if err != nil {
				return err
			}
			if !initDone {
				rend.fedBy(parent)
			}
			pending, initDone, newParent = nil, true, false
		case initDone && kind == "mdat":
			if err := rend.addFragment(pending); err != nil {
				fragmentFailed(err)
			}
			pending = nil
		}
		if len(pending) > 64<<20 {
			return errors.New("oversized CMAF fragment")
		}
	}
}

func readBox(reader *bufio.Reader) (string, []byte, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(reader, header); err != nil {
		return "", nil, err
	}
	size := uint64(binary.BigEndian.Uint32(header))
	kind := string(header[4:8])
	if size == 1 {
		large := make([]byte, 8)
		if _, err := io.ReadFull(reader, large); err != nil {
			return "", nil, err
		}
		header = append(header, large...)
		size = binary.BigEndian.Uint64(large)
	}
	if size < uint64(len(header)) || size > 64<<20 {
		return "", nil, errors.New("invalid box size in CMAF stream")
	}
	body := make([]byte, size)
	copy(body, header)
	if _, err := io.ReadFull(reader, body[len(header):]); err != nil {
		return "", nil, err
	}
	return kind, body, nil
}

func (r *Role) serve(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/v1/hls/"+r.node.SessionID+"/"+broadcast.ReceiptName {
		r.players.ServeReceipt(response, request)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "GET only")
		return
	}
	rest := strings.TrimPrefix(request.URL.Path, "/v1/hls/"+r.node.SessionID+"/")
	if rest == broadcast.MultivariantName {
		r.serveMaster(response, request)
		return
	}
	renditionID, name, ok := strings.Cut(rest, "/")
	rend := r.renditions[renditionID]
	if !ok || rend == nil || strings.Contains(name, "/") {
		broadcast.WriteError(response, http.StatusNotFound, "not_found", "")
		return
	}
	blocking, err := broadcast.ParseBlocking(request.URL.Query())
	if err != nil {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	switch name {
	case broadcast.PlaylistName:
		r.servePlaylist(response, request, rend, blocking)
	case broadcast.ManifestName:
		r.serveManifest(response, request, rend, blocking)
	default:
		r.serveObject(response, request, rend, name)
	}
}

func (r *Role) serveMaster(response http.ResponseWriter, request *http.Request) {
	arrived := r.players.Now()
	access, status, code := broadcast.AuthorizeMultivariant(r.node, request, time.Now())
	if status != 0 {
		broadcast.WriteError(response, status, code, "")
		return
	}
	answer, written := r.players.Multivariant(&access, arrived), 0
	defer func() { answer.Done(written) }()
	broadcast.Hold(response, 5*time.Second)
	deadline := time.After(5 * time.Second)
	for _, rend := range r.order {
		select {
		case <-rend.ready:
		case <-deadline:
		case <-request.Context().Done():
			return
		}
	}
	body, err := buildMaster(r.ladder, r.renditions)
	if err == nil {
		r.players.LearnVariants(body)
		body, err = broadcast.RewriteMultivariant(body, access.Credential)
	}
	if err != nil {
		broadcast.WriteError(response, http.StatusInternalServerError, "invalid_request", "")
		return
	}
	cache := broadcast.CachePrivate
	if access.Child != "" {
		cache = "public, max-age=2"
	}
	connection := r.viewers.Connection(r.env.Usage, request, access)
	written = broadcast.ServeBytes(response, request, body, broadcast.ContentTypePlaylist, cache)
	connection.Add(written)
	r.env.EgressB.Add(int64(written))
}

func (r *Role) block(response http.ResponseWriter, request *http.Request, rend *rendition, blocking broadcast.Blocking, access broadcast.HLSAccess) bool {
	limit := 3 * time.Duration(max(rend.targetDuration(), 1)) * time.Second
	if blocking.MSN >= 0 {
		if rend.tooFar(blocking.MSN) || blocking.Part > r.maxPart {
			broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "_HLS_msn or _HLS_part is out of range")
			return false
		}
		if rend.satisfiedNow(blocking.MSN, blocking.Part) {
			return true
		}
		release, ok := r.holds.Acquire(broadcast.HolderOf(request, access))
		if !ok {
			broadcast.WriteRateLimited(response)
			return false
		}
		defer release()
		broadcast.Hold(response, limit)
		if !rend.wait(request.Context(), blocking.MSN, blocking.Part, limit) {
			writeHoldTimeout(response)
			return false
		}
		return true
	}
	if !rend.hasStarted() {
		release, ok := r.holds.Acquire(broadcast.HolderOf(request, access))
		if !ok {
			broadcast.WriteRateLimited(response)
			return false
		}
		defer release()
		broadcast.Hold(response, limit)
		if !rend.wait(request.Context(), 0, -1, limit) {
			writeHoldTimeout(response)
			return false
		}
	}
	return true
}

func writeHoldTimeout(response http.ResponseWriter) {
	response.Header().Set("Retry-After", "1")
	broadcast.WriteError(response, http.StatusServiceUnavailable, "not_found", "the requested segment or part is not available yet")
}

func (r *Role) servePlaylist(response http.ResponseWriter, request *http.Request, rend *rendition, blocking broadcast.Blocking) {
	arrived := r.players.Now()
	access, status, code := broadcast.AuthorizeMediaPlaylist(r.node, request, time.Now())
	if status != 0 {
		broadcast.WriteError(response, status, code, "")
		return
	}
	answer, refusal := r.players.Playlist(access, rend.id, arrived)
	if refusal != "" {
		broadcast.WriteError(response, http.StatusForbidden, refusal, "")
		return
	}
	written := 0
	defer func() { answer.Done(written) }()
	if !r.block(response, request, rend, blocking, access) {
		return
	}
	var reports []*playlist.MediaRenditionReport
	for _, other := range r.order {
		if other != rend {
			if report := other.report(); report != nil {
				reports = append(reports, report)
			}
		}
	}
	body, err := rend.playlist(blocking.Skip, reports)
	if err != nil {
		broadcast.WriteError(response, http.StatusServiceUnavailable, "not_found", err.Error())
		return
	}
	body = r.players.Tag(body, access)
	connection := r.viewers.Connection(r.env.Usage, request, access)
	written = broadcast.ServeBytes(response, request, body, broadcast.ContentTypePlaylist, broadcast.CacheLive)
	connection.Add(written)
	r.env.EgressB.Add(int64(written))
}

func (r *Role) serveManifest(response http.ResponseWriter, request *http.Request, rend *rendition, blocking broadcast.Blocking) {
	if blocking.Page >= 0 {
		broadcast.WriteError(response, http.StatusNotFound, "not_found", "a live session has no manifest pages")
		return
	}
	access := r.childAccess(request)
	answer, written := r.players.Object(request, access, rend.id, false, r.players.Now()), 0
	defer func() { answer.Done(written) }()
	if !r.block(response, request, rend, blocking, access) {
		return
	}
	body, err := rend.manifest(r.node, blocking.MSN)
	if err != nil {
		broadcast.WriteError(response, http.StatusServiceUnavailable, "not_found", err.Error())
		return
	}
	connection := r.viewers.Connection(r.env.Usage, request, access)
	written = broadcast.ServeBytes(response, request, body, broadcast.ContentTypeJSON, broadcast.CacheLive)
	connection.Add(written)
	r.env.EgressB.Add(int64(written))
}

func (r *Role) childAccess(request *http.Request) broadcast.HLSAccess {
	if bearer := broadcast.Bearer(request); bearer != "" {
		if child, err := r.node.VerifyChild(bearer, time.Now()); err == nil {
			return broadcast.HLSAccess{Child: child}
		}
	}
	return broadcast.HLSAccess{}
}

func (r *Role) serveObject(response http.ResponseWriter, request *http.Request, rend *rendition, name string) {
	access := r.childAccess(request)
	answer, written := r.players.Object(request, access, rend.id, strings.HasSuffix(name, ".m4s"), r.players.Now()), 0
	defer func() { answer.Done(written) }()
	body, ok := rend.object(name)
	if !ok {
		var msn, index int64 = -1, -1
		if _, err := fmt.Sscanf(name, "seg_%d.%d.m4s", &msn, &index); err != nil {
			index = -1
			if _, err := fmt.Sscanf(name, "seg_%d.m4s", &msn); err != nil {
				msn = -1
			}
		}
		if msn >= 0 && !rend.tooFar(msn) && index <= r.maxPart {
			release, held := r.holds.Acquire(broadcast.HolderOf(request, access))
			if !held {
				broadcast.WriteRateLimited(response)
				return
			}
			limit := 3 * time.Duration(max(rend.targetDuration(), 1)) * time.Second
			broadcast.Hold(response, limit)
			if rend.wait(request.Context(), msn, index, limit) {
				body, ok = rend.object(name)
			}
			release()
		}
	}
	if !ok || body == nil {
		broadcast.WriteError(response, http.StatusNotFound, "not_found", "")
		return
	}
	connection := r.viewers.Connection(r.env.Usage, request, access)
	written = broadcast.ServeBytes(response, request, body, broadcast.ContentTypeMP4, broadcast.CacheImmutable)
	connection.Add(written)
	r.env.EgressB.Add(int64(written))
}
