package roomworkloads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type MediaMessage struct {
	Type        string
	Payload     json.RawMessage
	expires     time.Time
	final       bool
	periodStart time.Time
	describe    string
	panics      int
}

const MediaUsageReportMaxAge = 6 * time.Hour

const (
	mediaControlRetryWindow = 10 * time.Minute
	mediaRetryMaxBackoff    = time.Minute
	mediaSendPanicLimit     = 2
	mediaUpstreamSenders    = 4

	mediaUpstreamMaxBytes       = 256 << 20
	mediaWorkerMaxBytes         = 64 << 20
	mediaLaneMaxMessages        = 1024
	mediaLaneMaxBytes           = 16 << 20
	mediaUsageLaneMaxBytes      = 48 << 20
	mediaUsageLaneSpan          = time.Hour
	mediaUsageLaneMaxMessages   = 240
	mediaUsageReportsPerPeriod  = 64
	mediaUsageSequenceRetention = 8 * time.Hour

	mediaReserveMessages = 16
	mediaReserveBytes    = 2 << 20
)

type MediaRefusal struct {
	Type      string
	Reason    string
	Transient bool
}

func (r *MediaRefusal) Error() string {
	reason := r.Reason
	if reason == "" {
		reason = "not accepted"
	}
	return fmt.Sprintf("BeamCore refused %s: %s", r.Type, reason)
}

func (r *MediaRefusal) RetryLater() bool {
	return r.Transient || mediacontracts.MediaAckRetryable(r.Reason)
}

type mediaLane struct {
	key       string
	workerID  string
	usage     bool
	messages  []MediaMessage
	bytes     int
	notBefore time.Time
	backoff   time.Duration
	busy      bool
}

type usageSequence struct {
	highest  uint64
	periods  map[int64]int
	lastSeen time.Time
}

type mediaUpstream struct {
	mu          sync.Mutex
	lanes       map[string]*mediaLane
	order       []string
	cursor      int
	bytes       int
	workerBytes map[string]int
	sequences   map[string]*usageSequence
	wake        chan struct{}
}

func newMediaUpstream() *mediaUpstream {
	return &mediaUpstream{lanes: map[string]*mediaLane{}, workerBytes: map[string]int{}, sequences: map[string]*usageSequence{},
		wake: make(chan struct{}, 1)}
}

func (u *mediaUpstream) admitUsage(attempt string, sequence uint64, periodStartMS int64, now time.Time) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for key, state := range u.sequences {
		if now.Sub(state.lastSeen) > mediaUsageSequenceRetention {
			delete(u.sequences, key)
		}
	}
	state := u.sequences[attempt]
	if state == nil {
		state = &usageSequence{periods: map[int64]int{}}
		u.sequences[attempt] = state
	}
	if sequence <= state.highest {
		return fmt.Errorf("usage report sequence %d is not above %d", sequence, state.highest)
	}
	if state.periods[periodStartMS] >= mediaUsageReportsPerPeriod {
		return fmt.Errorf("more than %d usage reports for one period", mediaUsageReportsPerPeriod)
	}
	state.highest, state.lastSeen = sequence, now
	state.periods[periodStartMS]++
	if len(state.periods) > 4*mediaUsageReportsPerPeriod {
		for period := range state.periods {
			if period < periodStartMS-int64(mediaUsageSequenceRetention/time.Millisecond) {
				delete(state.periods, period)
			}
		}
	}
	return nil
}

