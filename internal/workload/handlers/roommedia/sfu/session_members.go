package sfu

import (
	"sort"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

func (s *Session) broadcastLocked(value any, skip *subscriberConn) {
	encoded, err := newEvent(value)
	if err != nil {
		return
	}
	for _, conn := range s.subscribers {
		if conn != skip && conn.ev().isReady() {
			conn.ev().push(encoded)
		}
	}
}

func (s *Session) participantLocked(claims mediacontracts.TokenClaims) (*Participant, bool) {
	if participant := s.identities[claims.Identity]; participant != nil && !participant.remote {
		return participant, false
	}
	participant := &Participant{info: mediacontracts.Participant{ParticipantID: newID("p_"),
		Identity: claims.Identity, Name: claims.Name, Role: claims.Role, Meta: claims.Meta, JoinedAtMS: time.Now().UnixMilli(),
		Kind: participantKind(claims)},
		whips: map[string]*publisherConn{}, grants: map[string]grant{}, role: claims.Role, nonce: claims.Nonce}
	s.participants[participant.info.ParticipantID] = participant
	s.identities[claims.Identity] = participant
	return participant, true
}

func (s *Session) grantLocked(participant *Participant, claims mediacontracts.TokenClaims) {
	participant.grants[claims.Nonce] = grant{role: claims.Role, exp: claims.Expires}
	s.refreshParticipantLocked(participant)
}

func (s *Session) refreshParticipantLocked(participant *Participant) {
	role := mediacontracts.RoleSubscribe
	for _, value := range participant.grants {
		if mediacontracts.RoleIncludes(value.role, role) {
			role = value.role
		}
	}
	publishing := false
	for _, track := range s.tracks {
		if track.info.ParticipantID == participant.info.ParticipantID {
			publishing = true
			break
		}
	}
	roleChanged := role != participant.role
	participant.role = role
	participant.info.Role = role
	if !participant.reported {
		participant.reported = true
		token := participant.nonce
		s.opts.SessionEvent(&mediacontracts.ParticipantJoinedBody{Kind: "participant_joined",
			ParticipantID: participant.info.ParticipantID, Identity: participant.info.Identity, Role: role,
			TokenNonce: &token, Name: participant.info.Name, Meta: participant.info.Meta, ParticipantKind: participant.info.Kind})
	} else if roleChanged {
		s.reportUpdatedLocked(participant)
	}
	announce := role != mediacontracts.RoleSubscribe || publishing || participant.isBot() || len(participant.info.Activities) > 0
	switch {
	case announce && !participant.announced:
		participant.announced = true
		s.broadcastLocked(mediacontracts.ParticipantEvent{EventHeader: header("participant_joined"), Participant: participant.info}, participant.whep)
	case participant.announced && roleChanged:
		s.broadcastLocked(mediacontracts.ParticipantEvent{EventHeader: header("participant_updated"), Participant: participant.info}, nil)
	}
	s.listenersDirty = true
}

func (s *Session) leaveLocked(participant *Participant, reason string) {
	if s.participants[participant.info.ParticipantID] != participant {
		return
	}
	if participant.whep != nil || len(participant.whips) > 0 || participant.graceEnd != nil {
		return
	}
	for _, track := range s.tracks {
		if track.info.ParticipantID == participant.info.ParticipantID {
			s.unpublishLocked(track)
		}
	}
	s.pruneLostLocked(participant)
	delete(s.participants, participant.info.ParticipantID)
	if s.identities[participant.info.Identity] == participant {
		delete(s.identities, participant.info.Identity)
	}
	s.speakers.forget(participant.info.ParticipantID)
	if participant.announced {
		s.broadcastLocked(mediacontracts.ParticipantLeftEvent{EventHeader: header("participant_left"),
			ParticipantID: participant.info.ParticipantID, Reason: reason}, nil)
	}
	if participant.reported && !participant.remote {
		s.opts.SessionEvent(&mediacontracts.ParticipantLeftBody{Kind: "participant_left",
			ParticipantID: participant.info.ParticipantID, Identity: participant.info.Identity, Reason: reason})
	}
	s.listenersDirty = true
}

func (s *Session) publishLocked(track *Track, participant *Participant) {
	s.tracks[track.info.TrackID] = track
	info := track.Info()
	s.broadcastLocked(mediacontracts.TrackPublishedEvent{EventHeader: header("track_published"), Track: info}, nil)
	if s.opts.Role == mediacontracts.RoleSFU && participant != nil && !participant.remote {
		s.opts.SessionEvent(&mediacontracts.TrackPublishedBody{Kind: "track_published", ParticipantID: info.ParticipantID,
			Identity: participant.info.Identity, TrackID: info.TrackID, TrackKind: info.Kind, Source: info.Source, Codec: info.Codec})
	}
	for _, conn := range s.subscribers {
		if conn.auto {
			go conn.autoAssign()
		}
	}
	if participant != nil {
		s.refreshParticipantLocked(participant)
	}
}

func (s *Session) unpublishLocked(track *Track) {
	if s.tracks[track.info.TrackID] != track {
		return
	}
	delete(s.tracks, track.info.TrackID)
	for _, conn := range s.subscribers {
		conn.trackGone(track)
	}
	s.broadcastLocked(mediacontracts.TrackUnpublishedEvent{EventHeader: header("track_unpublished"),
		TrackID: track.info.TrackID, ParticipantID: track.info.ParticipantID}, nil)
	participant := s.participants[track.info.ParticipantID]
	if s.opts.Role == mediacontracts.RoleSFU && participant != nil && !participant.remote {
		s.opts.SessionEvent(&mediacontracts.TrackUnpublishedBody{Kind: "track_unpublished",
			ParticipantID: track.info.ParticipantID, TrackID: track.info.TrackID})
	}
}

func (s *Session) snapshotLocked() ([]mediacontracts.Participant, []mediacontracts.Track) {
	participants := make([]mediacontracts.Participant, 0, len(s.participants))
	for _, participant := range s.participants {
		if participant.announced {
			participants = append(participants, participant.info)
		}
	}
	sort.Slice(participants, func(i, j int) bool { return participants[i].JoinedAtMS < participants[j].JoinedAtMS })
	tracks := make([]mediacontracts.Track, 0, len(s.tracks))
	for _, track := range s.tracks {
		tracks = append(tracks, track.Info())
	}
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].TrackID < tracks[j].TrackID })
	return participants, tracks
}

