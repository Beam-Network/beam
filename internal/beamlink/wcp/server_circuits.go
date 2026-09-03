package wcp

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
)

// UpsertCircuit durably authorizes and distributes a direct Worker-to-Worker
// data-plane circuit. The Orchestrator never proxies the data carried by the circuit.
func (s *Server) UpsertCircuit(_ context.Context, plan circuit.Plan) error {
	now := time.Now().UTC()
	s.mu.RLock()
	current, hasCurrent := s.circuits[plan.CircuitID]
	s.mu.RUnlock()
	if plan.IssuedAt.IsZero() {
		if hasCurrent && current.PlanVersion == plan.PlanVersion {
			plan.IssuedAt = current.IssuedAt
		} else {
			plan.IssuedAt = now
		}
	}
	if plan.Token == "" {
		if hasCurrent && current.PlanVersion == plan.PlanVersion {
			plan.Token = current.Token
		} else {
			token, err := circuit.NewToken()
			if err != nil {
				return err
			}
			plan.Token = token
		}
	}
	if err := plan.Validate(now); err != nil {
		return err
	}

	s.mu.RLock()
	sessions := make(map[string]*Session, len(plan.Peers))
	for _, peer := range plan.Peers {
		session := s.sessions[peer.WorkerID]
		if session == nil {
			s.mu.RUnlock()
			return fmt.Errorf("worker %s has no active WCP session", peer.WorkerID)
		}
		sessions[peer.WorkerID] = session
	}
	s.mu.RUnlock()
	for _, peer := range plan.Peers {
		if err := s.validateCircuitPeer(peer, now); err != nil {
			return err
		}
	}

	s.mu.Lock()
	if revoked, ok := s.revoked[plan.CircuitID]; ok && plan.PlanVersion <= revoked.Revocation.PlanVersion {
		s.mu.Unlock()
		return errors.New("circuit plan version does not supersede its revocation")
	}
	if current, ok := s.circuits[plan.CircuitID]; ok {
		if plan.PlanVersion < current.PlanVersion {
			s.mu.Unlock()
			return errors.New("circuit plan version rolled back")
		}
		if plan.PlanVersion == current.PlanVersion && !reflect.DeepEqual(plan, current) {
			s.mu.Unlock()
			return errors.New("circuit plan version conflicts with current state")
		}
	}
	if _, err := s.journal.Append(JournalEvent{
		EventID: fmt.Sprintf("circuit:%s:%d:upsert", plan.CircuitID, plan.PlanVersion),
		Type:    TypeCircuitUpsert, WorkloadID: plan.WorkloadID, CircuitPlan: &plan,
	}); err != nil {
		s.mu.Unlock()
		return err
	}
	s.circuits[plan.CircuitID] = plan
	delete(s.revoked, plan.CircuitID)
	s.mu.Unlock()

	for _, peer := range plan.Peers {
		if _, err := sessions[peer.WorkerID].send(TypeCircuitUpsert, "", plan); err != nil {
			return fmt.Errorf("distribute circuit to %s: %w", peer.WorkerID, err)
		}
	}
	return nil
}

// RevokeCircuit records a monotonic tombstone before notifying connected
// peers. A disconnected peer receives the same tombstone when it reconnects.
func (s *Server) RevokeCircuit(_ context.Context, revocation circuit.Revocation) error {
	if revocation.CircuitID == "" || revocation.PlanVersion == 0 {
		return errors.New("circuit revocation requires id and positive plan version")
	}
	s.mu.Lock()
	current, active := s.circuits[revocation.CircuitID]
	previous, alreadyRevoked := s.revoked[revocation.CircuitID]
	if !active && !alreadyRevoked {
		s.mu.Unlock()
		return errors.New("cannot revoke an unknown circuit")
	}
	if revocation.RevokedAt.IsZero() {
		revocation.RevokedAt = time.Now().UTC()
	}
	currentVersion := current.PlanVersion
	plan := current
	if alreadyRevoked {
		currentVersion = previous.Revocation.PlanVersion
		plan = previous.Plan
	}
	if revocation.WorkloadID == "" {
		revocation.WorkloadID = plan.WorkloadID
	}
	if revocation.PlanVersion < currentVersion || (active && revocation.PlanVersion <= currentVersion) {
		s.mu.Unlock()
		return errors.New("circuit revocation must supersede the current plan version")
	}
	if alreadyRevoked && revocation.PlanVersion == currentVersion && !reflect.DeepEqual(revocation, previous.Revocation) {
		s.mu.Unlock()
		return errors.New("circuit revocation version conflicts with current state")
	}
	if _, err := s.journal.Append(JournalEvent{
		EventID: fmt.Sprintf("circuit:%s:%d:revoke", revocation.CircuitID, revocation.PlanVersion),
		Type:    TypeCircuitRevoke, WorkloadID: plan.WorkloadID,
		CircuitPlan: &plan, CircuitRevocation: &revocation,
	}); err != nil {
		s.mu.Unlock()
		return err
	}
	delete(s.circuits, revocation.CircuitID)
	s.revoked[revocation.CircuitID] = revokedCircuit{Plan: plan, Revocation: revocation}
	sessions := make([]*Session, 0, len(plan.Peers))
	for _, peer := range plan.Peers {
		if session := s.sessions[peer.WorkerID]; session != nil {
			sessions = append(sessions, session)
		}
	}
	s.mu.Unlock()

	var sendErrors []error
	for _, session := range sessions {
		if _, err := session.send(TypeCircuitRevoke, "", revocation); err != nil {
			sendErrors = append(sendErrors, fmt.Errorf("notify %s: %w", session.workerID, err))
		}
	}
	return errors.Join(sendErrors...)
}

