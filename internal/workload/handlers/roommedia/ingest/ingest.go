package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortmplib/pkg/liberrors"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

const (
	ProtocolWHIP  = "whip"
	ProtocolRTMP  = "rtmp"
	ProtocolRTMPS = "rtmps"
	ProtocolSRT   = "srt"
)

var streamKeyPattern = regexp.MustCompile(`^bsk_[A-Za-z0-9_-]{43}$`)

type Role struct {
	env  *broadcast.Env
	node *broadcast.Node
	hub  *Hub
	whip *whipServer

	mu         sync.Mutex
	publishers map[*Publisher]*publisherState

	grants        *mc.IngestGrantNonces
	releaseGrants func()

	superseded     chan struct{}
	supersededOnce sync.Once
}

var ErrSuperseded = errors.New("a newer ingest node of this session took over on this Worker")

func (r *Role) supersede() { r.supersededOnce.Do(func() { close(r.superseded) }) }

type publisherState struct {
	usage                *broadcast.Connection
	keySHA256            string
	check                *sourceCheck
	videoTrack           int
	tracks               []TrackInfo
	refused              atomic.Bool
	ended                chan struct{}
	videoSeen, audioSeen atomic.Int64
	width, height        atomic.Int64
}

func (s *publisherState) mediaClass(since time.Time) string {
	switch after := since.UnixNano(); {
	case s.videoSeen.Load() >= after && s.videoSeen.Load() > 0:
		return mc.VideoClass(s.width.Load(), s.height.Load())
	case s.audioSeen.Load() >= after && s.audioSeen.Load() > 0:
		return mc.ClassAudio
	}
	return ""
}

func (s *publisherState) saw(unit Unit, track TrackInfo) {
	now := time.Now().UnixNano()
	if !track.Video {
		s.audioSeen.Store(now)
		return
	}
	s.videoSeen.Store(now)
	if !unit.Key {
		return
	}
	size := usage.H264Size
	if track.Codec == CodecH265 {
		size = usage.H265Size
	}
	for _, nalu := range unit.AU {
		if width, height := size(nalu); width > 0 && height > 0 {
			s.width.Store(int64(width))
			s.height.Store(int64(height))
			return
		}
	}
}

var grantMemoryRetention = time.Duration(mc.IngestGrantMaxLifetimeSeconds+mc.TokenClockSkewSeconds) * time.Second

type grantMemory struct {
	nonces  *mc.IngestGrantNonces
	users   int
	release *time.Timer
}

var grantMemories = struct {
	sync.Mutex
	byNode map[string]*grantMemory
}{byNode: map[string]*grantMemory{}}

func grantNonces(nodeID string) (*mc.IngestGrantNonces, func()) {
	grantMemories.Lock()
	defer grantMemories.Unlock()
	memory := grantMemories.byNode[nodeID]
	if memory == nil {
		memory = &grantMemory{nonces: mc.NewIngestGrantNonces(mc.IngestGrantNoncesMax)}
		grantMemories.byNode[nodeID] = memory
	}
	if memory.release != nil {
		memory.release.Stop()
		memory.release = nil
	}
	memory.users++
	var once sync.Once
	return memory.nonces, func() {
		once.Do(func() {
			grantMemories.Lock()
			defer grantMemories.Unlock()
			if memory.users--; memory.users > 0 {
				return
			}
			memory.release = time.AfterFunc(grantMemoryRetention, func() {
				grantMemories.Lock()
				defer grantMemories.Unlock()
				if memory.users == 0 && grantMemories.byNode[nodeID] == memory {
					delete(grantMemories.byNode, nodeID)
				}
			})
		})
	}
}

var active = struct {
	sync.RWMutex
	bySession map[string]*Role
}{bySession: map[string]*Role{}}