func (s *Session) listenersLocked() int64 {
	if s.upstreamKnown {
		return s.upstreamListeners
	}
	return s.subtreeListenersLocked()
}

func (s *Session) subtreeListenersLocked() int64 {
	count := int64(0)
	for _, participant := range s.participants {
		if !participant.remote && !participant.announced {
			count++
		}
	}
	for _, reported := range s.childListeners {
		count += reported
	}
	return min(count, mediacontracts.ListenerReportMax)
}

func (s *Session) ListenerReport() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.subtreeListenersLocked()
}

func (s *Session) SetChildListeners(child string, listeners int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.childListeners == nil {
		s.childListeners = map[string]int64{}
	}
	if s.childListeners[child] != listeners {
		s.childListeners[child] = min(max(listeners, 0), mediacontracts.ListenerReportMax)
		s.listenersDirty = true
	}
}

func (s *Session) forgetChildListenersLocked(child string) {
	if _, ok := s.childListeners[child]; ok {
		delete(s.childListeners, child)
		s.listenersDirty = true
	}
}

func (s *Session) IsRelayChild(nodeID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.children[nodeID] || s.view == nil {
		return false
	}
	node := viewNode(s.view, nodeID)
	return node != nil && node.Role == mediacontracts.RoleRelay
}

func (s *Session) welcomeLocked(conn *subscriberConn) {
	participants, tracks := s.snapshotLocked()
	epoch := int64(1)
	if s.view != nil {
		epoch = s.view.Epoch
	}
	firstParticipants, restParticipants := page(participants, mediacontracts.EventSnapshotMaxParticipants)
	firstTracks, restTracks := page(tracks, mediacontracts.EventSnapshotMaxTracks)
	complete := len(restParticipants) == 0 && len(restTracks) == 0
	conn.ev().pushValue(mediacontracts.WelcomeEvent{EventHeader: header("welcome"), ParticipantID: conn.participantID(),
		Identity: conn.claims.Identity, Role: conn.role(), SessionID: s.opts.SessionID, NodeID: s.opts.NodeID,
		TopologyEpoch: epoch, Participants: firstParticipants, Tracks: firstTracks, Listeners: s.listenersLocked(),
		SnapshotComplete: complete})
	for !complete {
		var nextParticipants []mediacontracts.Participant
		var nextTracks []mediacontracts.Track
		nextParticipants, restParticipants = page(restParticipants, mediacontracts.EventSnapshotMaxParticipants)
		nextTracks, restTracks = page(restTracks, mediacontracts.EventSnapshotMaxTracks)
		complete = len(restParticipants) == 0 && len(restTracks) == 0
		conn.ev().pushValue(mediacontracts.SnapshotEvent{EventHeader: header("snapshot"), Participants: nextParticipants,
			Tracks: nextTracks, Complete: complete})
	}
	conn.ev().setReady()
}

func page[T any](values []T, size int) ([]T, []T) {
	if len(values) <= size {
		return append([]T{}, values...), nil
	}
	return append([]T{}, values[:size]...), values[size:]
}
