package sfu

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/ratelimit"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

type subscriberConn struct {
	id          string
	session     *Session
	participant *Participant
	child       string
	ownID       string
	pc          *webrtc.PeerConnection
	api         *SubscriberAPI
	claims      mediacontracts.TokenClaims
	usage       *usage.Connection
	eventsPtr   atomic.Pointer[eventChannel]
	auto        bool

	mu        sync.Mutex
	remote    string
	outage    *time.Timer
	suspended atomic.Bool
	patchMu   sync.Mutex
	slots     []*DownTrack
	renegoMu  sync.Mutex
	commands  ratelimit.Limiter
	closed    atomic.Bool
	estimate  atomic.Int64
	loss      atomic.Uint64
	slotCount atomic.Int64
	egress    rateMeter

	allocMu      sync.Mutex
	allocStates  map[*DownTrack]*slotAllocation
	smoothed     float64
	smoothedLoss float64
	smoothedAt   time.Time
	allocLog     struct {
		lastStatus      time.Time
		lastDecision    time.Time
		suppressed      int
		short           bool
		lastProbe       time.Time
		probeSuppressed int
	}
	probe prober
}

func (c *subscriberConn) patching() *sync.Mutex             { return &c.patchMu }
func (c *subscriberConn) peer() *webrtc.PeerConnection      { return c.pc }
func (c *subscriberConn) token() mediacontracts.TokenClaims { return c.claims }
func (c *subscriberConn) remoteSDP() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remote
}
func (c *subscriberConn) setRemoteSDP(value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remote = value
}

func (c *subscriberConn) slotsSnapshot() []*DownTrack {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*DownTrack(nil), c.slots...)
}

func (c *subscriberConn) ev() *eventChannel { return c.eventsPtr.Load() }

func (c *subscriberConn) participantID() string {
	if c.participant != nil {
		return c.participant.info.ParticipantID
	}
	return c.ownID
}

func (c *subscriberConn) role() string {
	if c.participant != nil {
		return c.participant.role
	}
	return mediacontracts.RoleSubscribe
}

func (s *Session) serveWHEP(w http.ResponseWriter, r *http.Request) {
	verifiedMS := s.opts.Usage.NowMS()
	claims, err := s.authorize(r, "whep")
	if err != nil {
		WriteError(w, err)
		return
	}
	offer, err := readSDP(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	sections, err := parseOffer(offer)
	if err != nil {
		WriteError(w, err)
		return
	}
	hasEvents := false
	for _, section := range sections {
		if section.kind == "application" {
			hasEvents = true
		}
	}
	isChild := isNodeIdentity(claims.Identity)
	if mediaSections(sections) > slotLimit(isChild) {
		WriteError(w, httpError(http.StatusBadRequest, "invalid_request", "too many receive slots"))
		return
	}
	if s.opts.Admission != nil && !isChild {
		if err := s.opts.Admission("whep"); err != nil {
			WriteError(w, err)
			return
		}
	}
	e2ee := s.isE2EE()
	api, err := s.opts.Factory.Subscriber(e2ee)
	if err != nil {
		WriteError(w, err)
		return
	}
	pc, err := api.API.NewPeerConnection(s.peerConfiguration())
	if err != nil {
		WriteError(w, err)
		return
	}
	child, _ := strings.CutPrefix(claims.Identity, mediacontracts.NodeIdentityPrefix)
	if !isNodeIdentity(claims.Identity) {
		child = ""
	}
	admission := s.admitted(r, claims, verifiedMS)
	info := usage.ConnectionInfo{ID: admission.id, Proof: admission.proof, ReceiptKey: admission.receiptKey(), Direction: usage.DirectionEgress,
		Transport: usage.TransportWHEP, Layers: true, Detached: true}
	identity := claims.Identity
	info.Identity = &identity
	if child != "" {
		info.Transport, info.Layers = usage.TransportChild, false
	} else {
		nonce, kind := claims.Nonce, mediacontracts.ParticipantKindOf(claims)
		info.TokenNonce, info.Kind = &nonce, &kind
	}
	conn := &subscriberConn{id: newID("r_"), session: s, child: child, pc: pc, api: api, claims: claims, auto: !hasEvents,
		remote: offer, usage: s.opts.Usage.Open(info)}
	if child != "" {
		conn.ownID = newID("p_")
	}
	conn.currentEstimate()
	conn.usage.SetWire(&api.wire)
	if child == "" {
		conn.usage.SetClassify(conn.mediaClass)
	}
	conn.usage.FollowICE(pc, conn.followOutage)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		defer s.guard("whep datachannel", conn.abort)
		if dc.Label() != mediacontracts.EventsChannelLabel {
			return
		}
		conn.attachEvents(dc)
	})
	s.watchPeer(pc, "whep", conn.ended, conn.abort)
	local, err := answer(pc, offer, func() error {
		if err := conn.addSlots(); err != nil {
			return err
		}
		return applySlotCodecs(pc, sections)
	})
	if err != nil {
		conn.usage.Close()
		_ = pc.Close()
		WriteError(w, err)
		return
	}
	conn.mapSlots()
	replaced, accepted := func() (*subscriberConn, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed || !s.accepting {
			return nil, false
		}
		var replaced *subscriberConn
		s.subscribers[conn.id] = conn
		if child == "" {
			participant, _ := s.participantLocked(claims)
			conn.participant = participant
			if participant.whep != nil {
				replaced = participant.whep
			}
			participant.whep = conn
			s.grantLocked(participant, claims)
		}
		return replaced, true
	}()
	if !accepted {
		conn.usage.Close()
		_ = pc.Close()
		WriteError(w, httpError(http.StatusServiceUnavailable, "draining", ""))
		return
	}
	if replaced != nil {
		replaced.close("replaced", "")
	}
	go conn.allocate()
	if conn.auto {
		go conn.autoAssign()
	}
	setReceiptConnection(w, admission)
	writeSDP(w, "/v1/rtc/"+s.opts.SessionID+"/whep/"+conn.id, local)
}