func Run(ctx context.Context, env *broadcast.Env) error {
	role := &Role{env: env, node: env.Node, hub: NewHub(env.Config.PublisherReconnectGrace), publishers: map[*Publisher]*publisherState{},
		superseded: make(chan struct{})}
	role.grants, role.releaseGrants = grantNonces(role.node.NodeID)
	defer role.releaseGrants()
	role.hub.waitKey = mc.IsSourceLadder(env.Node.Topology().Ladder)
	whip, err := newWHIPServer(role)
	if err != nil {
		return err
	}
	role.whip = whip
	ensureListeners(env)
	active.Lock()
	if previous := active.bySession[role.node.SessionID]; previous != nil {
		if !newerNode(role.node, previous.node) {
			active.Unlock()
			return errors.New("an ingest node of this session that is not older in Core's order already runs on this Worker")
		}
		previous.supersede()
	}
	active.bySession[role.node.SessionID] = role
	active.Unlock()
	defer func() {
		active.Lock()
		if active.bySession[role.node.SessionID] == role {
			delete(active.bySession, role.node.SessionID)
		}
		active.Unlock()
	}()
	session := role.node.SessionID
	removeWHIP := env.Router.Handle(&broadcast.Route{Kind: broadcast.RouteIngest, SessionID: session, Node: role.node, Handler: whip})
	defer removeWHIP()
	removePull := env.Router.Handle(&broadcast.Route{Kind: broadcast.RoutePull, SessionID: session, Node: role.node,
		Match: func(rest string) bool { return rest == "source" }, Handler: http.HandlerFunc(role.pull)})
	defer removePull()
	role.announce()
	defer role.stopAll("ended")
	defer whip.closeAll()
	defer role.hub.CloseAll()

	go env.RunLoad(ctx, 2*time.Second, role.announce)
	go role.watchReceipts(ctx)
	for {
		changed := role.node.Changed()
		select {
		case <-ctx.Done():
			return nil
		case <-role.superseded:
			removeWHIP()
			removePull()
			log.Printf("media ingest %s: %v; this one ends", role.node.WorkloadID, ErrSuperseded)
			return nil
		case <-changed:
			if _, present := role.node.Self(); !present {
				return nil
			}
			role.enforce()
		}
	}
}

func (r *Role) announce() {
	topology := r.node.Topology()
	config := r.env.Config
	host := r.env.Host()
	session := r.node.SessionID
	var endpoints []mc.TopologyEndpoint
	allowed := topology.Ingest.Protocols
	if slices.Contains(allowed, ProtocolWHIP) {
		endpoints = append(endpoints, mc.TopologyEndpoint{Kind: "whip", URL: r.env.URL("/v1/ingest/" + session + "/whip")})
	}
	if slices.Contains(allowed, ProtocolRTMP) && config.RTMPPort > 0 && listenerUp(ProtocolRTMP) {
		endpoints = append(endpoints, mc.TopologyEndpoint{Kind: "rtmp", URL: "rtmp://" + hostPort(host, config.RTMPPort) + "/live"})
	}
	if slices.Contains(allowed, ProtocolRTMPS) && config.RTMPSPort > 0 && listenerUp(ProtocolRTMPS) {
		endpoints = append(endpoints, mc.TopologyEndpoint{Kind: "rtmps", URL: "rtmps://" + hostPort(host, config.RTMPSPort) + "/live"})
	}
	if slices.Contains(allowed, ProtocolSRT) && config.SRTPort > 0 && listenerUp(ProtocolSRT) {
		endpoints = append(endpoints, mc.TopologyEndpoint{Kind: "srt", URL: "srt://" + hostPort(host, config.SRTPort)})
	}
	endpoints = append(endpoints, mc.TopologyEndpoint{Kind: "pull", URL: r.env.URL("/v1/pull/" + session)})
	r.env.Announce(endpoints)
}

func hostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (r *Role) allows(protocol string) bool {
	return slices.Contains(r.node.Topology().Ingest.Protocols, protocol)
}

func (r *Role) accepting() string {
	select {
	case <-r.superseded:
		return "draining"
	default:
	}
	switch {
	case r.node.Expired(time.Now()):
		return "topology_expired"
	case r.node.Draining():
		return "draining"
	}
	return ""
}

func keyDigest(key string) string {
	if !streamKeyPattern.MatchString(key) {
		return ""
	}
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

func (r *Role) findKey(keySHA string) (mc.TopologyStreamKey, bool) {
	if keySHA == "" {
		return mc.TopologyStreamKey{}, false
	}
	if ingest := r.node.Topology().Ingest; ingest != nil {
		for _, entry := range ingest.StreamKeys {
			if entry.SHA256 == keySHA {
				return entry, true
			}
		}
	}
	return mc.TopologyStreamKey{}, false
}

func (r *Role) keyListed(keySHA string) bool {
	_, listed := r.findKey(keySHA)
	return listed
}

func (r *Role) channelID() string {
	if ingest := r.node.Topology().Ingest; ingest != nil && ingest.ChannelID != nil {
		return *ingest.ChannelID
	}
	return ""
}

type keyRoute struct {
	role   *Role
	keyID  string
	keySHA string
}

func routeKey(target, key string) (route keyRoute, matched bool) {
	keySHA := keyDigest(key)
	if keySHA == "" {
		return keyRoute{}, false
	}
	active.RLock()
	roles := make([]*Role, 0, len(active.bySession))
	for _, role := range active.bySession {
		roles = append(roles, role)
	}
	active.RUnlock()
	for _, role := range roles {
		if target != "" && target != role.node.SessionID && target != role.channelID() {
			continue
		}
		entry, listed := role.findKey(keySHA)
		if !listed {
			continue
		}
		matched = true
		if role.accepting() != "" {
			continue
		}
		if route.role == nil || newerRoute(role, route.role) {
			route = keyRoute{role: role, keyID: entry.KeyID, keySHA: keySHA}
		}
	}
	return route, matched
}

func newerRoute(candidate, current *Role) bool {
	if candidate.node.SessionID != current.node.SessionID && !candidate.node.StartedAt.Equal(current.node.StartedAt) {
		return candidate.node.StartedAt.After(current.node.StartedAt)
	}
	if newerNode(candidate.node, current.node) {
		return true
	}
	if newerNode(current.node, candidate.node) {
		return false
	}
	return candidate.node.StartedAt.After(current.node.StartedAt)
}

type admission struct {
	id                    string
	nonce, identity, kind *string
	proof                 *mc.UsageProof
	grant                 *mc.ReceiptGrant
}

func (a admission) receiptKey() string {
	if a.grant == nil {
		return ""
	}
	return a.grant.Key
}

func (r *Role) watchReceipts(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.checkReceipts()
		}
	}
}

