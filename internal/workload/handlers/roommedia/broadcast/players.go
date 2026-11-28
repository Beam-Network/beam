package broadcast

import (
	"container/list"
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

type Players struct {
	reporter *usage.Reporter
	node     *Node
	targetMS int64
	tags     *tagLog
	tagged   *tagCache

	mu                 sync.Mutex
	byNonce            map[string]*viewerNonce
	byTag              map[string]*viewerNonce
	learned            *list.List
	current            map[*player]struct{}
	links              map[string]*usage.Connection
	noViewer           *usage.Connection
	sizes              map[string]mc.VideoSize
	addresses, signers minuteLimiter
	maxNonces          int
}

const (
	tagTTL             = 24 * time.Hour
	maxTags            = 200_000
	maxPlayersPerNonce = 8
	maxMinted          = 512
)

type viewerNonce struct {
	nonce, identity, tag string
	kind                 string
	tokenIAT             int64
	bytes                byteCheck
	seen                 int64
	saved                int64
	players              []*player
	minted               map[int64]*player
	issued               []int64
	credentials          map[int64]int64
	late                 *player
	lateIAT              int64
	asked                *player
	element              *list.Element
}

type player struct {
	viewer                                          *viewerNonce
	key                                             string
	connection                                      *usage.Connection
	active                                          int64
	grant                                           string
	foreign                                         bool
	signed, signedAt, openedAt, answered, renewedAt int64
	refused                                         string
}

func NewPlayers(reporter *usage.Reporter, node *Node, stateDir string) *Players {
	players := &Players{reporter: reporter, node: node, targetMS: targetDurationMS(node.Topology()), tags: openTagLog(stateDir), tagged: newTagCache(),
		byNonce: map[string]*viewerNonce{}, byTag: map[string]*viewerNonce{}, learned: list.New(), current: map[*player]struct{}{},
		links: map[string]*usage.Connection{}, sizes: map[string]mc.VideoSize{}, maxNonces: maxTags}
	now := reporter.NowMS()
	for _, entry := range players.tags.load(now) {
		viewer := players.learn(entry.Nonce, entry.Identity, "", entry.SeenMS)
		viewer.saved = entry.SeenMS
	}
	return players
}

func targetDurationMS(topology *mc.Topology) int64 {
	if topology == nil || topology.Packaging == nil {
		return 6_000
	}
	return 1000 * max(mc.HLSTargetDurationS(topology.Ladder, *topology.Packaging), 1)
}

func (p *Players) Now() int64 { return p.reporter.NowMS() }

type Answer struct {
	clock      func() int64
	connection *usage.Connection
	class      string
	media      bool
	once       sync.Once
}

func (a *Answer) Done(n int) {
	if a != nil {
		a.once.Do(func() { a.connection.Finish(a.clock(), n, a.class, a.media) })
	}
}

func (p *Players) learn(nonce, identity, kind string, at int64) *viewerNonce {
	viewer := p.byNonce[nonce]
	if viewer == nil {
		viewer = &viewerNonce{nonce: nonce, identity: identity, tag: mc.PlaybackTag(p.node.PlaybackSecret(), nonce),
			minted: map[int64]*player{}, credentials: map[int64]int64{}}
		viewer.element = p.learned.PushBack(viewer)
		p.byNonce[nonce] = viewer
		p.byTag[viewer.tag] = viewer
		for p.learned.Len() > p.maxNonces {
			p.forget(p.learned.Front().Value.(*viewerNonce))
		}
	} else {
		p.learned.MoveToBack(viewer.element)
	}
	viewer.identity, viewer.seen = identity, max(viewer.seen, at)
	if kind != "" {
		viewer.kind = kind
	}
	return viewer
}

func (p *Players) forget(viewer *viewerNonce) {
	p.learned.Remove(viewer.element)
	delete(p.byNonce, viewer.nonce)
	if p.byTag[viewer.tag] == viewer {
		delete(p.byTag, viewer.tag)
	}
}

func (p *Players) remember(viewer *viewerNonce, at int64) {
	if !p.reporter.V2() || (viewer.saved != 0 && at-viewer.saved < time.Hour.Milliseconds()) {
		return
	}
	viewer.saved = at
	p.tags.append(tagEntry{Nonce: viewer.nonce, Identity: viewer.identity, SeenMS: at})
}

func (v *viewerNonce) find(key string) *player {
	for _, existing := range v.players {
		if existing.key == key {
			return existing
		}
	}
	return nil
}

func (v *viewerNonce) player(key string) *player {
	if existing := v.find(key); existing != nil {
		return existing
	}
	created := &player{viewer: v, key: key}
	v.players = append(v.players, created)
	return created
}

func (p *Players) live(target *player, at int64) *usage.Connection {
	if target.connection == nil {
		return nil
	}
	if _, current := p.current[target]; current && !target.connection.Expired(at) {
		return target.connection
	}
	p.end(target)
	return nil
}

func (p *Players) end(target *player) {
	if _, current := p.current[target]; current {
		delete(p.current, target)
		p.settleBytes(target)
		target.connection.CloseExpired()
	}
	target.connection = nil
	if target.key == "" {
		p.rebase(target.viewer)
	}
}

func (p *Players) open(target *player, id string, proof *mc.UsageProof) *usage.Connection {
	viewer := target.viewer
	currents := 0
	var oldest *player
	for _, other := range viewer.players {
		if _, current := p.current[other]; current {
			currents++
			if oldest == nil || other.active < oldest.active {
				oldest = other
			}
		}
	}
	if currents >= maxPlayersPerNonce {
		p.end(oldest)
	}
	kind := viewer.kind
	if kind == "" {
		kind = mc.KindHuman
	}
	target.connection = p.reporter.Open(usage.ConnectionInfo{ID: id, Proof: proof, ReceiptKey: target.key, TokenNonce: StringPtr(viewer.nonce), Identity: StringPtr(viewer.identity),
		Kind: &kind, Direction: DirectionEgress, Transport: TransportHLS, RequestTargetMS: p.targetMS, Versions: usage.OnlyV2})
	target.foreign, target.signed, target.answered, target.signedAt = false, 0, 0, 0
	target.openedAt = p.reporter.MonoMS()
	target.renewedAt = target.openedAt
	p.current[target] = struct{}{}
	if target.key != "" {
		p.note(viewer)
	}
	if len(viewer.players) > 2*maxPlayersPerNonce {
		kept := viewer.players[:0]
		for _, other := range viewer.players {
			if _, current := p.current[other]; current || other == viewer.late || other.key == "" || other == viewer.asked {
				kept = append(kept, other)
			}
		}
		viewer.players = kept
		for issued, minted := range viewer.minted {
			if !slices.Contains(kept, minted) {
				delete(viewer.minted, issued)
			}
		}
	}
	return target.connection
}

func (p *Players) request(target *player, at int64, class string, media bool) *Answer {
	connection := p.live(target, at)
	if connection == nil {
		connection = p.open(target, "", nil)
	}
	target.active = at
	connection.Begin(at)
	return &Answer{clock: p.reporter.NowMS, connection: connection, class: class, media: media}
}

func (p *Players) link(identity, direction, transport string, at int64) *Answer {
	key := transport + ":" + identity
	connection := p.links[key]
	if connection != nil && connection.Expired(at) {
		connection.CloseExpired()
		connection = nil
	}
	if connection == nil {
		connection = p.reporter.Open(usage.ConnectionInfo{Identity: StringPtr(mc.NodeIdentityPrefix + identity), Direction: direction,
			Transport: transport, RequestTargetMS: p.targetMS, Versions: usage.OnlyV2})
		p.links[key] = connection
	}
	connection.Begin(at)
	return &Answer{clock: p.reporter.NowMS, connection: connection}
}

func (p *Players) Multivariant(access *HLSAccess, at int64) *Answer {
	token, grantText := access.Token, access.Grant
	access.Token, access.Grant = "", ""
	if access.Child != "" {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.link(access.Child, DirectionEgress, TransportChild, at)
	}
	grant, bound := p.reporter.BindGrant(grantText, p.node.Topology().Usage, mc.TokenClaims{Nonce: access.Nonce,
		Identity: access.Identity, Role: access.Role, Kind: &access.Kind}, at)
	id := usage.NewConnectionID()
	var proof *mc.UsageProof
	if bound {
		proof = usage.GrantProof(grantText, at)
	} else if p.opens(access.Nonce, at) {
		proof = p.reporter.TokenProof(token, p.node.Topology().Usage, id, access.Nonce, at)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	viewer := p.learn(access.Nonce, access.Identity, access.Kind, at)
	p.remember(viewer, at)
	viewer.tokenIAT = max(viewer.tokenIAT, access.TokenIAT)
	target := viewer.player(grant.Key)
	if bound {
		target.grant = grantText
	}
	p.receiptRefusal(target, at)
	target.refused = ""
	if p.live(target, at) == nil {
		p.open(target, id, proof)
	}
	p.mint(viewer, target, access)
	return p.request(target, at, mc.ClassAudio, false)
}

func (p *Players) opens(nonce string, at int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	viewer := p.byNonce[nonce]
	return viewer == nil || p.live(viewer.player(""), at) == nil
}

func (p *Players) mint(viewer *viewerNonce, target *player, access *HLSAccess) {
	if access.Credential == "" {
		return
	}
	issued := access.IssuedMS
	for viewer.minted[issued] != nil {
		issued++
	}
	if issued != access.IssuedMS {
		credential, err := mc.MintPlaybackCredential(p.node.PlaybackSecret(), mc.PlaybackCredential{SessionID: p.node.SessionID,
			Identity: access.Identity, Nonce: access.Nonce, IssuedAtMS: issued})
		if err != nil {
			return
		}
		access.Credential, access.IssuedMS = credential, issued
	}
	viewer.minted[access.IssuedMS] = target
	viewer.issued = append(viewer.issued, access.IssuedMS)
	if len(viewer.issued) > maxMinted {
		delete(viewer.minted, viewer.issued[0])
		viewer.issued = viewer.issued[1:]
	}
}

func (p *Players) Playlist(access HLSAccess, renditionID string, at int64) (answer *Answer, refusal string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if access.Child != "" {
		return p.link(access.Child, DirectionEgress, TransportChild, at), ""
	}
	viewer := p.learn(access.Nonce, access.Identity, "", at)
	p.remember(viewer, at)
	target := viewer.minted[access.IssuedMS]
	minted := target != nil
	if !minted {
		target = p.foreign(viewer, access.IssuedMS)
	}
	if refusal := p.receiptRefusal(target, at); refusal != "" {
		return nil, refusal
	}
	viewer.asked = target
	opens := p.live(target, at) == nil
	answer = p.request(target, at, p.class(renditionID), false)
	if opens {
		target.foreign = !minted
	}
	return answer, ""
}

func (p *Players) foreign(viewer *viewerNonce, issued int64) *player {
	if viewer.late != nil && viewer.lateIAT == 0 {
		viewer.lateIAT = issued
	}
	if viewer.late != nil && viewer.lateIAT == issued {
		return viewer.late
	}
	if _, known := viewer.credentials[issued]; known || len(viewer.credentials) < maxMinted {
		viewer.credentials[issued] = p.reporter.MonoMS()
	}
	return viewer.player("")
}

func (p *Players) Object(request *http.Request, access HLSAccess, renditionID string, media bool, at int64) *Answer {
	tag := ""
	if request.URL.RawQuery != "" {
		tag = mc.RequestPlaybackTag(request.URL.Query()[mc.PlaybackTagQueryParameter])
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if access.Child != "" {
		return p.link(access.Child, DirectionEgress, TransportChild, at)
	}
	class := p.class(renditionID)
	viewer := p.byTag[tag]
	if tag == "" || viewer == nil || at-viewer.seen >= tagTTL.Milliseconds() {
		return p.anonymous(class)
	}
	target := viewer.asked
	if target == nil || p.receiptRefusal(target, at) != "" {
		target = viewer.player("")
	}
	opens := p.live(target, at) == nil
	answer := p.request(target, at, class, media)
	if opens {
		target.foreign = viewer.asked == nil
	}
	return answer
}

func (p *Players) anonymous(class string) *Answer {
	if p.noViewer == nil {
		p.noViewer = p.reporter.OpenAnonymous()
	}
	return &Answer{clock: p.reporter.NowMS, connection: p.noViewer, class: class}
}

func (p *Players) Upstream(parent string, at int64) *Answer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.link(parent, DirectionIngress, TransportUpstream, at)
}

func (p *Players) class(renditionID string) string {
	var size *mc.VideoSize
	ladder := p.node.Topology().Ladder
	known, learned := p.sizes[renditionID]
	if learned && (ladder == nil || passthrough(ladder, renditionID)) {
		size = &known
	}
	if class := mc.RenditionClass(ladder, renditionID, size); class != "" {
		return class
	}
	if learned {
		return mc.VideoClass(known.Width, known.Height)
	}
	return mc.ClassSD
}

func passthrough(ladder *mc.TranscodeLadder, renditionID string) bool {
	for _, video := range ladder.Video {
		if video.RenditionID == renditionID {
			return video.Passthrough
		}
	}
	return true
}

func (p *Players) LearnVariants(body []byte) {
	sizes := variantSizes(body)
	if len(sizes) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for rendition, size := range sizes {
		if _, known := p.sizes[rendition]; known || len(p.sizes) < 64 {
			p.sizes[rendition] = size
		}
	}
}

func (p *Players) Tag(body []byte, access HLSAccess) []byte {
	if access.Child != "" || access.Nonce == "" || !p.reporter.V2() {
		return body
	}
	p.mu.Lock()
	viewer := p.byNonce[access.Nonce]
	tag := ""
	if viewer != nil {
		tag = viewer.tag
	}
	p.mu.Unlock()
	if tag == "" {
		return body
	}
	return p.tagged.tag(body, tag)
}

func (p *Players) Connection(nonce string) *usage.Connection {
	p.mu.Lock()
	defer p.mu.Unlock()
	var last *player
	if viewer := p.byNonce[nonce]; viewer != nil {
		for _, candidate := range viewer.players {
			if _, current := p.current[candidate]; current && (last == nil || candidate.active >= last.active) {
				last = candidate
			}
		}
	}
	if last == nil {
		return nil
	}
	return last.connection
}

func (p *Players) Anonymous() *usage.Connection {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.noViewer
}

func (p *Players) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (p *Players) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	tagsDone := make(chan struct{})
	go func() {
		defer close(tagsDone)
		p.tags.run(ctx, p.reporter.NowMS)
	}()
	defer func() { <-tagsDone }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sweep(p.reporter.NowMS())
		}
	}
}

func (p *Players) Sweep() { p.sweep(p.reporter.NowMS()) }

func (p *Players) sweep(at int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for target := range p.current {
		if target.connection.Expired(at) {
			p.end(target)
		} else {
			p.receiptRefusal(target, at)
		}
	}
	for key, connection := range p.links {
		if connection.Expired(at) {
			connection.CloseExpired()
			delete(p.links, key)
		}
	}
	for front := p.learned.Front(); front != nil; front = p.learned.Front() {
		viewer := front.Value.(*viewerNonce)
		if at-viewer.seen < tagTTL.Milliseconds() {
			return
		}
		p.forget(viewer)
	}
}
