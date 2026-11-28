package sfu

import (
	"slices"
	"strings"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/pion/webrtc/v4"
)

func (s *Session) UpsertRemoteParticipant(info mediacontracts.Participant) {
	if info.Kind == mediacontracts.KindHuman {
		info.Kind = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.participants[info.ParticipantID]; existing != nil {
		if existing.remote && !sameParticipant(existing.info, info) {
			existing.info = info
			s.broadcastLocked(mediacontracts.ParticipantEvent{EventHeader: header("participant_updated"), Participant: info}, nil)
		}
		return
	}
	participant := &Participant{info: info, remote: true, announced: true, whips: map[string]*publisherConn{},
		grants: map[string]grant{}, role: info.Role}
	s.participants[info.ParticipantID] = participant
	if s.identities[info.Identity] == nil {
		s.identities[info.Identity] = participant
	}
	s.broadcastLocked(mediacontracts.ParticipantEvent{EventHeader: header("participant_joined"), Participant: info}, nil)
}

func (s *Session) RemoveRemoteParticipant(participantID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	participant := s.participants[participantID]
	if participant == nil || !participant.remote {
		return
	}
	s.leaveLocked(participant, reason)
}

func (s *Session) UpsertRemoteTrack(info mediacontracts.Track) *Track {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.tracks[info.TrackID]; existing != nil {
		mutedChanged := existing.mirror(info.Muted, info.Layers)
		if mutedChanged {
			s.broadcastLocked(mediacontracts.TrackMutedEvent{EventHeader: header("track_muted"), TrackID: info.TrackID,
				ParticipantID: info.ParticipantID, Muted: info.Muted}, nil)
		}
		if existing.mirrorState(info.State, info.GraceUntilMS) {
			s.broadcastLocked(existing.stateEvent(), nil)
		}
		return existing
	}
	kind := webrtc.RTPCodecTypeVideo
	if info.Kind == "audio" {
		kind = webrtc.RTPCodecTypeAudio
	}
	identity := ""
	if participant := s.participants[info.ParticipantID]; participant != nil {
		identity = participant.info.Identity
	}
	track := &Track{session: s, identity: identity, kind: kind, info: info}
	if len(info.Layers) == 0 {
		track.Layer("")
	}
	for _, layer := range info.Layers {
		track.Layer(layer.RID).spatial = int(min(max(layer.Spatial, 0), 2))
	}
	s.publishLocked(track, nil)
	return track
}

func (s *Session) RemoveTrack(trackID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if track := s.tracks[trackID]; track != nil {
		s.unpublishLocked(track)
	}
}

func (s *Session) TrackInfo(trackID string) (mediacontracts.Track, bool) {
	s.mu.RLock()
	track := s.tracks[trackID]
	s.mu.RUnlock()
	if track == nil {
		return mediacontracts.Track{}, false
	}
	return track.Info(), true
}

func (s *Session) SetTrackState(trackID, state string, graceUntilMS *int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	track := s.tracks[trackID]
	if track == nil {
		return
	}
	if track.mirrorState(state, graceUntilMS) {
		s.broadcastLocked(track.stateEvent(), nil)
	}
}

func (s *Session) SetTrackMuted(trackID string, muted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	track := s.tracks[trackID]
	if track == nil {
		return
	}
	changed := track.setMuted(muted, false)
	if changed {
		s.broadcastLocked(mediacontracts.TrackMutedEvent{EventHeader: header("track_muted"), TrackID: trackID,
			ParticipantID: track.info.ParticipantID, Muted: muted}, nil)
	}
}

func (s *Session) ForwardSpeakers(speakers []mediacontracts.Speaker, dominant *string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if speakers == nil {
		speakers = []mediacontracts.Speaker{}
	}
	s.broadcastLocked(mediacontracts.ActiveSpeakersEvent{EventHeader: header("active_speakers"), Speakers: speakers, DominantID: dominant}, nil)
}

func (s *Session) SetUpstreamListeners(count int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upstreamListeners = min(max(count, 0), mediacontracts.ListenerReportMax)
	s.upstreamKnown = true
	s.listenersDirty = true
}

func (s *Session) RemoteState() (participants, tracks map[string]bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	participants, tracks = map[string]bool{}, map[string]bool{}
	for id, participant := range s.participants {
		if participant.remote {
			participants[id] = true
		}
	}
	for id, track := range s.tracks {
		if participant := s.participants[track.info.ParticipantID]; participant != nil && participant.remote {
			tracks[id] = true
		}
	}
	return participants, tracks
}

func (s *Session) Track(trackID string) *Track {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tracks[trackID]
}

func (t *Track) mutedServer() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.serverMute
}

func lowerMime(mime string) string { return strings.ToLower(mime) }

func codecName(mime string) string {
	_, name, _ := strings.Cut(strings.ToLower(mime), "/")
	if name == "red" {
		return "opus"
	}
	return name
}

func sameParticipant(left, right mediacontracts.Participant) bool {
	return left.ParticipantID == right.ParticipantID && left.Identity == right.Identity && left.Role == right.Role &&
		left.JoinedAtMS == right.JoinedAtMS && sameString(left.Name, right.Name) && sameString(left.Meta, right.Meta) &&
		left.Kind == right.Kind && slices.Equal(left.Activities, right.Activities)
}

func (s *Session) childConnectedLocked(child string) bool {
	for _, conn := range s.subscribers {
		if conn.child == child {
			return true
		}
	}
	return false
}