func (u *mediaUpstream) enqueue(laneKey, workerID string, usage bool, message MediaMessage) error {
	size := len(message.Payload)
	u.mu.Lock()
	defer u.mu.Unlock()
	reserveMessages, reserveBytes := mediaReserveMessages, mediaReserveBytes
	if message.final {
		reserveMessages, reserveBytes = 0, 0
	}
	maxMessages, maxBytes := mediaLaneMaxMessages, mediaLaneMaxBytes
	if usage {
		maxMessages, maxBytes = mediaUsageLaneMaxMessages, mediaUsageLaneMaxBytes
	}
	lane := u.lanes[laneKey]
	var refusal string
	switch {
	case lane != nil && len(lane.messages)+reserveMessages >= maxMessages:
		refusal = fmt.Sprintf("lane %q holds %d messages", laneKey, len(lane.messages))
	case lane != nil && lane.bytes+size+reserveBytes > maxBytes:
		refusal = fmt.Sprintf("lane %q holds %d bytes", laneKey, lane.bytes)
	case usage && lane != nil && !message.final && len(lane.messages) > 0 &&
		message.periodStart.Sub(lane.messages[0].periodStart) > mediaUsageLaneSpan:
		refusal = fmt.Sprintf("lane %q holds more than %s of periods", laneKey, mediaUsageLaneSpan)
	case u.workerBytes[workerID]+size+reserveBytes > mediaWorkerMaxBytes:
		refusal = fmt.Sprintf("Worker %s holds %d bytes", workerID, u.workerBytes[workerID])
	case u.bytes+size+reserveBytes > mediaUpstreamMaxBytes:
		refusal = fmt.Sprintf("the upstream holds %d bytes", u.bytes)
	}
	if refusal != "" {
		log.Printf("ERROR media %s lost for the Runtime (%s): %s while the Runtime does not take them", message.Type, message.describe, refusal)
		return fmt.Errorf("media upstream full: %s", refusal)
	}
	if lane == nil {
		lane = &mediaLane{key: laneKey, workerID: workerID, usage: usage}
		u.lanes[laneKey] = lane
		u.order = append(u.order, laneKey)
	}
	lane.messages = append(lane.messages, message)
	lane.bytes += size
	u.bytes += size
	u.workerBytes[workerID] += size
	u.signal()
	return nil
}

func (u *mediaUpstream) pendingEvents(workloadID string) bool {
	prefix := "event\x00" + workloadID + "\x00"
	u.mu.Lock()
	defer u.mu.Unlock()
	for key, lane := range u.lanes {
		if strings.HasPrefix(key, prefix) && len(lane.messages) > 0 {
			return true
		}
	}
	return false
}

func (u *mediaUpstream) signal() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

func (u *mediaUpstream) next(now time.Time) (*mediaLane, MediaMessage, time.Duration, time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var due time.Time
	for range u.order {
		if len(u.order) == 0 {
			break
		}
		u.cursor %= len(u.order)
		lane := u.lanes[u.order[u.cursor]]
		u.cursor++
		if lane == nil || lane.busy || len(lane.messages) == 0 {
			continue
		}
		if now.Before(lane.notBefore) {
			if due.IsZero() || lane.notBefore.Before(due) {
				due = lane.notBefore
			}
			continue
		}
		lane.busy = true
		return lane, lane.messages[0], lane.backoff, time.Time{}
	}
	return nil, MediaMessage{}, 0, due
}

func (u *mediaUpstream) settle(lane *mediaLane, done bool, wait time.Duration, now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	lane.busy = false
	if done {
		size := len(lane.messages[0].Payload)
		lane.messages = lane.messages[1:]
		lane.bytes -= size
		u.bytes -= size
		u.workerBytes[lane.workerID] -= size
		if u.workerBytes[lane.workerID] <= 0 {
			delete(u.workerBytes, lane.workerID)
		}
		lane.backoff, lane.notBefore = 0, time.Time{}
		if len(lane.messages) == 0 {
			delete(u.lanes, lane.key)
			for index, key := range u.order {
				if key == lane.key {
					u.order = append(u.order[:index], u.order[index+1:]...)
					break
				}
			}
		}
	} else {
		lane.notBefore, lane.backoff = now.Add(wait), wait
	}
	u.signal()
}

func (u *mediaUpstream) settlePanicked(lane *mediaLane, backoff time.Duration, now time.Time) (MediaMessage, bool) {
	u.mu.Lock()
	lane.messages[0].panics++
	head := lane.messages[0]
	u.mu.Unlock()
	drop := head.panics >= mediaSendPanicLimit || !now.Add(backoff).Before(head.expires)
	u.settle(lane, drop, backoff, now)
	return head, drop
}