func (s *Server) validateCircuitPeer(peer circuit.Peer, now time.Time) error {
	membership, ok := s.registry.Membership(peer.WorkerID)
	if !ok || membership.Status != "active" {
		return fmt.Errorf("worker %s is not an active Orchestrator member", peer.WorkerID)
	}
	if err := membership.Validate(now); err != nil {
		return err
	}
	if membership.NodeID != peer.NodeID {
		return fmt.Errorf("worker %s node_id does not match its membership", peer.WorkerID)
	}
	observation, ok := s.registry.Observation(peer.WorkerID)
	if !ok || observation.Status != "active" || now.Sub(observation.ObservedAt) > 30*time.Second {
		return fmt.Errorf("worker %s has no fresh active observation", peer.WorkerID)
	}
	if observation.NodeID != peer.NodeID || observation.CircuitEndpoint != peer.Endpoint {
		return fmt.Errorf("worker %s circuit identity or endpoint does not match its observation", peer.WorkerID)
	}
	return nil
}

func (s *Server) restoreCircuits(events []JournalEvent) error {
	for _, event := range events {
		switch event.Type {
		case TypeCircuitUpsert:
			if event.CircuitPlan == nil {
				return fmt.Errorf("journal circuit upsert %s has no plan", event.EventID)
			}
			plan := *event.CircuitPlan
			if revoked, ok := s.revoked[plan.CircuitID]; ok && plan.PlanVersion <= revoked.Revocation.PlanVersion {
				continue
			}
			if current, ok := s.circuits[plan.CircuitID]; !ok || plan.PlanVersion >= current.PlanVersion {
				s.circuits[plan.CircuitID] = plan
				delete(s.revoked, plan.CircuitID)
			}
		case TypeCircuitRevoke:
			if event.CircuitPlan == nil || event.CircuitRevocation == nil {
				return fmt.Errorf("journal circuit revocation %s is incomplete", event.EventID)
			}
			revocation := *event.CircuitRevocation
			version := uint64(0)
			if current, ok := s.circuits[revocation.CircuitID]; ok {
				version = current.PlanVersion
			}
			if previous, ok := s.revoked[revocation.CircuitID]; ok && previous.Revocation.PlanVersion > version {
				version = previous.Revocation.PlanVersion
			}
			if revocation.PlanVersion >= version {
				delete(s.circuits, revocation.CircuitID)
				s.revoked[revocation.CircuitID] = revokedCircuit{Plan: *event.CircuitPlan, Revocation: revocation}
			}
		}
	}
	return nil
}

func (s *Server) replayCircuits(session *Session) error {
	s.mu.RLock()
	type replay struct {
		version uint64
		typeID  string
		value   any
	}
	var messages []replay
	for _, plan := range s.circuits {
		if plan.Includes(session.identity) && time.Now().Before(plan.ExpiresAt) {
			messages = append(messages, replay{version: plan.PlanVersion, typeID: TypeCircuitUpsert, value: plan})
		}
	}
	for _, revoked := range s.revoked {
		if revoked.Plan.Includes(session.identity) {
			messages = append(messages, replay{version: revoked.Revocation.PlanVersion, typeID: TypeCircuitRevoke, value: revoked.Revocation})
		}
	}
	s.mu.RUnlock()
	sort.Slice(messages, func(i, j int) bool { return messages[i].version < messages[j].version })
	for _, message := range messages {
		if _, err := session.send(message.typeID, "", message.value); err != nil {
			return err
		}
	}
	return nil
}
