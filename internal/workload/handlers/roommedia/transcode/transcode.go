package transcode

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
)

type Role struct {
	env          *broadcast.Env
	node         *broadcast.Node
	ladder       *mc.TranscodeLadder
	partMS       int64
	outputs      map[string]*output
	order        []*output
	plan         *encoderPlan
	device       int
	composeSlots int
	source       bool
	segmentMS    int64

	timeline timeline
}

type timeline struct {
	mu        sync.Mutex
	offset    int64
	end       int64
	alignment int64
}

func (t *timeline) startRun() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.end > 0 {
		next := t.end + 90000/10
		if t.alignment > 0 {
			next = (next + t.alignment - 1) / t.alignment * t.alignment
		}
		t.offset = next
	}
	return t.offset
}

func (r *Role) startRun() int64 {
	offset := r.timeline.startRun()
	for _, out := range r.order {
		out.newRun()
	}
	return offset
}

func (t *timeline) observe(ticks int64) {
	t.mu.Lock()
	if ticks > t.end {
		t.end = ticks
	}
	t.mu.Unlock()
}

func Validate(topology *mc.Topology) error {
	if topology.Ladder == nil {
		return errors.New("transcode node without a ladder")
	}
	for _, rendition := range topology.Ladder.Video {
		if rendition.Codec == "av1" {
			return errors.New("av1 renditions are not supported by this transcoder (MPEG-TS hand-off)")
		}
	}
	if mc.IsSourceLadder(topology.Ladder) {
		if topology.DeliveryProfile != mc.DeliveryBroadcast {
			return errors.New("a source ladder is transcoded in broadcast sessions only")
		}
		if err := mc.ValidateLadder(*topology.Ladder); err != nil {
			return err
		}
	}
	return nil
}

func Run(ctx context.Context, env *broadcast.Env) error {
	topology := env.Node.Topology()
	if err := Validate(topology); err != nil {
		return err
	}
	role := &Role{env: env, node: env.Node, ladder: topology.Ladder, partMS: 500, outputs: map[string]*output{}, device: -1,
		source: mc.IsSourceLadder(topology.Ladder)}
	if topology.Packaging != nil {
		role.partMS = topology.Packaging.PartMS
		role.segmentMS = topology.Packaging.SegmentMS
	} else {
		role.segmentMS = topology.Ladder.GOPMS
	}
	role.timeline.alignment = role.segmentMS * 90
	for _, rendition := range role.ladder.Video {
		out := newOutput(rendition.RenditionID, true)
		if !rendition.Passthrough {
			out.encoded = role.encoded(rendition.RenditionID)
		}
		role.outputs[rendition.RenditionID], role.order = out, append(role.order, out)
	}
	for _, rendition := range role.ladder.Audio {
		out := newOutput(rendition.RenditionID, false)
		out.encoded = role.encoded(rendition.RenditionID)
		role.outputs[rendition.RenditionID], role.order = out, append(role.order, out)
	}
	defer func() {
		for _, out := range role.order {
			out.closeAll()
		}
	}()
	remove := env.Router.Handle(&broadcast.Route{Kind: broadcast.RoutePull, SessionID: role.node.SessionID, Node: role.node,
		Match: func(rest string) bool { return rest != "" && (rest != "source" || role.source) }, Handler: http.HandlerFunc(role.pull)})
	defer remove()
	role.announce()
	go env.RunLoad(ctx, 2*time.Second, role.announce)

	backoff := time.Second
	for ctx.Err() == nil {
		if _, present := role.node.Self(); !present {
			return nil
		}
		parent, ok := role.node.Parent()
		if !ok || len(parent.Endpoints) == 0 {
			role.node.NoteParentWait("pull")
			if !role.waitChange(ctx, 10*time.Second) {
				return nil
			}
			continue
		}
		started := time.Now()
		var err error
		switch parent.Role {
		case mc.RoleIngest:
			err = role.runFromIngest(ctx, parent)
		case mc.RoleSFU:
			err = role.runFromSFU(ctx, parent)
		default:
			err = fmt.Errorf("unsupported transcode parent role %q", parent.Role)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Printf("media transcode %s: %v", role.node.NodeID, err)
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 15*time.Second)
	}
	return nil
}