func (u *mediaUpstream) repair() {
	u.mu.Lock()
	defer u.mu.Unlock()
	order := make([]string, 0, len(u.lanes))
	listed := map[string]bool{}
	for _, key := range u.order {
		if u.lanes[key] != nil && !listed[key] {
			order, listed[key] = append(order, key), true
		}
	}
	missing := make([]string, 0)
	for key := range u.lanes {
		if !listed[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	order = append(order, missing...)
	u.bytes, u.workerBytes, u.order, u.cursor = 0, map[string]int{}, order[:0], 0
	for _, key := range order {
		lane := u.lanes[key]
		if len(lane.messages) == 0 && !lane.busy {
			delete(u.lanes, key)
			continue
		}
		lane.bytes = 0
		for _, message := range lane.messages {
			lane.bytes += len(message.Payload)
		}
		u.bytes += lane.bytes
		u.workerBytes[lane.workerID] += lane.bytes
		u.order = append(u.order, key)
	}
	u.signal()
}

func (m *Manager) ForwardMediaUpstream(ctx context.Context, send func(context.Context, string, json.RawMessage) error,
	transient func(error) bool) {
	nodes := m.mediaNodes
	for {
		life := nodes.awaitStart(ctx)
		if life == nil {
			return
		}
		nodes.forwardUpstream(ctx, life, send, transient)
	}
}

func (n *mediaNodes) forwardUpstream(ctx context.Context, life *mediaLife, send func(context.Context, string, json.RawMessage) error,
	transient func(error) bool) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-life.stop:
			cancel()
		case <-runCtx.Done():
		}
	}()
	upstream := n.upstream
	slots := make(chan struct{}, mediaUpstreamSenders)
	var group sync.WaitGroup
	defer group.Wait()
	var held *mediaLane
	defer func() {
		if value := recover(); value != nil {
			if held != nil {
				upstream.settle(held, false, time.Second, n.now())
			}
			n.contain(life, "upstream forwarder", value)
		}
	}()
	for {
		lane, message, lastBackoff, due := upstream.next(n.now())
		if lane == nil {
			var timer <-chan time.Time
			if !due.IsZero() {
				timer = time.After(max(time.Until(due), 10*time.Millisecond))
			}
			select {
			case <-runCtx.Done():
				return
			case <-upstream.wake:
			case <-timer:
			}
			continue
		}
		held = lane
		select {
		case <-runCtx.Done():
			held = nil
			upstream.settle(lane, false, 0, n.now())
			return
		case slots <- struct{}{}:
		}
		group.Add(1)
		held = nil
		go n.sendUpstream(runCtx, life, lane, message, lastBackoff, send, transient, func() {
			<-slots
			group.Done()
		})
	}
}

func upstreamBackoff(lastBackoff time.Duration) time.Duration {
	return min(max(2*lastBackoff, time.Second), mediaRetryMaxBackoff)
}

func (n *mediaNodes) sendUpstream(ctx context.Context, life *mediaLife, lane *mediaLane, message MediaMessage, lastBackoff time.Duration,
	send func(context.Context, string, json.RawMessage) error, transient func(error) bool, done func()) {
	defer done()
	upstream := n.upstream
	settled := false
	settle := func(sent bool, wait time.Duration, now time.Time) {
		settled = true
		upstream.settle(lane, sent, wait, now)
	}
	defer func() {
		if value := recover(); value != nil {
			if !settled {
				settled = true
				head, dropped := upstream.settlePanicked(lane, upstreamBackoff(lastBackoff), n.now())
				if dropped {
					log.Printf("ERROR media %s (%s) lost for the Runtime: its send panicked %d times", head.Type, head.describe, head.panics)
				}
			}
			n.contain(life, "upstream sender", value)
		}
	}()
	err := send(ctx, message.Type, message.Payload)
	now := n.now()
	switch {
	case err == nil:
		settle(true, 0, now)
		return
	case ctx.Err() != nil:
		settle(false, 0, now)
		return
	}
	var refusal *MediaRefusal
	retry := transient(err)
	if errors.As(err, &refusal) {
		retry = refusal.RetryLater()
	}
	backoff := upstreamBackoff(lastBackoff)
	switch {
	case !retry:
		log.Printf("drop media %s (%s) for the Runtime: %v", message.Type, message.describe, err)
		settle(true, 0, now)
	case !now.Add(backoff).Before(message.expires):
		log.Printf("ERROR media %s (%s) lost for the Runtime after retrying past its age limit: %v", message.Type, message.describe, err)
		settle(true, 0, now)
	default:
		settle(false, backoff, now)
	}
}
