package broadcast

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/ratelimit"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

const ReceiptName = "receipt"

const (
	maxReceiptBody     = 2 << 10
	receiptsPerAddress = 6000
	receiptsPerPlayer  = 60
)

func (p *Players) ServeReceipt(response http.ResponseWriter, request *http.Request) {
	if !p.reporter.TakesReceipts() {
		WriteError(response, http.StatusBadRequest, "invalid_request", "GET only")
		return
	}
	if request.Method != http.MethodPost {
		WriteError(response, http.StatusBadRequest, "invalid_request", "POST only")
		return
	}
	arrived := p.reporter.NowMS()
	now, address := time.Now(), ratelimit.ClientIP(request)
	if !p.addresses.allow(address, receiptsPerAddress, now) {
		WriteRateLimited(response)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxReceiptBody+1))
	var post mc.HLSReceiptPost
	if err != nil || len(body) > maxReceiptBody || mc.UnmarshalStrict(body, &post) != nil {
		WriteError(response, http.StatusBadRequest, "invalid_request", "")
		return
	}
	grant, err := mc.ParseReceiptGrant(post.Grant)
	if err != nil || grant.Session != p.node.SessionID {
		WriteError(response, http.StatusBadRequest, "invalid_request", "grant")
		return
	}
	if !p.signers.allow(address+" "+grant.Nonce, receiptsPerPlayer, now) {
		WriteRateLimited(response)
		return
	}
	if post.Receipt != nil && mc.ValidateHLSReceiptPost(post) != nil {
		WriteError(response, http.StatusBadRequest, "receipt_invalid", "")
		return
	}
	target, connection, err := p.receiptConnection(post.Grant, grant, arrived)
	if err == nil && post.Receipt != nil {
		err = p.takeReceipt(target, connection, *post.Receipt)
	}
	if err != nil {
		code := usage.ReceiptCode(err)
		switch code {
		case "receipt_unbound":
			WriteError(response, http.StatusNotFound, code, "")
		case "receipt_connection":
			WriteError(response, http.StatusConflict, code, "")
		case "rate_limited":
			WriteRateLimited(response)
		default:
			WriteError(response, http.StatusBadRequest, code, "")
		}
		return
	}
	encoded, err := mc.MarshalWire(mc.HLSReceiptAnswer{NodeID: p.node.NodeID, ConnectionID: connection.ID(), Sequence: connection.LastReceipt()})
	if err != nil {
		WriteError(response, http.StatusInternalServerError, "invalid_request", "")
		return
	}
	response.Header().Set("Content-Type", ContentTypeJSON)
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(encoded)
}

