package circuit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

const tableFormatVersion = 1

type tableSnapshot struct {
	FormatVersion int               `json:"format_version"`
	Plans         map[string]Plan   `json:"plans"`
	Versions      map[string]uint64 `json:"versions"`
}

type Controller interface {
	Upsert(Plan) error
	Revoke(Revocation) error
}

type Table struct {
	mu       sync.RWMutex
	path     string
	identity domain.Identity
	plans    map[string]Plan
	versions map[string]uint64
	now      func() time.Time
}

func OpenTable(path string, identity domain.Identity) (*Table, error) {
	if path == "" {
		return nil, errors.New("circuit table path is required")
	}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	table := &Table{
		path: path, identity: identity, plans: make(map[string]Plan),
		versions: make(map[string]uint64), now: time.Now,
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return table, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshot tableSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("decode circuit table: %w", err)
	}
	if snapshot.FormatVersion != tableFormatVersion {
		return nil, fmt.Errorf("unsupported circuit table version %d", snapshot.FormatVersion)
	}
	if snapshot.Plans != nil {
		table.plans = snapshot.Plans
	}
	if snapshot.Versions != nil {
		table.versions = snapshot.Versions
	}
	for circuitID, plan := range table.plans {
		if circuitID != plan.CircuitID || table.versions[circuitID] != plan.PlanVersion {
			return nil, fmt.Errorf("persisted circuit %s has inconsistent identity or version", circuitID)
		}
		validationTime := table.now()
		if !plan.ExpiresAt.IsZero() && !validationTime.Before(plan.ExpiresAt) {
			validationTime = plan.ExpiresAt.Add(-time.Nanosecond)
		}
		if err := plan.Validate(validationTime); err != nil {
			return nil, fmt.Errorf("persisted circuit %s is invalid: %w", circuitID, err)
		}
		if !plan.Includes(identity) {
			return nil, fmt.Errorf("persisted circuit %s does not include this Worker", circuitID)
		}
	}
	return table, nil
}

func (t *Table) Upsert(plan Plan) error {
	now := t.now()
	if err := plan.Validate(now); err != nil {
		return err
	}
	if !plan.Includes(t.identity) {
		return errors.New("circuit plan does not include this Worker identity")
	}
	plan = clonePlan(plan)
	t.mu.Lock()
	defer t.mu.Unlock()
	version := t.versions[plan.CircuitID]
	if plan.PlanVersion < version {
		return errors.New("circuit plan version rolled back")
	}
	if plan.PlanVersion == version {
		if existing, ok := t.plans[plan.CircuitID]; ok && plansEqual(existing, plan) {
			return nil
		}
		return errors.New("circuit plan version conflicts with persisted state")
	}
	previous, existed := t.plans[plan.CircuitID]
	previousVersion := version
	t.plans[plan.CircuitID] = plan
	t.versions[plan.CircuitID] = plan.PlanVersion
	if err := t.persistLocked(); err != nil {
		if existed {
			t.plans[plan.CircuitID] = previous
		} else {
			delete(t.plans, plan.CircuitID)
		}
		t.versions[plan.CircuitID] = previousVersion
		return err
	}
	return nil
}

func plansEqual(left, right Plan) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func (t *Table) Revoke(revocation Revocation) error {
	if revocation.CircuitID == "" || revocation.PlanVersion == 0 {
		return errors.New("circuit revocation requires id and positive plan version")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if revocation.PlanVersion < t.versions[revocation.CircuitID] {
		return errors.New("circuit revocation version rolled back")
	}
	previous, existed := t.plans[revocation.CircuitID]
	previousVersion := t.versions[revocation.CircuitID]
	delete(t.plans, revocation.CircuitID)
	t.versions[revocation.CircuitID] = revocation.PlanVersion
	if err := t.persistLocked(); err != nil {
		if existed {
			t.plans[revocation.CircuitID] = previous
		}
		t.versions[revocation.CircuitID] = previousVersion
		return err
	}
	return nil
}

func (t *Table) Authorize(circuitID, localNodeID, peerNodeID string, now time.Time) (Plan, Peer, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	plan, ok := t.plans[circuitID]
	if !ok {
		return Plan{}, Peer{}, errors.New("circuit circuit is unknown or revoked")
	}
	if !now.Before(plan.ExpiresAt) {
		return Plan{}, Peer{}, errors.New("circuit circuit has expired")
	}
	if localNodeID != t.identity.NodeID {
		return Plan{}, Peer{}, errors.New("circuit local identity mismatch")
	}
	peer, ok := plan.Peer(peerNodeID)
	if !ok || peerNodeID == localNodeID {
		return Plan{}, Peer{}, errors.New("peer is not authorized by this circuit")
	}
	return clonePlan(plan), clonePeer(peer), nil
}

func (t *Table) Plan(circuitID string) (Plan, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	plan, ok := t.plans[circuitID]
	return clonePlan(plan), ok
}

func (t *Table) Plans() []Plan {
	t.mu.RLock()
	defer t.mu.RUnlock()
	plans := make([]Plan, 0, len(t.plans))
	for _, plan := range t.plans {
		plans = append(plans, clonePlan(plan))
	}
	return plans
}

func clonePlan(plan Plan) Plan {
	cloned := plan
	cloned.Peers = make([]Peer, len(plan.Peers))
	for index, peer := range plan.Peers {
		cloned.Peers[index] = clonePeer(peer)
	}
	return cloned
}

func clonePeer(peer Peer) Peer {
	cloned := peer
	cloned.StandbyEndpoints = append([]string(nil), peer.StandbyEndpoints...)
	cloned.Roles = append([]string(nil), peer.Roles...)
	cloned.Capabilities = append([]string(nil), peer.Capabilities...)
	return cloned
}

func (t *Table) persistLocked() error {
	directory := filepath.Dir(t.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".circuits-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if err := json.NewEncoder(temporary).Encode(tableSnapshot{
		FormatVersion: tableFormatVersion, Plans: t.plans, Versions: t.versions,
	}); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, t.path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err == nil {
		err = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return err
}

var _ Controller = (*Table)(nil)
