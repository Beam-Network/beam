package roomworkloads

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/scheduling"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type MediaPolicy struct {
	MaxNodesPerRole   map[string]int
	NominalEgressMbps map[string]int64
	HeadroomRatio     float64
	MaxCPURatio       float64
}

func DefaultMediaPolicy() MediaPolicy {
	return MediaPolicy{
		MaxNodesPerRole: map[string]int{mediacontracts.RoleSFU: 16, mediacontracts.RoleRelay: 32, mediacontracts.RoleIngest: 8,
			mediacontracts.RoleTranscode: 4, mediacontracts.RolePackage: 8, mediacontracts.RoleShield: 64, mediacontracts.RoleEdge: 64},
		NominalEgressMbps: map[string]int64{mediacontracts.RoleSFU: 50, mediacontracts.RoleRelay: 50, mediacontracts.RoleIngest: 20,
			mediacontracts.RoleTranscode: 15, mediacontracts.RolePackage: 30, mediacontracts.RoleShield: 100, mediacontracts.RoleEdge: 100},
		HeadroomRatio: scheduling.DefaultMediaHeadroomRatio,
		MaxCPURatio:   scheduling.DefaultMediaMaxCPURatio,
	}
}

func ParseMediaRoleLimits(value string, defaults map[string]int) (map[string]int, error) {
	limits := make(map[string]int, len(defaults))
	for role, limit := range defaults {
		limits[role] = limit
	}
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		role, number, ok := strings.Cut(entry, "=")
		limit, err := strconv.Atoi(strings.TrimSpace(number))
		role = strings.TrimSpace(role)
		if !ok || err != nil || limit < 0 || !slices.Contains(contracts.RoomMediaNodeRoles, role) {
			return nil, fmt.Errorf("invalid media role limit %q: want <role>=<count> with role one of %s", entry,
				strings.Join(contracts.RoomMediaNodeRoles, ", "))
		}
		limits[role] = limit
	}
	return limits, nil
}

type mediaReservation struct {
	workerID   string
	role       string
	egressMbps int64
	ingress    int64
	encoders   int64
	at         time.Time
}

const mediaReservationTTL = time.Minute

func (m *Manager) ConfigureMedia(policy MediaPolicy, drainState string) error {
	defaults := DefaultMediaPolicy()
	if policy.MaxNodesPerRole == nil {
		policy.MaxNodesPerRole = defaults.MaxNodesPerRole
	}
	if policy.NominalEgressMbps == nil {
		policy.NominalEgressMbps = defaults.NominalEgressMbps
	}
	if policy.HeadroomRatio <= 0 || policy.HeadroomRatio > 1 {
		policy.HeadroomRatio = defaults.HeadroomRatio
	}
	if policy.MaxCPURatio <= 0 || policy.MaxCPURatio > 1 {
		policy.MaxCPURatio = defaults.MaxCPURatio
	}
	nodes := m.mediaNodes
	nodes.mu.Lock()
	defer nodes.mu.Unlock()
	nodes.policy = policy
	nodes.drainPath = drainState
	drained, _, err := readMediaDrainState(drainState)
	for _, workerID := range drained {
		nodes.draining[workerID] = true
	}
	return err
}

func readMediaDrainState(path string) (drained []string, found bool, err error) {
	if path == "" {
		return nil, false, nil
	}
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(encoded, &drained); err != nil {
		return nil, false, fmt.Errorf("read media drain state: %w", err)
	}
	return drained, true, nil
}

func (n *mediaNodes) reloadDrain() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	drained, found, err := readMediaDrainState(n.drainPath)
	if err != nil || !found {
		return err
	}
	n.draining = make(map[string]bool, len(drained))
	for _, workerID := range drained {
		n.draining[workerID] = true
	}
	return nil
}

func (m *Manager) DrainMediaWorker(workerID string, drain bool) error {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return errors.New("worker_id is required")
	}
	nodes := m.mediaNodes
	nodes.mu.Lock()
	defer nodes.mu.Unlock()
	draining := make(map[string]bool, len(nodes.draining)+1)
	for id := range nodes.draining {
		draining[id] = true
	}
	if drain {
		draining[workerID] = true
	} else {
		delete(draining, workerID)
	}
	if err := writeMediaDrainState(nodes.drainPath, draining); err != nil {
		return err
	}
	nodes.draining = draining
	return nil
}

func writeMediaDrainState(path string, draining map[string]bool) error {
	if path == "" {
		return nil
	}
	drained := make([]string, 0, len(draining))
	for id := range draining {
		drained = append(drained, id)
	}
	sort.Strings(drained)
	encoded, err := json.Marshal(drained)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func (m *Manager) MediaDrainingWorkers() []string { return m.mediaNodes.drainedWorkers() }

func (n *mediaNodes) drainedWorkers() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	drained := make([]string, 0, len(n.draining))
	for id := range n.draining {
		drained = append(drained, id)
	}
	sort.Strings(drained)
	return drained
}

func (n *mediaNodes) isDraining(workerID string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.draining[workerID]
}