func (p *Players) receiptConnection(value string, grant mc.ReceiptGrant, at int64) (*player, *usage.Connection, error) {
	p.mu.Lock()
	viewer := p.byNonce[grant.Nonce]
	if viewer == nil {
		p.mu.Unlock()
		return nil, nil, usage.ErrReceiptUnbound
	}
	target := viewer.find(grant.Key)
	var connection *usage.Connection
	if target != nil {
		connection = p.live(target, at)
	}
	var late *player
	if unbound := viewer.find(""); connection == nil && unbound != nil && p.live(unbound, at) != nil && unbound.foreign && p.credentials(viewer) <= 1 {
		late = unbound
	}
	verified := target != nil && target.grant == value
	identity, tokenIAT := viewer.identity, viewer.tokenIAT
	p.mu.Unlock()
	if connection == nil && late == nil {
		return nil, nil, usage.ErrReceiptUnbound
	}
	expired := false
	if !verified || late != nil {
		context := mc.ReceiptGrantContext{Session: p.node.SessionID, Nonce: grant.Nonce, Identity: identity, Role: grant.Role, Kind: grant.Kind, Now: -1}
		if settings := p.node.Topology().Usage; settings != nil {
			context.Keys = settings.GrantKeys
		}
		if _, err := mc.VerifyReceiptGrant(value, context); err != nil {
			return nil, nil, usage.ErrReceiptUnbound
		}
		context.Now = at / 1000
		_, err := mc.VerifyReceiptGrant(value, context)
		expired = errors.Is(err, mc.ErrReceiptGrantExpired)
	}
	if late == nil {
		return target, connection, nil
	}
	if grant.IssuedAt < tokenIAT {
		return nil, nil, mc.ErrReceiptConnection
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	target = viewer.player(grant.Key)
	if connection := p.live(target, at); connection != nil {
		return target, connection, nil
	}
	old := p.live(late, at)
	if old == nil {
		return nil, nil, usage.ErrReceiptUnbound
	}
	var proof *mc.UsageProof
	if !expired {
		proof = usage.GrantProof(value, at)
	}
	target.grant = value
	connection = p.open(target, "", proof)
	delete(p.current, late)
	late.connection = nil
	p.rebase(viewer)
	connection.Begin(at)
	old.Replace(connection, at)
	connection.Finish(p.reporter.NowMS(), 0, "", false)
	target.active = at
	viewer.late, viewer.lateIAT = target, 0
	for issued := range viewer.credentials {
		viewer.lateIAT = issued
	}
	if viewer.asked == late {
		viewer.asked = target
	}
	return target, connection, nil
}

func (p *Players) takeReceipt(target *player, connection *usage.Connection, value string) error {
	receipt, taken, err := connection.ReceiptTime(value)
	if err != nil || !taken {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if target.connection != connection {
		return nil
	}
	_, target.answered = connection.Totals(0)
	target.signed, target.signedAt = receipt.MediaBytes, p.reporter.MonoMS()
	p.compareBytes(target)
	return nil
}

func (p *Players) credentials(viewer *viewerNonce) int {
	now := p.reporter.MonoMS()
	for issued, seen := range viewer.credentials {
		if now-seen > 3*p.targetMS {
			delete(viewer.credentials, issued)
		}
	}
	return len(viewer.credentials)
}

type byteCheck struct {
	running       bool
	base          mc.ReceiptPoint
	signed, bytes int64
	history       []mc.ReceiptPoint
	points        []mc.ReceiptPoint
	disagrees     bool
}

func (p *Players) bound(viewer *viewerNonce) []*player {
	var list []*player
	for _, target := range viewer.players {
		if target.key != "" && target.connection != nil {
			list = append(list, target)
		}
	}
	return list
}

func (p *Players) sums(viewer *viewerNonce) (signed, bytes int64) {
	signed, bytes = viewer.bytes.signed, viewer.bytes.bytes
	for _, target := range p.bound(viewer) {
		_, answered := target.connection.Totals(0)
		signed, bytes = signed+target.signed, bytes+answered
	}
	return signed, bytes
}

func (p *Players) note(viewer *viewerNonce) {
	check, now := &viewer.bytes, p.reporter.MonoMS()
	signed, bytes := p.sums(viewer)
	if !check.running {
		check.running, check.base = true, mc.ReceiptPoint{AtMS: now, ReceiptBytes: signed, NodeBytes: bytes}
	}
	check.history = append(check.history, mc.ReceiptPoint{AtMS: now, NodeBytes: bytes})
	for len(check.history) > 2 && check.history[1].AtMS <= now-3*mc.ReceiptMissingAfterMS {
		check.history = check.history[1:]
	}
}

func (p *Players) rebase(viewer *viewerNonce) {
	if check := &viewer.bytes; check.running {
		signed, bytes := p.sums(viewer)
		check.base = mc.ReceiptPoint{AtMS: p.reporter.MonoMS(), ReceiptBytes: signed, NodeBytes: bytes}
		check.points, check.disagrees = nil, false
	}
}

func (p *Players) unboundCurrent(viewer *viewerNonce, at int64) bool {
	unbound := viewer.find("")
	return unbound != nil && p.live(unbound, at) != nil
}

func (p *Players) compareBytes(signer *player) {
	viewer := signer.viewer
	check, now, at := &viewer.bytes, p.reporter.MonoMS(), p.reporter.NowMS()
	for _, target := range p.bound(viewer) {
		p.live(target, at)
	}
	p.note(viewer)
	signed, bytes := p.sums(viewer)
	if p.unboundCurrent(viewer, at) {
		p.rebase(viewer)
		signer.renewedAt = now
		return
	}
	oldest := now
	for _, target := range p.bound(viewer) {
		if target.signedAt == 0 {
			oldest = min(oldest, target.openedAt)
		} else {
			oldest = min(oldest, target.signedAt)
		}
	}
	covered := check.base.NodeBytes
	for _, point := range check.history {
		if point.AtMS <= oldest && point.AtMS >= check.base.AtMS {
			covered = point.NodeBytes
		}
	}
	point := mc.ReceiptPoint{AtMS: now, ReceiptBytes: signed, NodeBytes: covered}
	if !mc.ReceiptDisagrees(check.base, point) {
		signer.renewedAt = now
	}
	check.disagrees = false
	if now-check.base.AtMS >= mc.ReceiptMismatchWindowMS {
		reference := check.base
		for _, earlier := range check.points {
			if earlier.AtMS <= now-mc.ReceiptMismatchWindowMS {
				reference = earlier
			}
		}
		check.disagrees = mc.ReceiptDisagrees(reference, point) || mc.ReceiptDisagrees(check.base, point)
	}
	check.points = append(check.points, mc.ReceiptPoint{AtMS: now, ReceiptBytes: signed, NodeBytes: bytes})
	for len(check.points) > 2 && check.points[1].AtMS <= now-3*mc.ReceiptMissingAfterMS {
		check.points = check.points[1:]
	}
}

func (p *Players) settleBytes(target *player) {
	if target.key == "" || target.connection == nil {
		return
	}
	check := &target.viewer.bytes
	_, bytes := target.connection.Totals(0)
	check.bytes += bytes
	check.signed += target.signed + bytes - target.answered
	target.signed, target.signedAt, target.answered = 0, 0, 0
	if len(p.bound(target.viewer)) <= 1 {
		*check = byteCheck{}
	}
}

func (p *Players) receiptRefusal(target *player, at int64) string {
	if target.key == "" {
		return ""
	}
	enforced := usage.EnforcesReceipts(p.node.Topology().Usage, TransportHLS)
	if connection := p.live(target, at); connection != nil {
		reason := connection.ReceiptRefusal()
		if reason == "" && p.reporter.MonoMS()-target.renewedAt > mc.ReceiptMissingAfterMS {
			reason = "receipt_missing"
		}
		if reason == "" && target.viewer.bytes.disagrees && !p.unboundCurrent(target.viewer, at) {
			reason = "receipt_mismatch"
		}
		switch {
		case reason != "" && enforced:
			p.end(target)
			target.refused = reason
		case reason != "":
			connection.NoteReceiptRefusal(reason)
		}
	}
	if !enforced {
		target.refused = ""
	}
	return target.refused
}

type minuteLimiter struct {
	mu     sync.Mutex
	window time.Time
	counts map[string]int
	max    int
}

func (l *minuteLimiter) maxKeys() int {
	if l.max > 0 {
		return l.max
	}
	return ratelimit.MaxKeys
}

func (l *minuteLimiter) allow(key string, limit int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts == nil || now.Sub(l.window) >= time.Minute {
		l.window, l.counts = now, map[string]int{}
	}
	if _, known := l.counts[key]; !known && len(l.counts) >= l.maxKeys() {
		for other := range l.counts {
			delete(l.counts, other)
			break
		}
	}
	if l.counts[key] >= limit {
		return false
	}
	l.counts[key]++
	return true
}
