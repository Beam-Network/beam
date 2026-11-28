package usage

import (
	"crypto/rand"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type accounting struct {
	presence presence
	media    atomic.Int64
	overhead atomic.Int64
	wire     *atomic.Int64
	classify func(since time.Time) string
	objects  objectLog

	mediaSeen, overheadSeen, connectedSeen int64
	sampledMS                              int64
	lastClass                              string
	mediaAtLoss                            int64
	classMS                                mediacontracts.ClassAmounts
	classBytes                             mediacontracts.ClassAmounts
	linkBytes                              int64
	proof                                  *mediacontracts.UsageProof
	receipts                               *mediacontracts.ReceiptConnection
	latest                                 *mediacontracts.UsageReceipt
	receiptNoted                           bool
	stopped                                bool
	draining                               bool
	replacedBy                             *Connection
}

func NewConnectionID() string {
	random := make([]byte, 16)
	_, _ = rand.Read(random)
	return "c_" + mediacontracts.B64URL(random)
}

func (r *Reporter) V2() bool {
	if r.config.Features == nil {
		return false
	}
	v2, _ := r.config.Features()
	return v2
}

func (r *Reporter) TakesReceipts() bool {
	if r.config.Features == nil {
		return false
	}
	v2, receipts := r.config.Features()
	return v2 && receipts
}

func (c *Connection) openPresence(now, periodStart int64) {
	if c.info.RequestTargetMS > 0 {
		c.presence = newRequests(c.info.RequestTargetMS)
		c.presence.setFloor(periodStart)
		return
	}
	line := newTransport()
	line.setFloor(periodStart)
	if !c.info.Detached {
		line.connect(now)
		defer c.startReceipts()
	}
	c.presence = line
}

func (c *Connection) Connect() {
	if c == nil {
		return
	}
	now := c.reporter.NowMS()
	c.mu.Lock()
	if line, ok := c.presence.(*transport); ok {
		c.sentWithoutICE(now)
		line.connect(now)
		c.startReceipts()
	}
	c.mu.Unlock()
}

func (c *Connection) Disconnect() {
	if c == nil {
		return
	}
	now := c.reporter.NowMS()
	c.mu.Lock()
	if line, ok := c.presence.(*transport); ok {
		if line.lose(now) {
			c.mediaAtLoss = c.media.Load()
		}
	}
	c.mu.Unlock()
}

func (c *Connection) sentWithoutICE(at int64) {
	line, ok := c.presence.(*transport)
	if !ok || c.info.Direction != DirectionEgress {
		return
	}
	if total := c.media.Load(); line.lost != none && total != c.mediaAtLoss {
		c.mediaAtLoss = total
		line.sent(c.sampledMS, at)
	}
}

func (c *Connection) Begin(at int64) {
	c.mu.Lock()
	if line, ok := c.presence.(*requests); ok {
		line.begin(at)
		c.startReceipts()
	}
	c.mu.Unlock()
}

func (c *Connection) Finish(at int64, n int, class string, media bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if next := c.replacedBy; next != nil {
		next.Finish(at, n, class, media)
		return
	}
	if line, ok := c.presence.(*requests); ok {
		line.finish(at)
		if c.draining && line.inflight == 0 {
			c.draining, c.closedAt = false, c.reporter.config.Now()
		}
	}
	if n <= 0 {
		return
	}
	c.media.Add(int64(n))
	c.mediaSeen += int64(n)
	if c.service() {
		c.linkBytes += int64(n)
		return
	}
	c.classBytes.Add(class, int64(n))
	if media {
		c.objects.add(at, class)
	}
}

func (c *Connection) Replace(next *Connection, at int64) {
	now := c.reporter.config.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	next.mu.Lock()
	defer next.mu.Unlock()
	if !c.closedAt.IsZero() || c.draining {
		return
	}
	line, ok := c.presence.(*requests)
	other, also := next.presence.(*requests)
	if !ok || !also {
		return
	}
	c.sampleLocked(now)
	c.sampleV2(c.reporter.NowMS())
	line.end(at)
	other.inflight += line.inflight
	line.inflight = 0
	c.replacedBy, c.closedAt = next, now
}

func (c *Connection) Expired(at int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	line, ok := c.presence.(*requests)
	return ok && line.expired(at)
}

func (c *Connection) CloseExpired() { c.closeAt(c.reporter.NowMS()) }

func (c *Connection) AddRTP(v1, media, overhead int) {
	if c == nil {
		return
	}
	if v1 > 0 {
		c.bytes.Add(int64(v1))
	}
	if media > 0 {
		c.media.Add(int64(media))
	}
	if overhead > 0 {
		c.overhead.Add(int64(overhead))
	}
}

func (c *Connection) SetWire(wire *atomic.Int64) {
	c.mu.Lock()
	c.wire = wire
	c.mu.Unlock()
}

func (c *Connection) SetClassify(classify func(since time.Time) string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.classify = classify
	c.mu.Unlock()
}

func (c *Connection) dropPeriod(at int64) {
	c.presence.cut(at)
	c.takeReceipt(at)
	c.presence.short()
	c.objects.keep(at, c.info.RequestTargetMS)
	c.overheadSeen = c.overheadTotal()
	c.classMS, c.classBytes, c.linkBytes = mediacontracts.ClassAmounts{}, mediacontracts.ClassAmounts{}, 0
}

func (c *Connection) overheadTotal() int64 {
	if c.wire == nil {
		return c.overhead.Load()
	}
	return max(c.wire.Load()-c.media.Load(), c.overheadSeen)
}

func (c *Connection) sampleV2(at int64) {
	since := c.sampledMS
	c.sentWithoutICE(at)
	c.sampledMS = at
	total, connected := c.media.Load(), max(c.presence.total(at), c.connectedSeen)
	moved, elapsed := total-c.mediaSeen, connected-c.connectedSeen
	c.mediaSeen, c.connectedSeen = total, connected
	if moved <= 0 || c.info.RequestTargetMS > 0 {
		return
	}
	if c.service() {
		c.linkBytes += moved
		return
	}
	class := ""
	if c.classify != nil {
		class = c.classify(time.UnixMilli(since))
	}
	if class == "" {
		class = c.lastClass
	}
	if class == "" {
		class = mediacontracts.ClassAudio
	}
	c.lastClass = class
	c.classBytes.Add(class, moved)
	c.classMS.Add(class, max(elapsed, 0))
}

func (c *Connection) skip(from, to int64) {
	c.mu.Lock()
	c.presence.hole(from, to)
	c.mu.Unlock()
}

func (c *Connection) deduct(ms, last, at int64) {
	c.mu.Lock()
	c.presence.deduct(ms, last, at)
	c.mu.Unlock()
}

func (c *Connection) stop(at int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.presence.end(at)
	c.stopped = true
}

func (c *Connection) keepProof(proof *mediacontracts.UsageProof) {
	c.mu.Lock()
	c.proof = proof
	c.mu.Unlock()
}

func (c *Connection) row(start, end int64) (row mediacontracts.UsageConnectionV2, present, over bool) {
	connected := c.presence.cut(end)
	over = !c.closedAt.IsZero()
	ended := (over || c.stopped) && !c.presence.more()
	over = over && ended
	row = mediacontracts.UsageConnectionV2{ConnectionID: c.id, TokenNonce: c.info.TokenNonce, Identity: c.info.Identity,
		Direction: c.info.Direction, Transport: c.info.Transport, StartMS: start, EndMS: start, Ended: ended, LossPPM: c.lossPPM, RTTMS: c.rttMS}
	if row.TokenNonce != nil {
		kind := mediacontracts.KindHuman
		if c.info.Kind != nil {
			kind = *c.info.Kind
		}
		row.Kind = &kind
	}
	if len(connected) > 0 {
		row.StartMS, row.EndMS = connected[0][0], connected[len(connected)-1][1]
		for _, interval := range connected {
			row.ConnectedMS += interval[1] - interval[0]
		}
		row.ConnectedMS = max(row.ConnectedMS-c.presence.short(), 0)
		if merged := mergeIntervals(connected); len(merged) > 1 {
			row.Intervals = &merged
		}
	}
	classMS := c.classMS
	if c.info.RequestTargetMS > 0 {
		classMS = c.objects.classMS(connected, c.info.RequestTargetMS)
		c.objects.keep(end, c.info.RequestTargetMS)
	}
	total := c.overheadTotal()
	if meters := c.info.Transport; meters == TransportWHIP || meters == TransportWHEP || c.service() {
		row.OverheadBytes = max(total-c.overheadSeen, 0)
	}
	c.overheadSeen = total
	if c.service() {
		row.MediaBytes = c.linkBytes
	} else {
		classMS = limitClasses(classMS, row.ConnectedMS)
		classBytes := c.classBytes
		row.ClassMS, row.ClassBytes = &classMS, &classBytes
		row.MediaMS, row.MediaBytes = classMS.Sum(), classBytes.Sum()
	}
	c.classMS, c.classBytes, c.linkBytes = mediacontracts.ClassAmounts{}, mediacontracts.ClassAmounts{}, 0
	moved := row.MediaBytes > 0 || row.OverheadBytes > 0 || c.latest != nil && c.latest.ReceivedMS <= end
	switch _, begun := c.presence.started(); {
	case c.info.Versions == OnlyV1:
		return row, false, over
	case c.id == mediacontracts.AnonymousConnectionID:
		row.Ended = false
		return row, row.MediaBytes > 0, over
	case !begun:
		return row, false, over
	}
	present = moved || len(connected) > 0 || ended
	if present {
		row.Proof, c.proof = c.proof, nil
		row.Receipt = c.takeReceipt(end)
	}
	return row, present, over
}

func mergeIntervals(list []mediacontracts.UsageInterval) []mediacontracts.UsageInterval {
	merged := append([]mediacontracts.UsageInterval{}, list...)
	for len(merged) > 1 {
		closest := 1
		for index := 2; index < len(merged); index++ {
			if merged[index][0]-merged[index-1][1] < merged[closest][0]-merged[closest-1][1] {
				closest = index
			}
		}
		if len(merged) <= mediacontracts.UsageIntervalsMax && merged[closest][0]-merged[closest-1][1] >= mediacontracts.UsageOutageMinMS {
			break
		}
		merged[closest-1][1] = merged[closest][1]
		merged = append(merged[:closest], merged[closest+1:]...)
	}
	return merged
}

func limitClasses(amounts mediacontracts.ClassAmounts, limit int64) mediacontracts.ClassAmounts {
	excess := amounts.Sum() - limit
	for _, amount := range []*int64{&amounts.Audio, &amounts.SD, &amounts.HD, &amounts.FHD, &amounts.UHD} {
		if excess <= 0 {
			break
		}
		cut := min(excess, *amount)
		*amount -= cut
		excess -= cut
	}
	return amounts
}

func (c *Connection) Started() (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.presence.started()
}

func (c *Connection) NowMS() int64 { return c.reporter.NowMS() }

func (c *Connection) Totals(at int64) (connectedMS, mediaBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.presence.total(at), c.media.Load()
}

func (c *Connection) service() bool {
	return c.info.Transport == TransportChild || c.info.Transport == TransportUpstream
}

func GrantProof(grant string, verifiedMS int64) *mediacontracts.UsageProof {
	return &mediacontracts.UsageProof{Type: mediacontracts.ProofGrant, VerifiedMS: verifiedMS, Grant: &grant}
}

func RequestGrant(request *http.Request) string {
	values := request.URL.Query()[mediacontracts.ReceiptGrantQueryParameter]
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

func (r *Reporter) BindGrant(value string, settings *mediacontracts.UsageSettings, claims mediacontracts.TokenClaims,
	verifiedMS int64) (mediacontracts.ReceiptGrant, bool) {
	if value == "" || settings == nil || len(settings.GrantKeys) == 0 || !r.TakesReceipts() {
		return mediacontracts.ReceiptGrant{}, false
	}
	grant, err := mediacontracts.VerifyReceiptGrant(value, mediacontracts.ReceiptGrantContext{Keys: settings.GrantKeys,
		Session: r.config.SessionID, Nonce: claims.Nonce, Identity: claims.Identity, Role: claims.Role,
		Kind: mediacontracts.ParticipantKindOf(claims), Now: verifiedMS / 1000})
	return grant, err == nil
}

func (r *Reporter) TokenProof(token string, settings *mediacontracts.UsageSettings, connectionID, nonce string, verifiedMS int64) *mediacontracts.UsageProof {
	if token == "" || settings == nil || len(settings.SealKeys) == 0 {
		return nil
	}
	proof, err := mediacontracts.SealToken(token, settings.SealKeys[0], mediacontracts.ProofBinding{SessionID: r.config.SessionID,
		NodeID: r.config.NodeID, WorkloadID: r.config.WorkloadID, ConnectionID: connectionID, TokenNonce: nonce}, verifiedMS)
	if err != nil {
		return nil
	}
	return &proof
}

func (r *Reporter) OpenAnonymous() *Connection {
	return r.Open(ConnectionInfo{ID: mediacontracts.AnonymousConnectionID, Direction: DirectionEgress, Transport: TransportHLS,
		Detached: true, Versions: OnlyV2})
}

func (c *Connection) Proof() *mediacontracts.UsageProof {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.proof
}
