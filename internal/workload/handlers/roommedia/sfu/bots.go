package sfu

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/pion/webrtc/v4"
)

func participantKind(claims mediacontracts.TokenClaims) string {
	if mediacontracts.ParticipantKindOf(claims) == mediacontracts.KindBot {
		return mediacontracts.KindBot
	}
	return ""
}

func (p *Participant) isBot() bool { return p.info.Kind == mediacontracts.KindBot }

func (t *Track) isMusic() bool { return t.info.Source == mediacontracts.TrackSourceMusic }

func (s *Session) speechTracksLocked() []*Track {
	audio := make([]*Track, 0, len(s.tracks))
	for _, track := range s.tracks {
		if track.kind == webrtc.RTPCodecTypeAudio && !track.isMusic() {
			audio = append(audio, track)
		}
	}
	return audio
}

func applySourceCodecs(pc *webrtc.PeerConnection, sections []offerSection, sources map[string]string) error {
	for _, transceiver := range pc.GetTransceivers() {
		if transceiver.Kind() != webrtc.RTPCodecTypeAudio {
			continue
		}
		mid := transceiver.Mid()
		if sources[mid] != mediacontracts.TrackSourceMusic {
			continue
		}
		if err := pinOpus(transceiver, sections, mediacontracts.OpusFmtpMusic, false); err != nil {
			return httpError(400, "invalid_request", "music track: "+err.Error())
		}
	}
	return nil
}

func applySlotCodecs(pc *webrtc.PeerConnection, sections []offerSection) error {
	for _, transceiver := range pc.GetTransceivers() {
		if transceiver.Kind() != webrtc.RTPCodecTypeAudio {
			continue
		}
		if err := pinOpus(transceiver, sections, mediacontracts.OpusFmtpWHEPSlot, true); err != nil {
			return httpError(400, "invalid_request", "audio slot: "+err.Error())
		}
	}
	return nil
}

func pinOpus(transceiver *webrtc.RTPTransceiver, sections []offerSection, fmtp string, keepRED bool) error {
	mid := transceiver.Mid()
	index := slices.IndexFunc(sections, func(section offerSection) bool { return section.mid == mid })
	if index < 0 || sections[index].opusPT == 0 {
		return nil
	}
	section := sections[index]
	feedback := []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBTransportCC}}
	codecs := []webrtc.RTPCodecParameters{{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus,
		ClockRate: 48000, Channels: 2, SDPFmtpLine: fmtp, RTCPFeedback: feedback}, PayloadType: webrtc.PayloadType(section.opusPT)}}
	if keepRED && section.redPT != 0 {
		withRED := append(slices.Clone(codecs), webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: mimeTypeRED, ClockRate: 48000, Channels: 2, SDPFmtpLine: section.fmtps[section.redPT], RTCPFeedback: feedback},
			PayloadType: webrtc.PayloadType(section.redPT)})
		if transceiver.SetCodecPreferences(withRED) == nil {
			return nil
		}
	}
	return transceiver.SetCodecPreferences(codecs)
}

func opusPayloadType(rtpmap string) uint8 { return payloadTypeOf(rtpmap, "opus/") }

func payloadTypeOf(rtpmap, codec string) uint8 {
	pt, name, ok := strings.Cut(rtpmap, " ")
	if !ok || !strings.HasPrefix(strings.ToLower(name), codec) {
		return 0
	}
	value, err := strconv.ParseUint(pt, 10, 7)
	if err != nil {
		return 0
	}
	return uint8(value)
}

func (c *subscriberConn) setActivities(command mediacontracts.SetActivitiesCommand) *HTTPError {
	s := c.session
	if s.opts.Role == mediacontracts.RoleRelay {
		return commandError("forbidden", "set_activities is served by the origin sfu, not relay "+s.opts.NodeID)
	}
	if c.participant == nil {
		return commandError("forbidden", "set_activities is for participants")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if command.Activities == nil {
		return commandError("invalid_command", "activities is required")
	}
	switch mediacontracts.SetActivitiesError(c.participant.info.Kind, command.Activities) {
	case "invalid_command":
		return commandError("invalid_command", "activities are at most 4 unique values of recording, transcribing, translating, assisting")
	case "forbidden":
		return commandError("forbidden", "recording and transcribing are for bot participants only")
	}
	now := time.Now()
	if now.Sub(c.participant.activitiesAt) < mediacontracts.SetActivitiesMinIntervalMS*time.Millisecond {
		return commandError("rate_limited", "at most one set_activities a second")
	}
	c.participant.activitiesAt = now
	var activities []string
	if len(command.Activities) > 0 {
		activities = slices.Clone(command.Activities)
	}
	if slices.Equal(c.participant.info.Activities, activities) {
		return nil
	}
	s.setActivitiesLocked(c.participant, activities)
	return nil
}

func (s *Session) setActivitiesLocked(participant *Participant, activities []string) {
	participant.info.Activities = activities
	s.reportUpdatedLocked(participant)
	if participant.announced {
		s.broadcastLocked(mediacontracts.ParticipantEvent{EventHeader: header("participant_updated"), Participant: participant.info}, nil)
		return
	}
	s.refreshParticipantLocked(participant)
	if participant.whep != nil && participant.whep.ev().isReady() {
		participant.whep.ev().pushValue(mediacontracts.ParticipantEvent{EventHeader: header("participant_updated"), Participant: participant.info})
	}
}

func (s *Session) reportUpdatedLocked(participant *Participant) {
	if !participant.reported || participant.remote {
		return
	}
	s.opts.SessionEvent(&mediacontracts.ParticipantUpdatedBody{Kind: "participant_updated", ParticipantID: participant.info.ParticipantID,
		Identity: participant.info.Identity, Role: participant.role, ParticipantKind: participant.info.Kind,
		Activities: slices.Clone(participant.info.Activities)})
}