func (r *Role) encoded(renditionID string) func(time.Duration) {
	class := mc.RenditionClass(r.ladder, renditionID, nil)
	return func(duration time.Duration) { r.env.Usage.Reporter().AddRendition(renditionID, class, duration) }
}

func (r *Role) waitChange(ctx context.Context, limit time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-r.node.Changed():
	case <-time.After(limit):
	}
	return true
}

func (r *Role) announce() {
	r.env.Announce([]mc.TopologyEndpoint{{Kind: "pull", URL: r.env.URL("/v1/pull/" + r.node.SessionID)}})
}

func (r *Role) parentWatch(ctx context.Context, parent mc.TopologyNode) (context.Context, context.CancelFunc) {
	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case <-r.node.Changed():
				current, ok := r.node.Parent()
				if !ok || current.NodeID != parent.NodeID || broadcast.Endpoint(current, "pull") != broadcast.Endpoint(parent, "pull") ||
					broadcast.Endpoint(current, "whep") != broadcast.Endpoint(parent, "whep") {
					cancel()
					return
				}
			}
		}
	}()
	return runCtx, cancel
}

func (r *Role) runFromIngest(ctx context.Context, parent mc.TopologyNode) error {
	runCtx, cancel := r.parentWatch(ctx, parent)
	defer cancel()
	endpoint := broadcast.Endpoint(parent, "pull")
	if endpoint == "" {
		return errors.New("ingest parent announces no pull endpoint")
	}
	request, err := http.NewRequestWithContext(runCtx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/source", nil)
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
		return fmt.Errorf("ingest pull: HTTP %d", response.StatusCode)
	}
	upstream := r.env.Usage.Open(nil, broadcast.StringPtr(mc.NodeIdentityPrefix+parent.NodeID), broadcast.DirectionIngress, broadcast.TransportUpstream)
	defer upstream.Close()
	body := &meteredReader{r: response.Body, usage: upstream, total: &r.env.IngressB}
	if r.source {
		return r.runSource(runCtx, body)
	}
	var probed bytes.Buffer
	probe := &mpegts.Reader{R: io.TeeReader(io.LimitReader(body, 8<<20), &probed)}
	if err := probe.Initialize(); err != nil {
		return fmt.Errorf("source probe: %w", err)
	}
	plan := inputPlan{format: "mpegts"}
	for _, track := range probe.Tracks() {
		switch track.Codec.(type) {
		case *tscodecs.H264, *tscodecs.H265:
			plan.hasVideo = true
		case *tscodecs.MPEG4Audio, *tscodecs.Opus, *tscodecs.MPEG1Audio:
			if plan.audioInputs == 0 {
				plan.audioInputs = 1
			}
		}
	}
	return r.encode(runCtx, io.MultiReader(&probed, body), plan)
}

func (r *Role) encode(ctx context.Context, input io.Reader, plan inputPlan) error {
	config := r.env.Config.Transcode
	var lease *encoderLease
	if r.plan == nil {
		plan := planEncoders(r.ladder, config, GPUAvailable(config))
		first, err := acquireEncoder(config, plan.sessions, -1)
		if err != nil {
			plan = planEncoders(r.ladder, config, false)
			first, _ = acquireEncoder(config, 0, -1)
		}
		r.plan, lease = &plan, first
	} else {
		var err error
		lease, err = acquireEncoderWaiting(ctx, config, r.plan.sessions, r.device)
		if err != nil {
			return fmt.Errorf("%w: the renditions keep their NVENC encoder for the session", err)
		}
	}
	if lease.index >= 0 {
		r.device = lease.index
	}
	encoders := *r.plan
	args := ffmpegArgs(r.ladder, plan, encoders)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	process, err := startFFmpeg(runCtx, config, args, lease)
	if err != nil {
		lease.release()
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	defer func() { lease.releaseAfterRemoval(process.removeErr, process.remove, log.Printf) }()
	copyDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(process.stdin, input)
		_ = process.stdin.Close()
		copyDone <- err
	}()
	demuxErr := r.demux(process.stdout, r.startRun())
	stopErr := process.stop()
	cancel()
	if demuxErr != nil && !errors.Is(demuxErr, io.EOF) {
		return demuxErr
	}
	return stopErr
}