func (r *Role) checkReceipts() {
	r.mu.Lock()
	var bound []*Publisher
	for publisher := range r.publishers {
		if publisher.meter.ReceiptBound() {
			bound = append(bound, publisher)
		}
	}
	r.mu.Unlock()
	enforced := usage.EnforcesReceipts(r.node.Topology().Usage, ProtocolWHIP)
	for _, publisher := range bound {
		reason := publisher.meter.ReceiptRefusal()
		switch {
		case reason != "" && enforced:
			log.Printf("media ingest %s: publisher closed: %s", r.node.NodeID, reason)
			publisher.Stop("timeout")
		case reason != "":
			publisher.meter.NoteReceiptRefusal(reason)
		}
	}
}

func (r *Role) startPublisher(protocol, keyID, keySHA string, admitted admission, closeFn func(string),
	tracks []TrackInfo, keyNeeded func()) (publisher *Publisher, started bool) {
	nonce, identity := admitted.nonce, admitted.identity
	publisher = &Publisher{Protocol: protocol, TokenNonce: nonce, Identity: identity, Close: closeFn}
	usageIdentity := identity
	if keyID != "" {
		publisher.StreamKeyID = broadcast.StringPtr(keyID)
		usageIdentity = broadcast.StringPtr("stream_key:" + keyID)
	}
	state := &publisherState{keySHA256: keySHA, ended: make(chan struct{}), tracks: tracks,
		usage: r.env.Usage.OpenWith(usage.ConnectionInfo{ID: admitted.id, Proof: admitted.proof, Kind: admitted.kind, TokenNonce: nonce,
			Identity: usageIdentity, Direction: broadcast.DirectionIngress, Transport: protocol, Detached: protocol == ProtocolWHIP,
			ReceiptKey: admitted.receiptKey()})}
	state.usage.Meter().SetClassify(state.mediaClass)
	publisher.meter = state.usage.Meter()
	refusal := ""
	if ladder := r.node.Topology().Ladder; mc.IsSourceLadder(ladder) {
		state.check, state.videoTrack = newSourceCheck(ladder), slices.IndexFunc(tracks, func(t TrackInfo) bool { return t.Video })
		refusal = sourceRefusal(tracks)
	}
	r.mu.Lock()
	r.publishers[publisher] = state
	r.mu.Unlock()
	if r.credentialRevoked(publisher, state, time.Now()) {
		publisher.Stop("revoked")
		return publisher, false
	}
	if refusal != "" {
		r.refuseSource(publisher, state, refusal)
		return publisher, true
	}
	r.hub.Attach(publisher, tracks, keyNeeded)
	r.env.Publishers.Store(1)
	r.env.Emitter.Event(mc.IngestConnectedBody{Kind: "ingest_connected", Protocol: protocol,
		StreamKeyID: publisher.StreamKeyID, TokenNonce: nonce})
	if state.check != nil {
		go r.watchKeyframes(publisher, state)
	}
	return publisher, true
}

func (r *Role) credentialRevoked(publisher *Publisher, state *publisherState, now time.Time) bool {
	switch {
	case state.keySHA256 != "" && !r.keyListed(state.keySHA256):
		return true
	case publisher.TokenNonce != nil && publisher.Identity != nil && r.node.Revoked(*publisher.TokenNonce, *publisher.Identity, now):
		return true
	}
	return !r.allows(publisher.Protocol)
}

const sourceStartGrace = 5 * time.Second