func (c *subscriberConn) addSlots() error {
	var added []*DownTrack
	for _, transceiver := range c.pc.GetTransceivers() {
		kind := transceiver.Kind()
		if kind != webrtc.RTPCodecTypeAudio && kind != webrtc.RTPCodecTypeVideo {
			continue
		}
		if transceiver.Sender() != nil {
			continue
		}
		slot := newDownTrack(c, "slot-"+strconv.FormatInt(c.slotCount.Add(1), 10), kind)
		sender, err := c.pc.AddTrack(slot)
		if err != nil {
			return httpError(http.StatusBadRequest, "invalid_request", "cannot bind receive slot: "+err.Error())
		}
		added = append(added, slot)
		go func(sender *webrtc.RTPSender) {
			defer c.session.guard("whep slot rtcp", c.abort)
			slot.readRTCP(sender)
		}(sender)
	}
	func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.slots = append(c.slots, added...)
	}()
	return nil
}

func (c *subscriberConn) mapSlots() {
	for _, transceiver := range c.pc.GetTransceivers() {
		sender := transceiver.Sender()
		if sender == nil {
			continue
		}
		if slot, ok := sender.Track().(*DownTrack); ok {
			slot.setMID(transceiver.Mid())
		}
	}
}

func (c *subscriberConn) attachEvents(dc *webrtc.DataChannel) {
	channel := newEventChannel(dc, func() { c.close("", "timeout") }, c.session.opts.Logf)
	var once sync.Once
	open := func() {
		once.Do(func() {
			s := c.session
			s.mu.Lock()
			defer s.mu.Unlock()
			c.eventsPtr.Store(channel)
			if !s.closed {
				s.welcomeLocked(c)
			}
		})
	}
	dc.OnOpen(func() {
		defer c.session.guard("whep events open", c.abort)
		open()
	})
	dc.OnMessage(func(message webrtc.DataChannelMessage) {
		defer c.session.guard("whep command", c.abort)
		open()
		if !message.IsString || len(message.Data) > maxCommandBytes {
			channel.pushValue(errorEvent(nil, "invalid_command", "commands are JSON text"))
			return
		}
		c.command(channel, message.Data)
	})
	dc.OnClose(func() { channel.close() })
}

func (c *subscriberConn) sent(now time.Time, bytes, media int) {
	c.usage.AddRTP(bytes, media, 0)
	c.egress.add(now, bytes)
	c.session.egress.add(now, bytes)
}

