package usage

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const (
	hourMS      = int64(time.Hour / time.Millisecond)
	maxHourCuts = 24
)

func (r *Reporter) Run(ctx context.Context) {
	r.mu.Lock()
	r.jobs = newJobQueue()
	jobs := r.jobs
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.jobs = nil
		r.mu.Unlock()
		jobs.close()
	}()
	sample := time.NewTicker(sampleInterval)
	defer sample.Stop()
	for {
		var hour <-chan time.Time
		var timer *time.Timer
		if r.V2() {
			now := r.config.Now()
			timer = time.NewTimer(now.Truncate(time.Hour).Add(time.Hour).Sub(now))
			hour = timer.C
		}
		select {
		case <-ctx.Done():
			r.Flush()
			return
		case <-sample.C:
			r.tick()
		case <-hour:
			r.tick()
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (r *Reporter) tick() {
	at, mono := r.NowMS(), r.monoMS()
	connections := r.snapshot()
	r.mu.Lock()
	last := r.lastTick
	r.lastTick = at
	r.mu.Unlock()
	ahead := r.ahead()
	switch {
	case last == 0:
	case at < last:
		r.stepped(connections, last, at)
		last = at
	case ahead >= mediacontracts.UsageOutageMinMS:
		for _, connection := range connections {
			connection.skip(last, min(last+ahead, at))
		}
	case ahead > 0:
		for _, connection := range connections {
			connection.deduct(ahead, last, at)
		}
	}
	r.mu.Lock()
	start := r.periodStart
	r.mu.Unlock()
	r.cutHours(connections, max(start, last), at)
	r.sample(connections, at)
	r.mu.Lock()
	start = r.periodStart
	r.mu.Unlock()
	if r.due(mono) && at-start >= min(1000, r.config.Period.Milliseconds()/2) {
		r.report(at)
	}
}

func (r *Reporter) due(mono int64) bool {
	period := r.config.Period.Milliseconds()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.V2() {
		r.nextDue = r.periodMono + period
		return mono-r.periodMono >= period
	}
	if mono < r.nextDue {
		return false
	}
	for r.nextDue <= mono {
		r.nextDue += period
	}
	return true
}

func (r *Reporter) Flush() {
	at := r.NowMS()
	connections := r.snapshot()
	if r.V2() {
		r.mu.Lock()
		if at == r.periodStart {
			at++
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	from := max(r.periodStart, r.lastTick)
	r.mu.Unlock()
	r.cutHours(connections, from, at)
	r.sample(connections, at)
	for _, connection := range connections {
		connection.stop(at)
	}
	r.report(at)
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

func (r *Reporter) cutHours(connections []*Connection, from, at int64) {
	if !r.V2() {
		return
	}
	for hour := max((from/hourMS+1)*hourMS, (at/hourMS-maxHourCuts+1)*hourMS); hour <= at; hour += hourMS {
		sampleV2(connections, hour)
		r.report(hour)
	}
}

func (r *Reporter) snapshot() []*Connection {
	r.mu.Lock()
	defer r.mu.Unlock()
	connections := make([]*Connection, 0, len(r.connections))
	for _, connection := range r.connections {
		connections = append(connections, connection)
	}
	return connections
}

func (r *Reporter) sample(connections []*Connection, at int64) {
	now := r.config.Now()
	for _, connection := range connections {
		connection.mu.Lock()
		connection.sampleLocked(now)
		connection.sampleV2(at)
		connection.mu.Unlock()
	}
}

func sampleV2(connections []*Connection, at int64) {
	for _, connection := range connections {
		connection.mu.Lock()
		connection.sampleV2(at)
		connection.mu.Unlock()
	}
}

func (r *Reporter) report(end int64) {
	r.mu.Lock()
	if r.closed || end <= r.periodStart {
		r.mu.Unlock()
		return
	}
	start := r.periodStart
	r.periodStart, r.periodMono = end, r.monoMS()
	var build func() []outgoing
	lost := func() {}
	if r.V2() && start/hourMS == (end-1)/hourMS {
		rows, owners, renditions := r.takeV2(start, end)
		build = func() []outgoing { return r.buildV2(start, end, rows, owners, renditions) }
		lost = func() {
			for index, row := range rows {
				if row.Proof != nil {
					owners[index].keepProof(row.Proof)
				}
			}
		}
	} else {
		rows, connected := r.takeV1(start, end)
		build = func() []outgoing { return r.buildV1(start, end, rows, connected) }
	}
	jobs := r.jobs
	r.mu.Unlock()
	send := func() { r.send(end, build) }
	if jobs == nil {
		send()
	} else if !jobs.add(send) {
		log.Printf("ERROR media %s: a usage period of %d ms is lost: %d wait for the disk", r.config.NodeID, end-start, jobsMax)
		lost()
	}
}

func (r *Reporter) send(end int64, build func() []outgoing) {
	r.reportMu.Lock()
	defer r.reportMu.Unlock()
	built := build()
	if r.config.Sequences != nil && len(built) > 0 {
		_ = r.config.Sequences.Save(r.sequence)
		if periods, ok := r.config.Sequences.(PeriodStore); ok {
			_ = periods.SavePeriodEnd(end)
		}
	}
	if r.config.Sink == nil {
		return
	}
	for _, item := range built {
		if err := r.config.Sink(item.message); err != nil {
			for connection, proof := range item.proofs {
				connection.keepProof(proof)
			}
		}
	}
}

const jobsMax = 60

type jobQueue struct {
	mu     sync.Mutex
	wake   *sync.Cond
	jobs   []func()
	closed bool
	done   chan struct{}
}

func newJobQueue() *jobQueue {
	q := &jobQueue{done: make(chan struct{})}
	q.wake = sync.NewCond(&q.mu)
	go q.run()
	return q
}

func (q *jobQueue) add(job func()) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || len(q.jobs) >= jobsMax {
		return false
	}
	q.jobs = append(q.jobs, job)
	q.wake.Signal()
	return true
}

func (q *jobQueue) run() {
	defer close(q.done)
	for {
		q.mu.Lock()
		for len(q.jobs) == 0 && !q.closed {
			q.wake.Wait()
		}
		if len(q.jobs) == 0 {
			q.mu.Unlock()
			return
		}
		job := q.jobs[0]
		q.jobs = q.jobs[1:]
		q.mu.Unlock()
		job()
	}
}

func (q *jobQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.wake.Signal()
	q.mu.Unlock()
	<-q.done
}

type outgoing struct {
	message mediacontracts.UsageReportMessage
	proofs  map[*Connection]*mediacontracts.UsageProof
}

func (r *Reporter) message(schema string, payload []byte) mediacontracts.UsageReportMessage {
	return mediacontracts.UsageReportMessage{Type: mediacontracts.UsageReportType, SchemaVersion: schema,
		WorkloadID: r.config.WorkloadID, NodeID: r.config.NodeID, Report: r.Sign(schema, payload)}
}

func (r *Reporter) sorted() []*Connection {
	connections := make([]*Connection, 0, len(r.connections))
	for _, connection := range r.connections {
		connections = append(connections, connection)
	}
	sort.Slice(connections, func(i, j int) bool { return connections[i].id < connections[j].id })
	return connections
}

func (r *Reporter) takeV1(start, end int64) ([]mediacontracts.UsageConnection, []int64) {
	rows := make([]mediacontracts.UsageConnection, 0, len(r.connections))
	connected := make([]int64, 0, len(r.connections))
	for _, connection := range r.sorted() {
		connection.mu.Lock()
		connection.dropPeriod(end)
		ended := !connection.closedAt.IsZero()
		if connection.info.Versions != OnlyV2 {
			row := mediacontracts.UsageConnection{ConnectionID: connection.id, TokenNonce: connection.info.TokenNonce,
				Identity: connection.info.Identity, Direction: connection.info.Direction, Transport: connection.info.Transport,
				Bytes: connection.periodBytes, MediaMS: max(min(connection.mediaMS, end-start), 0),
				LossPPM: connection.lossPPM, RTTMS: connection.rttMS}
			if connection.info.Layers {
				layers := connection.layersMS
				row.LayersMS = &layers
			}
			rows, connected = append(rows, row), append(connected, connection.connectionMS)
		}
		connection.dropV1()
		connection.mu.Unlock()
		if ended {
			delete(r.connections, connection.id)
		}
	}
	for _, rendition := range r.renditions {
		rendition.us = 0
	}
	return rows, connected
}

func (r *Reporter) buildV1(start, end int64, rows []mediacontracts.UsageConnection, connected []int64) []outgoing {
	var built []outgoing
	for _, connections := range chunk(rows, connected) {
		r.sequence++
		report := mediacontracts.UsageReport{SchemaVersion: mediacontracts.UsageReportSchema, SessionID: r.config.SessionID,
			NodeID: r.config.NodeID, WorkerID: r.config.WorkerID, Role: r.config.Role, TopologyEpoch: max(r.config.Epoch(), 1),
			Sequence: r.sequence, PeriodStartMS: start, PeriodEndMS: end, Connections: connections.rows, Totals: connections.totals}
		payload, err := mediacontracts.MarshalWire(report)
		if err != nil {
			r.sequence--
			continue
		}
		built = append(built, outgoing{message: r.message(mediacontracts.UsageReportSchema, payload)})
	}
	return built
}

func (c *Connection) dropV1() {
	c.periodBytes, c.mediaMS, c.connectionMS = 0, 0, 0
	c.layersMS = mediacontracts.LayersMS{}
}

type reportChunk struct {
	rows   []mediacontracts.UsageConnection
	totals mediacontracts.UsageTotals
}

func chunk(rows []mediacontracts.UsageConnection, connected []int64) []reportChunk {
	var result []reportChunk
	for _, part := range split(rows, 1<<10) {
		current := reportChunk{rows: rows[part[0]:part[1]]}
		for index := part[0]; index < part[1]; index++ {
			if rows[index].Direction == DirectionIngress {
				current.totals.IngressBytes += rows[index].Bytes
			} else {
				current.totals.EgressBytes += rows[index].Bytes
			}
			current.totals.ConnectionMS += connected[index]
		}
		result = append(result, current)
	}
	return result
}

func split[T any](rows []T, reportOverhead int) [][2]int {
	var parts [][2]int
	start, size := 0, reportOverhead
	for index := range rows {
		rowSize := 512
		if encoded, err := mediacontracts.MarshalWire(rows[index]); err == nil {
			rowSize = len(encoded) + 1
		}
		if index > start && (index-start >= MaxConnectionsPerReport || size+rowSize > maxReportPayloadBytes) {
			parts = append(parts, [2]int{start, index})
			start, size = index, reportOverhead
		}
		size += rowSize
	}
	return append(parts, [2]int{start, len(rows)})
}

func (r *Reporter) takeV2(start, end int64) ([]mediacontracts.UsageConnectionV2, []*Connection, *[]mediacontracts.UsageRendition) {
	rows := make([]mediacontracts.UsageConnectionV2, 0, len(r.connections))
	var owners []*Connection
	for _, connection := range r.sorted() {
		connection.mu.Lock()
		row, present, over := connection.row(start, end)
		connection.dropV1()
		connection.mu.Unlock()
		if present {
			rows, owners = append(rows, row), append(owners, connection)
		}
		if over {
			delete(r.connections, connection.id)
		}
	}
	return rows, owners, r.takeRenditions()
}

func (r *Reporter) buildV2(start, end int64, rows []mediacontracts.UsageConnectionV2, owners []*Connection,
	renditions *[]mediacontracts.UsageRendition) []outgoing {
	valid, kept := rows[:0], owners[:0]
	for index, row := range rows {
		if r.accepted(row, start, end) {
			valid, kept = append(valid, row), append(kept, owners[index])
		}
	}
	rows, owners = valid, kept
	var built []outgoing
	for _, part := range split(rows, 4<<10) {
		report := mediacontracts.UsageReportV2{SchemaVersion: mediacontracts.UsageReportV2Schema, SessionID: r.config.SessionID,
			NodeID: r.config.NodeID, WorkerID: r.config.WorkerID, Role: r.config.Role, TopologyEpoch: max(r.config.Epoch(), 1),
			Sequence: r.sequence + 1, PeriodStartMS: start, PeriodEndMS: end,
			Connections: append([]mediacontracts.UsageConnectionV2{}, rows[part[0]:part[1]]...), Renditions: renditions}
		payload, ok := r.encodeV2(&report)
		if !ok {
			continue
		}
		r.sequence++
		if renditions != nil {
			renditions = &[]mediacontracts.UsageRendition{}
		}
		proofs := map[*Connection]*mediacontracts.UsageProof{}
		for index := part[0]; index < part[1]; index++ {
			if rows[index].Proof != nil {
				proofs[owners[index]] = rows[index].Proof
			}
		}
		built = append(built, outgoing{message: r.message(mediacontracts.UsageReportV2Schema, payload), proofs: proofs})
	}
	return built
}

func (r *Reporter) encodeV2(report *mediacontracts.UsageReportV2) ([]byte, bool) {
	report.Totals = mediacontracts.TotalsOf(report.Connections)
	payload, err := mediacontracts.MarshalWire(*report)
	if err == nil {
		_, err = mediacontracts.ParseUsageReportV2(payload)
	}
	if err != nil {
		r.refused.Add(1)
		log.Printf("ERROR media %s: usage report %d not sent: %v", r.config.NodeID, report.Sequence, err)
		return nil, false
	}
	return payload, true
}

func (r *Reporter) accepted(row mediacontracts.UsageConnectionV2, start, end int64) bool {
	report := mediacontracts.UsageReportV2{SchemaVersion: mediacontracts.UsageReportV2Schema, SessionID: r.config.SessionID,
		NodeID: r.config.NodeID, WorkerID: r.config.WorkerID, Role: r.config.Role, TopologyEpoch: 1, Sequence: 1, PeriodStartMS: start,
		PeriodEndMS: end, Connections: []mediacontracts.UsageConnectionV2{row}}
	report.Totals = mediacontracts.TotalsOf(report.Connections)
	if r.withRenditions() {
		report.Renditions = &[]mediacontracts.UsageRendition{}
	}
	err := mediacontracts.ValidateUsageReportV2(report)
	var issue *mediacontracts.UsageIssue
	if !errors.As(err, &issue) || !strings.HasPrefix(issue.Path, "connections.") {
		return true
	}
	r.refused.Add(1)
	log.Printf("ERROR media %s: a row left out of a usage report: %v", r.config.NodeID, err)
	return false
}

func (r *Reporter) withRenditions() bool {
	return r.config.Role == mediacontracts.RoleTranscode || r.config.Role == mediacontracts.RolePackage
}

func (r *Reporter) Refused() int64 { return r.refused.Load() }

type rendition struct {
	class string
	us    int64
}

func (r *Reporter) AddRendition(id, class string, duration time.Duration) {
	if r == nil || class == "" || duration <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.renditions[id]
	if entry == nil {
		if len(r.renditions) >= mediacontracts.UsageRenditionsMax {
			return
		}
		entry = &rendition{}
		r.renditions[id] = entry
	}
	entry.class = class
	entry.us += duration.Microseconds()
}

func (r *Reporter) takeRenditions() *[]mediacontracts.UsageRendition {
	if !r.withRenditions() {
		return nil
	}
	list := []mediacontracts.UsageRendition{}
	for id, entry := range r.renditions {
		if ms := entry.us / 1000; ms > 0 {
			list = append(list, mediacontracts.UsageRendition{RenditionID: id, Class: entry.class, MediaMS: ms})
			entry.us -= ms * 1000
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].RenditionID < list[j].RenditionID })
	return &list
}