func (r *Role) demux(stdout io.Reader, offset int64) error {
	reader := &mpegts.Reader{R: bufio.NewReaderSize(stdout, 1<<20)}
	if err := reader.Initialize(); err != nil {
		return fmt.Errorf("encoder output: %w", err)
	}
	reader.OnDecodeError(func(error) {})
	index := map[uint16]int{}
	for i := range r.order {
		index[pidOf(i)] = i
	}
	var flushers []func()
	for _, track := range reader.Tracks() {
		position, known := index[track.PID]
		if !known {
			continue
		}
		out := r.order[position]
		switch codec := track.Codec.(type) {
		case *tscodecs.H264, *tscodecs.H265:
			_, isH265 := codec.(*tscodecs.H265)
			fragmenter := &videoFragmenter{out: out, h265: isH265, partTicks: r.partMS * 90}
			onAU := func(pts, dts int64, au [][]byte) error {
				fragmenter.write(pts+offset, dts+offset, au)
				r.timeline.observe(dts + offset)
				return nil
			}
			if isH265 {
				reader.OnDataH265(track, onAU)
			} else {
				reader.OnDataH264(track, onAU)
			}
			flushers = append(flushers, fragmenter.flush)
		case *tscodecs.MPEG4Audio:
			fragmenter := &audioFragmenter{out: out, partMS: r.partMS}
			config := codec.Config
			fragmenter.initAAC(&config)
			reader.OnDataMPEG4Audio(track, func(pts int64, aus [][]byte) error {
				fragmenter.write(pts+offset, aus)
				return nil
			})
			flushers = append(flushers, fragmenter.flush)
		case *tscodecs.Opus:
			fragmenter := &audioFragmenter{out: out, opus: true, partMS: r.partMS}
			channels := 2
			if codec.Desc != nil && codec.Desc.ChannelConfigCode > 0 && codec.Desc.ChannelConfigCode <= 8 {
				channels = int(codec.Desc.ChannelConfigCode)
			}
			fragmenter.initOpus(channels)
			reader.OnDataOpus(track, func(pts int64, packets [][]byte) error {
				fragmenter.write(pts+offset, packets)
				return nil
			})
			flushers = append(flushers, fragmenter.flush)
		}
	}
	defer func() {
		for _, flush := range flushers {
			flush()
		}
	}()
	for {
		if err := reader.Read(); err != nil {
			return err
		}
	}
}

func (r *Role) pull(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		broadcast.WriteError(response, http.StatusBadRequest, "invalid_request", "GET only")
		return
	}
	now := time.Now()
	if r.node.Expired(now) {
		broadcast.WriteError(response, http.StatusServiceUnavailable, "topology_expired", "")
		return
	}
	child, err := r.node.VerifyChild(broadcast.Bearer(request), now)
	if err != nil {
		broadcast.WriteTokenError(response, err)
		return
	}
	renditionID := strings.TrimPrefix(request.URL.Path, "/v1/pull/"+r.node.SessionID+"/")
	out := r.outputs[renditionID]
	if out == nil {
		broadcast.WriteError(response, http.StatusNotFound, "not_found", "unknown rendition")
		return
	}
	broadcast.Unbounded(response)
	init, sub := out.subscribe(request.Context().Done())
	if sub == nil {
		return
	}
	defer out.unsubscribe(sub)
	usage := r.env.Usage.Open(nil, broadcast.StringPtr(mc.NodeIdentityPrefix+child), broadcast.DirectionEgress, broadcast.TransportChild)
	defer usage.Close()
	r.env.Subscribers.Add(1)
	defer r.env.Subscribers.Add(-1)
	response.Header().Set("Content-Type", "video/mp4")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(response)
	write := func(data []byte) error {
		n, err := response.Write(data)
		usage.Add(n)
		r.env.EgressB.Add(int64(n))
		if err != nil {
			return err
		}
		return controller.Flush()
	}
	if write(init) != nil {
		return
	}
	for {
		select {
		case <-request.Context().Done():
			return
		case <-sub.done:
			return
		case fragment := <-sub.queue:
			if write(fragment.Data) != nil {
				return
			}
		}
	}
}

type meteredReader struct {
	r     io.Reader
	usage *broadcast.Connection
	total interface{ Add(int64) int64 }
}

func (m *meteredReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	m.usage.Add(n)
	m.total.Add(int64(n))
	return n, err
}