func (c *subscriberConn) receiverReport(report rtcp.ReceptionReport) {
	loss := int64(report.FractionLost) * 1_000_000 / 256
	var rtt *int64
	if report.LastSenderReport != 0 {
		now := ntpCompact(time.Now())
		value := int64(now-report.LastSenderReport-report.Delay) * 1000 / 65536
		if value >= 0 && value < 60_000 {
			rtt = &value
			c.api.setRTT(time.Duration(value) * time.Millisecond)
		}
	}
	c.usage.SetQuality(&loss, rtt)
}

func ntpCompact(now time.Time) uint32 {
	const ntpEpochOffset = 2_208_988_800
	seconds := uint64(now.Unix()) + ntpEpochOffset
	fraction := uint64(now.Nanosecond()) * (1 << 32) / 1_000_000_000
	return uint32((seconds<<16)&0xffff0000 | (fraction>>16)&0xffff)
}

func (c *subscriberConn) layerChanged(trackID, participantID string, spatial, temporal int, reason string) {
	defer c.session.guard("whep layer event", c.abort)
	if spatial < 0 || c.ev() == nil {
		return
	}
	c.ev().pushValue(mediacontracts.LayerChangedEvent{EventHeader: header("layer_changed"), TrackID: trackID,
		ParticipantID: participantID, Layer: mediacontracts.Layer{Spatial: int64(spatial), Temporal: int64(temporal)}, Reason: reason})
}

func (c *subscriberConn) trackGone(track *Track) {
	slots := c.slotsSnapshot()
	for _, slot := range slots {
		if slot.assigned() == track {
			slot.release()
			if c.ev() != nil {
				c.ev().pushValue(mediacontracts.SlotReleasedEvent{EventHeader: header("slot_released"), MID: slot.MID()})
			}
		}
	}
}

func (c *subscriberConn) autoAssign() {
	defer c.session.guard("whep auto assign", c.abort)
	s := c.session
	tracks := func() []*Track {
		s.mu.RLock()
		defer s.mu.RUnlock()
		tracks := make([]*Track, 0, len(s.tracks))
		for _, track := range s.tracks {
			tracks = append(tracks, track)
		}
		return tracks
	}()
	slots := c.slotsSnapshot()
	used := map[*Track]bool{}
	for _, slot := range slots {
		if track := slot.assigned(); track != nil {
			used[track] = true
		}
	}
	for _, track := range tracks {
		if used[track] {
			continue
		}
		for _, slot := range slots {
			if slot.kind == track.kind && slot.assigned() == nil {
				slot.assign(track, nil, noPrefs())
				used[track] = true
				break
			}
		}
	}
}

func (c *subscriberConn) abort() { c.lost() }

func (c *subscriberConn) lost() { c.close("", "timeout") }

func (c *subscriberConn) ended(state webrtc.PeerConnectionState) {
	if state == webrtc.PeerConnectionStateClosed {
		c.close("", "left")
		return
	}
	c.lost()
}

func (c *subscriberConn) close(connectionReason, leaveReason string) {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	if connectionReason != "" && c.ev() != nil {
		closed := mediacontracts.ConnectionClosedEvent{EventHeader: header("connection_closed"), Reason: connectionReason}
		if c.usage.ReceiptBound() {
			id := c.usage.ID()
			closed.ConnectionID = &id
		}
		c.ev().pushValue(closed)
		c.ev().flushAndClose(250 * time.Millisecond)
	} else {
		c.ev().close()
	}
	_ = c.pc.Close()
	c.usage.Close()
	slots := c.slotsSnapshot()
	for _, slot := range slots {
		slot.release()
	}
	s := c.session
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subscribers, c.id)
	if c.child != "" && !s.childConnectedLocked(c.child) {
		s.forgetChildListenersLocked(c.child)
	}
	participant := c.participant
	if participant == nil {
		return
	}
	if participant.whep == c {
		participant.whep = nil
	}
	if connectionReason == "replaced" {
		return
	}
	if !s.grantLiveLocked(participant, c.claims.Nonce) {
		delete(participant.grants, c.claims.Nonce)
	}
	if participant.whep == nil && len(participant.whips) == 0 {
		if leaveReason == "" {
			leaveReason = "left"
		}
		s.leaveLocked(participant, leaveReason)
	} else {
		s.refreshParticipantLocked(participant)
	}
}
