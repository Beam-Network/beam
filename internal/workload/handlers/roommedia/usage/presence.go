package usage

import (
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type presence interface {
	total(at int64) int64
	cut(at int64) []mediacontracts.UsageInterval
	end(at int64)
	hole(from, to int64)
	deduct(ms, last, at int64)
	short() int64
	rebase(last, at, floor int64)
	setFloor(at int64)
	more() bool
	started() (int64, bool)
}

const none = int64(-1)

type intervals struct {
	closed []mediacontracts.UsageInterval
	past   int64
	first  int64
	begun  bool
	floor  int64
	taken  int64
}

func (i *intervals) short() int64 {
	taken := i.taken
	i.taken = 0
	return taken
}

func (i *intervals) takeOff(ms int64) {
	if ms > 0 {
		i.past -= ms
		i.taken += ms
	}
}

func (i *intervals) subtract(from, to int64) {
	var kept []mediacontracts.UsageInterval
	for _, interval := range i.closed {
		if interval[0] < from && interval[1] > from {
			kept = append(kept, mediacontracts.UsageInterval{interval[0], from})
		}
		if interval[1] > to && interval[0] < to {
			kept = append(kept, mediacontracts.UsageInterval{to, interval[1]})
		}
		if interval[1] <= from || interval[0] >= to {
			kept = append(kept, interval)
		}
	}
	i.closed = kept
}

func (i *intervals) setFloor(at int64) { i.floor = at }

func (i *intervals) start(at int64) {
	if !i.begun {
		i.begun, i.first = true, at
	}
}

func (i *intervals) started() (int64, bool) { return i.first, i.begun }

func (i *intervals) more() bool { return len(i.closed) > 0 }

func (i *intervals) add(from, to int64) {
	if to > from {
		i.closed = append(i.closed, mediacontracts.UsageInterval{from, to})
	}
}

func (i *intervals) before(at int64) int64 {
	total := i.past
	for _, interval := range i.closed {
		total += max(min(interval[1], at)-interval[0], 0)
	}
	return total
}

func (i *intervals) clamp(at, floor int64) {
	kept := i.closed[:0]
	for _, interval := range i.closed {
		if interval[0] < at {
			kept = append(kept, mediacontracts.UsageInterval{interval[0], min(interval[1], at)})
		}
	}
	i.closed, i.floor = kept, min(i.floor, floor)
}

func (i *intervals) take(at int64) []mediacontracts.UsageInterval {
	var taken, kept []mediacontracts.UsageInterval
	for _, interval := range i.closed {
		if interval[0] < at {
			part := mediacontracts.UsageInterval{interval[0], min(interval[1], at)}
			taken = append(taken, part)
			i.past += part[1] - part[0]
		}
		if interval[1] > at {
			kept = append(kept, mediacontracts.UsageInterval{max(interval[0], at), interval[1]})
		}
	}
	i.closed = kept
	return taken
}

type transport struct {
	intervals
	open int64
	lost int64
	over bool
}

func newTransport() *transport { return &transport{open: none, lost: none} }

func (t *transport) connect(at int64) {
	if t.over {
		return
	}
	at = max(at, t.floor)
	t.start(at)
	switch {
	case t.lost == none && t.open == none:
		t.open = at
	case t.lost != none && at-t.lost < mediacontracts.UsageOutageMinMS:
		if t.open == none {
			t.open = max(t.floor, t.lost)
		}
	case t.lost != none:
		if t.open != none {
			t.add(t.open, t.lost)
		}
		t.open = at
	}
	t.lost = none
}

func (t *transport) lose(at int64) bool {
	if t.over || t.open == none || t.lost != none {
		return false
	}
	t.lost = max(at, t.open)
	return true
}

func (t *transport) sent(from, to int64) {
	if t.over || t.lost == none || to <= t.lost {
		return
	}
	from = max(from, t.lost, t.floor)
	if from-t.lost < mediacontracts.UsageOutageMinMS {
		if t.open == none {
			t.open = max(t.floor, t.lost)
		}
	} else {
		if t.open != none {
			t.add(t.open, t.lost)
		}
		t.open = from
	}
	t.lost = max(to, t.open)
}

func (t *transport) rebase(last, at, floor int64) {
	total := t.total(last)
	t.clamp(at, floor)
	if t.open != none {
		t.open = min(t.open, at)
	}
	if t.lost != none {
		t.lost = at - max(last-t.lost, 0)
		if t.open != none {
			t.lost = max(t.lost, t.open)
		}
	}
	t.past += total - t.total(at)
}

func (t *transport) hole(from, to int64) {
	t.subtract(from, to)
	if t.over || t.open == none || t.lost != none && t.lost <= from {
		return
	}
	if t.open < from {
		t.add(t.open, from)
	}
	t.open = max(t.open, to)
	if t.lost != none {
		t.lost = max(t.lost, t.open)
	}
}

func (t *transport) deduct(ms, last, at int64) {
	t.takeOff(min(ms, t.total(at)-t.total(last)))
}

func (t *transport) end(at int64) {
	if t.over {
		return
	}
	t.over = true
	if t.open != none {
		if t.lost != none {
			at = t.lost
		}
		t.add(t.open, at)
	}
	t.open, t.lost = none, none
}

func (t *transport) total(at int64) int64 {
	total := t.before(at)
	if t.open != none {
		end := at
		if t.lost != none {
			end = min(at, t.lost)
		}
		total += max(end-t.open, 0)
	}
	return total
}

func (t *transport) cut(at int64) []mediacontracts.UsageInterval {
	taken := t.take(at)
	if t.open != none && t.open < at {
		end := at
		if t.lost != none && t.lost < at {
			end = t.lost
		}
		if end > t.open {
			taken = append(taken, mediacontracts.UsageInterval{t.open, end})
			t.past += end - t.open
		}
		if end < at {
			t.open = none
		} else {
			t.open = at
		}
	}
	t.floor = at
	return taken
}

type requests struct {
	intervals
	target   int64
	open     int64
	reached  int64
	inflight int
	over     bool
	skipped  int64
}

func newRequests(targetMS int64) *requests { return &requests{target: targetMS, open: none} }

func (r *requests) expired(at int64) bool {
	return r.begun && r.inflight == 0 && at > r.reached+3*r.target
}

func (r *requests) begin(at int64) {
	if r.over {
		return
	}
	at = max(at, r.floor)
	r.start(at)
	if r.open == none {
		r.open = at
	} else {
		r.takeOff(r.skipped)
	}
	r.skipped = 0
	r.reached = max(r.reached, at)
	r.inflight++
}

func (r *requests) finish(at int64) {
	if r.inflight == 0 {
		return
	}
	r.inflight--
	if r.over {
		return
	}
	r.reached = max(r.reached, at)
}

func (r *requests) hole(from, to int64) {
	r.subtract(from, to)
	if r.over || r.open == none {
		r.reached += to - from
		return
	}
	if known := r.known(from); known < from {
		r.add(r.open, known)
		r.open = none
	} else {
		if r.open < from {
			r.add(r.open, from)
		}
		r.open = max(r.open, to)
	}
	if r.reached <= from {
		r.reached += to - from
	}
}

func (r *requests) deduct(ms, last, at int64) {
	if !r.over && r.open != none && r.inflight == 0 {
		r.skipped += ms
		return
	}
	r.takeOff(min(ms, r.total(at)-r.total(last)))
}

func (r *requests) rebase(last, at, floor int64) {
	total := r.total(last)
	r.clamp(at, floor)
	if r.open != none {
		r.open = min(r.open, at)
	}
	r.reached = at - max(last-r.reached, 0)
	r.past += total - r.total(at)
}

func (r *requests) known(at int64) int64 {
	if r.inflight > 0 {
		return at
	}
	return min(at, r.reached+r.target)
}

func (r *requests) end(at int64) {
	if r.over {
		return
	}
	if r.open != none {
		r.add(r.open, r.known(at))
	}
	r.over, r.open = true, none
}

func (r *requests) total(at int64) int64 {
	total := r.before(at)
	if r.open != none {
		total += max(r.known(at)-r.open, 0)
	}
	return total
}

func (r *requests) cut(at int64) []mediacontracts.UsageInterval {
	taken := r.take(at)
	if r.open != none && r.open < at {
		end := r.known(at)
		if end > r.open {
			taken = append(taken, mediacontracts.UsageInterval{r.open, end})
			r.past += end - r.open
		}
		if end < at {
			r.open = none
		} else {
			r.open = at
		}
	}
	r.floor = at
	return taken
}
