package sfu

import (
	"time"

	"github.com/pion/webrtc/v4"
)

var disconnectRecheck = 8 * time.Second

const sweepEvery = 2 * time.Second

func peerEnded(state webrtc.PeerConnectionState) bool {
	return state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed
}

func (s *Session) watchPeer(pc *webrtc.PeerConnection, what string, ended func(webrtc.PeerConnectionState), abort func()) {
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		defer s.guard(what+" state", abort)
		switch {
		case peerEnded(state):
			go func() {
				defer s.guard(what+" lost", nil)
				ended(state)
			}()
		case state == webrtc.PeerConnectionStateDisconnected:
			go func() {
				defer s.guard(what+" disconnect", abort)
				time.Sleep(disconnectRecheck)
				if current := pc.ConnectionState(); current == webrtc.PeerConnectionStateDisconnected || peerEnded(current) {
					ended(current)
				}
			}()
		}
	})
}

func (s *Session) sweepEnded() {
	var lost []func()
	func() {
		s.mu.RLock()
		defer s.mu.RUnlock()
		for _, conn := range s.publishers {
			if !conn.closed.Load() && peerEnded(conn.pc.ConnectionState()) {
				lost = append(lost, conn.lost)
			}
		}
		for _, conn := range s.subscribers {
			if conn.pc == nil || conn.closed.Load() {
				continue
			}
			if state := conn.pc.ConnectionState(); peerEnded(state) {
				lost = append(lost, func() { conn.ended(state) })
			}
		}
	}()
	for _, drop := range lost {
		go func() {
			defer s.guard("sweep lost", nil)
			drop()
		}()
	}
}

func (s *Session) pruneLostLocked(participant *Participant) {
	for id, conn := range s.lostWHIPs {
		if conn.participant != participant {
			continue
		}
		held := false
		for _, track := range conn.tracks {
			if s.tracks[track.info.TrackID] == track && track.detachedFrom(conn) {
				held = true
				break
			}
		}
		if !held {
			delete(s.lostWHIPs, id)
		}
	}
}

func (s *Session) lostWHIP(id string) *publisherConn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lostWHIPs[id]
}

func (s *Session) deleteLost(c *publisherConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lostWHIPs[c.id] != c {
		return false
	}
	delete(s.lostWHIPs, c.id)
	participant := c.participant
	for _, track := range c.tracks {
		if track.detachedFrom(c) {
			s.unpublishLocked(track)
		}
	}
	if !s.grantLiveLocked(participant, c.claims.Nonce) {
		delete(participant.grants, c.claims.Nonce)
	}
	if participant.graceEnd != nil && !s.waitsForPublisherLocked(participant) {
		participant.graceEnd.Stop()
		participant.graceEnd = nil
	}
	if participant.whep == nil && len(participant.whips) == 0 {
		s.leaveLocked(participant, "left")
	} else {
		s.refreshParticipantLocked(participant)
	}
	return true
}

func (s *Session) waitsForPublisherLocked(participant *Participant) bool {
	for _, track := range s.tracks {
		if track.info.ParticipantID == participant.info.ParticipantID && track.waitsForPublisher() {
			return true
		}
	}
	return false
}
