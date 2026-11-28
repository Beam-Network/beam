package sfu

import (
	"encoding/json"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type commandHead struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

func (c *subscriberConn) command(channel *eventChannel, data []byte) {
	var head commandHead
	if err := json.Unmarshal(data, &head); err != nil || head.Type == "" {
		channel.pushValue(errorEvent(nil, "invalid_command", "malformed command"))
		return
	}
	if len(head.ID) < 1 || len(head.ID) > 64 {
		channel.pushValue(errorEvent(nil, "invalid_command", "id must be 1-64 characters"))
		return
	}
	id := head.ID
	if !c.commands.Allow("commands", time.Now()) {
		channel.pushValue(errorEvent(&id, "rate_limited", ""))
		return
	}
	var err *HTTPError
	switch head.Type {
	case "subscribe":
		var command mediacontracts.SubscribeCommand
		if decodeErr := decodeJSON(data, &command); decodeErr != nil {
			err = commandError("invalid_command", decodeErr.Error())
		} else {
			err = c.subscribe(command)
		}
	case "unsubscribe":
		var command mediacontracts.UnsubscribeCommand
		if decodeErr := decodeJSON(data, &command); decodeErr != nil {
			err = commandError("invalid_command", decodeErr.Error())
		} else {
			err = c.unsubscribe(command.TrackID)
		}
	case "mute":
		var command mediacontracts.MuteCommand
		if decodeErr := decodeJSON(data, &command); decodeErr != nil {
			err = commandError("invalid_command", decodeErr.Error())
		} else {
			err = c.mute(command)
		}
	case "mute_participant":
		var command mediacontracts.MuteParticipantCommand
		if decodeErr := decodeJSON(data, &command); decodeErr != nil {
			err = commandError("invalid_command", decodeErr.Error())
		} else {
			err = c.muteParticipant(command)
		}
	case "kick":
		var command mediacontracts.KickCommand
		if decodeErr := decodeJSON(data, &command); decodeErr != nil {
			err = commandError("invalid_command", decodeErr.Error())
		} else {
			err = c.kick(command)
		}
	case "set_activities":
		var command mediacontracts.SetActivitiesCommand
		if decodeErr := decodeJSON(data, &command); decodeErr != nil {
			err = commandError("invalid_command", decodeErr.Error())
		} else {
			err = c.setActivities(command)
		}
	case "listener_report":
		var command mediacontracts.ListenerReportCommand
		if decodeErr := decodeJSON(data, &command); decodeErr != nil {
			err = commandError("invalid_command", decodeErr.Error())
		} else {
			err = c.listenerReport(command)
		}
	case "receipt":
		var command mediacontracts.ReceiptCommand
		if !c.session.opts.Usage.TakesReceipts() {
			err = commandError("invalid_command", "unknown command "+head.Type)
		} else if decodeErr := decodeJSON(data, &command); decodeErr != nil {
			err = commandError("invalid_command", "malformed receipt command")
		} else {
			err = c.receipt(command)
		}
	case "offer":
		var command mediacontracts.OfferCommand
		if decodeErr := decodeJSON(data, &command); decodeErr != nil {
			err = commandError("invalid_command", decodeErr.Error())
		} else {
			go func() {
				defer c.session.guard("whep renegotiate", c.abort)
				if answerSDP, failure := c.renegotiate(command.SDP); failure != nil {
					channel.pushValue(errorEvent(&id, failure.Code, failure.Detail))
				} else {
					channel.pushValue(mediacontracts.AnswerEvent{EventHeader: header("answer"), ID: id, SDP: answerSDP})
				}
			}()
			return
		}
	default:
		err = commandError("invalid_command", "unknown command "+head.Type)
	}
	if err != nil {
		channel.pushValue(errorEvent(&id, err.Code, err.Detail))
		return
	}
	channel.pushValue(mediacontracts.AckEvent{EventHeader: header("ack"), ID: id})
}

const maxKickReason = 256

func utf16Length(value string) int {
	count := 0
	for _, r := range value {
		count++
		if r > 0xffff {
			count++
		}
	}
	return count
}

func commandError(code, detail string) *HTTPError { return &HTTPError{Code: code, Detail: detail} }

func (c *subscriberConn) listenerReport(command mediacontracts.ListenerReportCommand) *HTTPError {
	if c.child == "" || !c.session.IsRelayChild(c.child) {
		return commandError("forbidden", "listener_report is for relay children only")
	}
	if command.Listeners < 0 || command.Listeners > mediacontracts.ListenerReportMax {
		return commandError("invalid_command", "listeners must be between 0 and 1000000")
	}
	c.session.SetChildListeners(c.child, command.Listeners)
	return nil
}

func (c *subscriberConn) subscribe(command mediacontracts.SubscribeCommand) *HTTPError {
	if command.RID != nil && c.child == "" {
		return commandError("invalid_command", "rid is for relay mode only")
	}
	track := c.session.Track(command.TrackID)
	if track == nil {
		return commandError("not_found", "unknown track")
	}
	slots := c.slotsSnapshot()
	var slot *DownTrack
	if command.MID != nil {
		for _, candidate := range slots {
			if candidate.MID() == *command.MID {
				slot = candidate
				break
			}
		}
		if slot == nil {
			return commandError("no_slot", "unknown mid")
		}
		if slot.kind != track.kind {
			return commandError("no_slot", "the slot is not of the track's kind")
		}
	} else {
		for _, candidate := range slots {
			if candidate.kind == track.kind && candidate.assigned() == nil {
				slot = candidate
				break
			}
		}
		if slot == nil {
			return commandError("no_slot", "no free slot: renegotiate with offer")
		}
	}
	prefs := noPrefs()
	if command.MaxSpatial != nil {
		prefs.maxSpatial = int(min(max(*command.MaxSpatial, 0), 2))
	}
	if command.MaxTemporal != nil {
		prefs.maxTemporal = int(min(max(*command.MaxTemporal, 0), 2))
	}
	if command.Width != nil {
		prefs.width = *command.Width
	}
	if command.Height != nil {
		prefs.height = *command.Height
	}
	for _, other := range slots {
		if other != slot && other.assigned() == track && command.RID == nil {
			other.release()
			c.ev().pushValue(mediacontracts.SlotReleasedEvent{EventHeader: header("slot_released"), MID: other.MID()})
		}
	}
	rid := command.RID
	if c.child != "" && rid == nil {
		single := ""
		rid = &single
	}
	slot.assign(track, rid, prefs)
	info := track.Info()
	c.ev().pushValue(mediacontracts.SlotAssignedEvent{EventHeader: header("slot_assigned"), MID: slot.MID(),
		TrackID: info.TrackID, ParticipantID: info.ParticipantID})
	go func() {
		defer c.session.guard("whep allocate", c.abort)
		c.allocateOnce()
	}()
	return nil
}

func (c *subscriberConn) unsubscribe(trackID string) *HTTPError {
	slots := c.slotsSnapshot()
	found := false
	for _, slot := range slots {
		if track := slot.assigned(); track != nil && track.info.TrackID == trackID {
			slot.release()
			found = true
			c.ev().pushValue(mediacontracts.SlotReleasedEvent{EventHeader: header("slot_released"), MID: slot.MID()})
		}
	}
	if !found {
		return commandError("not_found", "the track is not subscribed")
	}
	return nil
}

func (c *subscriberConn) mute(command mediacontracts.MuteCommand) *HTTPError {
	if c.participant == nil {
		return commandError("forbidden", "mute needs the publish role")
	}
	s := c.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if !mediacontracts.RoleIncludes(c.participant.role, mediacontracts.RolePublish) {
		return commandError("forbidden", "mute needs the publish role")
	}
	track := s.tracks[command.TrackID]
	if track == nil {
		return commandError("not_found", "unknown track")
	}
	if track.info.ParticipantID != c.participant.info.ParticipantID {
		return commandError("forbidden", "not your track")
	}
	if track.mutedServer() && !command.Muted {
		return commandError("forbidden", "muted by an admin")
	}
	s.muteTrackLocked(track, command.Muted, false)
	return nil
}

func (c *subscriberConn) muteParticipant(command mediacontracts.MuteParticipantCommand) *HTTPError {
	if c.participant == nil {
		return commandError("forbidden", "mute_participant needs the admin role")
	}
	s := c.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.participant.role != mediacontracts.RoleAdmin {
		return commandError("forbidden", "mute_participant needs the admin role")
	}
	target := s.participants[command.ParticipantID]
	if target == nil {
		return commandError("not_found", "unknown participant")
	}
	if target.remote {
		return commandError("forbidden", "the participant is served by another node")
	}
	found := false
	for _, track := range s.tracks {
		if track.info.ParticipantID != target.info.ParticipantID {
			continue
		}
		if command.TrackID != nil && track.info.TrackID != *command.TrackID {
			continue
		}
		found = true
		s.muteTrackLocked(track, true, true)
	}
	if !found {
		return commandError("not_found", "no such track")
	}
	return nil
}

func (c *subscriberConn) kick(command mediacontracts.KickCommand) *HTTPError {
	if command.Reason != nil && utf16Length(*command.Reason) > maxKickReason {
		return commandError("invalid_command", "reason is at most 256 characters")
	}
	if c.participant == nil {
		return commandError("forbidden", "kick needs the admin role")
	}
	role, identity := func() (string, string) {
		c.session.mu.RLock()
		defer c.session.mu.RUnlock()
		return c.participant.role, c.participant.info.Identity
	}()
	if role != mediacontracts.RoleAdmin {
		return commandError("forbidden", "kick needs the admin role")
	}
	if !c.session.Kick(command.ParticipantID, identity, command.Reason) {
		return commandError("not_found", "unknown participant")
	}
	return nil
}

func (s *Session) Kick(participantID, byIdentity string, reason *string) bool {
	target, whep, whips := s.kickLocked(participantID, byIdentity, reason)
	if target == nil {
		return false
	}
	if target.remote {
		return true
	}
	for _, conn := range whips {
		conn.close("kicked", "kicked")
	}
	if whep != nil {
		whep.close("kicked", "kicked")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaveLocked(target, "kicked")
	return true
}

func (s *Session) kickLocked(participantID, byIdentity string, reason *string) (*Participant, *subscriberConn, []*publisherConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.participants[participantID]
	if target == nil {
		return nil, nil, nil
	}
	expires := time.Now().Unix() + mediacontracts.TokenMaxLifetimeSeconds
	if !target.remote {
		expires = 0
		for _, value := range target.grants {
			expires = max(expires, value.exp)
		}
	}
	s.opts.SessionEvent(&mediacontracts.KickBody{Kind: "kick", ParticipantID: target.info.ParticipantID,
		Identity: target.info.Identity, ByIdentity: byIdentity, TokenExp: expires, Reason: reason})
	if target.remote {
		return target, nil, nil
	}
	whep := target.whep
	whips := make([]*publisherConn, 0, len(target.whips))
	for _, conn := range target.whips {
		whips = append(whips, conn)
	}
	if target.graceEnd != nil {
		target.graceEnd.Stop()
		target.graceEnd = nil
	}
	text := "kicked"
	if reason != nil && *reason != "" {
		text = *reason
	}
	if whep != nil && whep.ev() != nil {
		whep.ev().pushValue(mediacontracts.KickedEvent{EventHeader: header("kicked"), Reason: text})
	}
	return target, whep, whips
}

func (c *subscriberConn) renegotiate(offer string) (string, *HTTPError) {
	c.renegoMu.Lock()
	defer c.renegoMu.Unlock()
	if len(offer) > mediacontracts.EventMessageMaxBytes/4 {
		return "", commandError("renegotiation_failed", "offer too large")
	}
	sections, err := parseOffer(offer)
	if err != nil {
		return "", commandError("renegotiation_failed", "malformed SDP")
	}
	if mediaSections(sections) > slotLimit(c.child != "") {
		return "", commandError("renegotiation_failed", "too many receive slots")
	}
	local, answerErr := answer(c.pc, offer, func() error {
		if err := c.addSlots(); err != nil {
			return err
		}
		return applySlotCodecs(c.pc, sections)
	})
	if answerErr != nil {
		return "", commandError("renegotiation_failed", answerErr.Error())
	}
	c.setRemoteSDP(offer)
	c.mapSlots()
	if c.auto {
		go c.autoAssign()
	}
	return local, nil
}

func slotLimit(child bool) int {
	if child {
		return maxChildSlots
	}
	return maxSlots
}