func nominalNeed(policy MediaPolicy, role, nodeID string, topology *mediacontracts.Topology) (int64, int64) {
	egress := policy.NominalEgressMbps[role]
	var encoders int64
	if topology != nil && topology.Ladder != nil && (role == mediacontracts.RoleTranscode || role == mediacontracts.RolePackage) {
		var bitrate int64
		for _, rendition := range topology.Ladder.Video {
			bitrate += rendition.BitrateBPS
		}
		for _, rendition := range topology.Ladder.Audio {
			bitrate += rendition.BitrateBPS
		}
		ladderMbps := (bitrate + 999_999) / 1_000_000
		if role == mediacontracts.RolePackage {
			children := int64(0)
			for _, link := range topology.Links {
				if link.Parent == nodeID {
					children++
				}
			}
			ladderMbps *= max(children, 1)
		}
		egress = max(egress, ladderMbps)
		if role == mediacontracts.RoleTranscode {
			for _, rendition := range topology.Ladder.Video {
				if !rendition.Passthrough {
					encoders++
				}
			}
		}
	}
	if role == mediacontracts.RoleTranscode && (topology == nil || topology.Ladder == nil) {
		encoders = 1
	}
	return egress, encoders
}

func (n *mediaNodes) place(definition contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails], required []string,
	resources domain.Resources, excluded []string) (placement scheduling.Placement, err error) {
	life := n.current()
	defer n.recoverInto(life, "placement", &err)
	if life.stopped() {
		return scheduling.Placement{}, ErrMediaStopped
	}
	if n.withheld.Load() {
		return scheduling.Placement{}, ErrMediaWithheld
	}
	details := definition.Details
	topology, err := details.NodeTopology()
	if err != nil {
		return scheduling.Placement{}, err
	}
	entry, _ := contracts.NodeEntry(topology, details.NodeID)
	policy, excluded := n.placementPolicy(excluded, details.ExcludedWorkers)
	life.placeMu.Lock()
	defer life.placeMu.Unlock()
	now := n.now()
	egress, encoders := nominalNeed(policy, details.Role, details.NodeID, topology)
	ingress := int64(0)
	if details.Role == mediacontracts.RoleIngest {
		ingress = policy.NominalEgressMbps[mediacontracts.RoleIngest]
	}
	placed := map[string]scheduling.MediaLoad{}
	add := func(workerID, role string, egress, ingress, encoders int64) {
		load := placed[workerID]
		if load.Nodes == nil {
			load.Nodes = map[string]int{}
		}
		load.Nodes[role]++
		load.EgressMbps += egress
		load.IngressMbps += ingress
		load.EncoderSessions += encoders
		placed[workerID] = load
	}
	bound := map[string]bool{}
	for _, node := range n.active() {
		bound[attemptKey(node.WorkloadID, node.UnitID, node.Epoch, node.Attempt)] = true
		nodeIngress := int64(0)
		if node.Role == mediacontracts.RoleIngest {
			nodeIngress = policy.NominalEgressMbps[mediacontracts.RoleIngest]
		}
		add(node.WorkerID, node.Role, node.egressMbps, nodeIngress, node.encoders)
	}
	for key, reservation := range life.reserved {
		if bound[key] || now.Sub(reservation.at) > mediaReservationTTL {
			delete(life.reserved, key)
			continue
		}
		add(reservation.workerID, reservation.role, reservation.egressMbps, reservation.ingress, reservation.encoders)
	}
	request := scheduling.MediaRequest{Role: details.Role, Region: entry.Region, EgressMbps: egress, IngressMbps: ingress,
		EncoderSessions: encoders, MaxNodes: policy.MaxNodesPerRole[details.Role], Placed: placed,
		HeadroomRatio: policy.HeadroomRatio, MaxCPURatio: policy.MaxCPURatio}
	placement, err = n.dispatcher.SelectMediaWorker(required, resources, excluded, request)
	if err != nil {
		if len(placement.Rejected) > 0 {
			reasons := make([]string, 0, min(len(placement.Rejected), 8))
			for _, rejection := range placement.Rejected[:min(len(placement.Rejected), 8)] {
				reasons = append(reasons, rejection.WorkerID+": "+rejection.Reason)
			}
			return placement, fmt.Errorf("%w (%s)", err, strings.Join(reasons, "; "))
		}
		return placement, err
	}
	if life.stopped() || n.current() != life {
		return scheduling.Placement{}, ErrMediaStopped
	}
	identity := definition.Identity
	life.reserved[attemptKey(identity.WorkloadID, identity.UnitID, identity.Epoch, identity.Attempt)] = mediaReservation{workerID: placement.WorkerID, role: details.Role,
		egressMbps: egress, ingress: ingress, encoders: encoders, at: now}
	return placement, nil
}

func (n *mediaNodes) placementPolicy(excluded, core []string) (MediaPolicy, []string) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.policy, placementExclusions(excluded, n.draining, core)
}

func placementExclusions(excluded []string, draining map[string]bool, core []string) []string {
	out := slices.Clone(excluded)
	for workerID := range draining {
		out = append(out, workerID)
	}
	return append(out, core...)
}