func (r *Role) watchKeyframes(publisher *Publisher, state *publisherState) {
	gop := time.Duration(state.check.ladder.GOPMS) * time.Millisecond
	ticker := time.NewTicker(gop / 2)
	defer ticker.Stop()
	started := time.Now()
	for {
		select {
		case <-state.ended:
			return
		case now := <-ticker.C:
			if state.refused.Load() {
				return
			}
			limit := 2*gop + sourceStartGrace
			if state.check.lastKeyAt().IsZero() {
				if waited := now.Sub(started); waited > limit {
					r.refuseSource(publisher, state, fmt.Sprintf("no video keyframe for %d ms (one at least every %d ms)",
						waited.Milliseconds(), gop.Milliseconds()))
					return
				}
				continue
			}
			if silent := now.Sub(state.check.lastVideoAt()); silent > limit {
				r.refuseSource(publisher, state, fmt.Sprintf("no video for %d ms (a keyframe at least every %d ms)",
					silent.Milliseconds(), gop.Milliseconds()))
				return
			}
		}
	}
}

func (r *Role) refuseSource(publisher *Publisher, state *publisherState, rule string) {
	if state.refused.Swap(true) {
		return
	}
	log.Printf("media ingest %s: %s source refused for the source ladder: %s", r.node.NodeID, publisher.Protocol, rule)
	publisher.Stop(mc.IngestSourceIncompatible)
}

func (r *Role) write(publisher *Publisher, unit Unit) {
	r.mu.Lock()
	state := r.publishers[publisher]
	r.mu.Unlock()
	if state != nil {
		size := unit.size()
		if publisher.Protocol == ProtocolWHIP {
			state.usage.AddV1(size)
		} else {
			state.usage.Add(size)
		}
		if unit.Track < len(state.tracks) {
			state.saw(unit, state.tracks[unit.Track])
		}
		r.env.IngressB.Add(int64(size))
		if state.refused.Load() {
			return
		}
		if state.check != nil && unit.Track == state.videoTrack {
			if rule := state.check.video(unit); rule != "" {
				r.refuseSource(publisher, state, rule)
				return
			}
		}
	}
	r.hub.Write(publisher, unit)
}

func (r *Role) endPublisher(publisher *Publisher, err error) {
	r.mu.Lock()
	state := r.publishers[publisher]
	delete(r.publishers, publisher)
	r.mu.Unlock()
	if state == nil {
		return
	}
	close(state.ended)
	state.usage.Close()
	if r.hub.Detach(publisher) {
		r.env.Publishers.Store(0)
	}
	reason := publisher.Reason()
	if reason == "" {
		reason = disconnectReason(err)
	}
	r.env.Emitter.Event(mc.IngestDisconnectedBody{Kind: "ingest_disconnected", Protocol: publisher.Protocol,
		StreamKeyID: publisher.StreamKeyID, Reason: reason})
}

func disconnectReason(err error) string {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.As(err, &liberrors.ErrReaderUnpublished{}) {
		return "ended"
	}
	return "timeout"
}

func (r *Role) enforce() {
	now := time.Now()
	r.mu.Lock()
	var revoked []*Publisher
	for publisher, state := range r.publishers {
		if r.credentialRevoked(publisher, state, now) {
			revoked = append(revoked, publisher)
		}
	}
	r.mu.Unlock()
	for _, publisher := range revoked {
		publisher.Stop("revoked")
	}
}

func (r *Role) stopAll(reason string) {
	r.mu.Lock()
	publishers := make([]*Publisher, 0, len(r.publishers))
	for publisher := range r.publishers {
		publishers = append(publishers, publisher)
	}
	r.mu.Unlock()
	for _, publisher := range publishers {
		publisher.Stop(reason)
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
	broadcast.Unbounded(response)
	sub := r.hub.Subscribe(request.Context().Done())
	if sub == nil {
		return
	}
	defer r.hub.Unsubscribe(sub)
	link := r.env.Usage.Open(nil, broadcast.StringPtr(mc.NodeIdentityPrefix+child), broadcast.DirectionEgress, broadcast.TransportChild)
	defer link.Close()
	r.env.Subscribers.Add(1)
	defer r.env.Subscribers.Add(-1)
	response.Header().Set("Content-Type", "video/mp2t")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	writer := &countingWriter{w: response, usage: link, total: &r.env.EgressB}
	_ = sub.WriteTS(writer, flusher(response), request.Context().Done())
}

type countingWriter struct {
	w     io.Writer
	usage *broadcast.Connection
	total interface{ Add(int64) int64 }
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.usage.Add(n)
	c.total.Add(int64(n))
	return n, err
}

func newerNode(candidate, current *broadcast.Node) bool {
	a, b := nodeOrder(candidate), nodeOrder(current)
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func nodeOrder(node *broadcast.Node) [4]int64 {
	var order [4]int64
	if topology := node.Topology(); topology != nil {
		order[0], order[1] = topology.Epoch, topology.Revision
	}
	if node.Spec != nil {
		order[2], order[3] = int64(node.Spec.Identity.Epoch), int64(node.Spec.Identity.Attempt)
	}
	return order
}
