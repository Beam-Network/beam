package roomworkloads

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/orchestrator/dispatch"
	"github.com/Beam-Network/beam/internal/orchestrator/roomworkload"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

type mediaRecord = record[contracts.MediaUnitDetails, contracts.MediaResultDetails]

type mediaNodes struct {
	dispatcher *dispatch.Service
	now        func() time.Time
	records    func() []mediaRecord
	upstream   *mediaUpstream

	mu        sync.RWMutex
	policy    MediaPolicy
	draining  map[string]bool
	drainPath string
	workers   MediaWorkerControl
	offers    MediaOfferSource

	life          atomic.Pointer[mediaLife]
	lifeMu        sync.Mutex
	restarted     chan struct{}
	onPanic       func(*MediaPanic)
	held          bool
	panics        []time.Time
	restartPolicy mediaRestartPolicy
	withheld      atomic.Bool
}

type nodeShape struct {
	region        string
	egressMbps    int64
	encoders      int64
	nodeKey       []byte
	seenAt        time.Time
	topologyEpoch int64
	computed      bool
}

const mediaEndedGrace = 5 * time.Minute

const mediaShapeRetention = 15 * time.Minute

func newMediaNodes(dispatcher *dispatch.Service, now func() time.Time) *mediaNodes {
	nodes := &mediaNodes{dispatcher: dispatcher, now: now, policy: DefaultMediaPolicy(), draining: map[string]bool{},
		upstream: newMediaUpstream(), restarted: make(chan struct{}), restartPolicy: defaultMediaRestartPolicy()}
	nodes.life.Store(newMediaLife())
	return nodes
}

type MediaNode struct {
	WorkerID      string                   `json:"worker_id"`
	WorkloadID    string                   `json:"workload_id"`
	UnitID        string                   `json:"unit_id"`
	Epoch         uint64                   `json:"epoch"`
	Attempt       uint64                   `json:"attempt"`
	SessionID     string                   `json:"session_id"`
	NodeID        string                   `json:"node_id"`
	Role          string                   `json:"role"`
	Region        string                   `json:"region"`
	State         string                   `json:"state"`
	Load          *mediacontracts.NodeLoad `json:"load"`
	definitionID  string
	ended         bool
	nodeKey       []byte
	egressMbps    int64
	topologyEpoch int64
	encoders      int64
}

func (m *Manager) MediaNodes() []MediaNode { return m.mediaNodes.active() }

func (n *mediaNodes) active() []MediaNode { return n.collect(time.Time{}) }

func (n *mediaNodes) collect(endedSince time.Time) []MediaNode {
	if n.records == nil {
		return nil
	}
	n.mu.RLock()
	policy := n.policy
	n.mu.RUnlock()
	life := n.current()
	now := n.now()
	var nodes []MediaNode
	for _, current := range n.records() {
		definition := current.Definition.Workload
		details := definition.Details
		if !contracts.IsRoomMediaNodeProfile(details.Profile) {
			continue
		}
		recordEnded := current.UpstreamDelivered || current.Lifecycle == "cancelled"
		recent := !endedSince.IsZero() && current.UpdatedAt.After(endedSince)
		if recordEnded && !recent {
			continue
		}
		shape := life.shape(current.Definition.ID, policy, details, now)
		for _, attempt := range current.Attempts {
			ended := recordEnded || attempt.State == roomworkload.AttemptCompleted || attempt.State == roomworkload.AttemptFailed ||
				attempt.State == roomworkload.AttemptCancelled
			if attempt.WorkerID == "" || (ended && !recent) {
				continue
			}
			node := MediaNode{WorkerID: attempt.WorkerID, WorkloadID: definition.Identity.WorkloadID, UnitID: definition.Identity.UnitID,
				Epoch: definition.Identity.Epoch, Attempt: definition.Identity.Attempt, SessionID: details.SessionID, NodeID: details.NodeID,
				Role: details.Role, Region: shape.region, State: string(attempt.State), definitionID: current.Definition.ID, ended: ended, topologyEpoch: shape.topologyEpoch,
				egressMbps: shape.egressMbps, encoders: shape.encoders}
			if attempt.Progress != nil {
				var progress contracts.MediaProgressDetails
				if json.Unmarshal(attempt.Progress.Progress.Details, &progress) == nil {
					node.Load = progress.Load
					if progress.NodeRuntime != nil {
						n.rememberNodeKey(current.Definition.ID, progress.NodeRuntime.NodeKey)
					}
				}
			}
			node.nodeKey = life.nodeKey(current.Definition.ID)
			nodes = append(nodes, node)
		}
	}
	life.shapesMu.Lock()
	for id, shape := range life.shapes {
		if now.Sub(shape.seenAt) > mediaShapeRetention {
			delete(life.shapes, id)
		}
	}
	life.shapesMu.Unlock()
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].WorkerID != nodes[j].WorkerID {
			return nodes[i].WorkerID < nodes[j].WorkerID
		}
		return nodes[i].WorkloadID < nodes[j].WorkloadID
	})
	return nodes
}

func (l *mediaLife) shape(definitionID string, policy MediaPolicy, details contracts.MediaUnitDetails, now time.Time) nodeShape {
	l.shapesMu.Lock()
	if shape, ok := l.shapes[definitionID]; ok && shape.computed {
		shape.seenAt = now
		value := *shape
		l.shapesMu.Unlock()
		return value
	}
	l.shapesMu.Unlock()
	shape := &nodeShape{seenAt: now, computed: true}
	topology, _ := details.NodeTopology()
	shape.egressMbps, shape.encoders = nominalNeed(policy, details.Role, details.NodeID, topology)
	if entry, found := contracts.NodeEntry(topology, details.NodeID); found {
		shape.region = entry.Region
	}
	if topology != nil {
		shape.topologyEpoch = topology.Epoch
	}
	l.shapesMu.Lock()
	if existing, ok := l.shapes[definitionID]; ok {
		shape.nodeKey = existing.nodeKey
	}
	l.shapes[definitionID] = shape
	value := *shape
	l.shapesMu.Unlock()
	return value
}

func (n *mediaNodes) rememberNodeKey(definitionID, encoded string) {
	n.current().rememberNodeKey(definitionID, encoded, n.now())
}

func (l *mediaLife) rememberNodeKey(definitionID, encoded string, now time.Time) {
	key, err := mediacontracts.DecodeB64URL(encoded)
	if err != nil || len(key) != 32 {
		return
	}
	l.shapesMu.Lock()
	defer l.shapesMu.Unlock()
	shape := l.shapes[definitionID]
	if shape == nil {
		shape = &nodeShape{}
		l.shapes[definitionID] = shape
	}
	shape.nodeKey, shape.seenAt = key, now
}

func (l *mediaLife) nodeKey(definitionID string) []byte {
	l.shapesMu.Lock()
	defer l.shapesMu.Unlock()
	if shape := l.shapes[definitionID]; shape != nil {
		return shape.nodeKey
	}
	return nil
}

func (node MediaNode) attemptKey() string {
	return attemptKey(node.WorkloadID, node.UnitID, node.Epoch, node.Attempt)
}

func attemptKey(workloadID, unitID string, epoch, attempt uint64) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d", workloadID, unitID, epoch, attempt)
}

func (m *Manager) MediaCapacityReport(hotkey string, now time.Time) (report mediacontracts.CapacityReport, joined string) {
	nodes := m.mediaNodes
	life := nodes.current()
	empty := func() mediacontracts.CapacityReport {
		return mediacontracts.CapacityReport{Type: mediacontracts.CapacityReportType, SchemaVersion: mediacontracts.CapacitySchema,
			OrchestratorHotkey: strings.TrimSpace(hotkey), ReportedAtMS: now.UnixMilli(), Workers: []mediacontracts.WorkerCapacity{}}
	}
	defer func() {
		if value := recover(); value != nil {
			nodes.contain(life, "capacity report", value)
			report, joined = empty(), ""
		}
	}()
	byWorker := map[string][]MediaNode{}
	for _, node := range nodes.active() {
		byWorker[node.WorkerID] = append(byWorker[node.WorkerID], node)
	}
	stopped := life.stopped()
	report = empty()
	fingerprint := make([]string, 0)
	for _, observation := range m.dispatcher.WorkerObservations() {
		if observation.Media == nil || now.Sub(observation.ObservedAt) > contracts.MediaCapacityStaleMS*time.Millisecond ||
			!m.dispatcher.WorkerConnected(observation.WorkerID) {
			continue
		}
		capacity := *observation.Media
		capacity.Roles = slices.Clone(capacity.Roles)
		if stopped || nodes.withheld.Load() || observation.Status != "active" || nodes.isDraining(observation.WorkerID) {
			capacity.Roles = []string{}
		}
		var egressBPS, ingressBPS, subscribers int64
		for _, node := range byWorker[observation.WorkerID] {
			if node.Load != nil {
				egressBPS += node.Load.EgressBPS
				ingressBPS += node.Load.IngressBPS
				subscribers += node.Load.Subscribers
			}
		}
		sessions := map[string]bool{}
		for _, node := range byWorker[observation.WorkerID] {
			sessions[node.SessionID] = true
		}
		if len(sessions) > 0 {
			capacity.Load.Sessions = int64(len(sessions))
		}
		capacity.Load.Subscribers = max(capacity.Load.Subscribers, subscribers)
		capacity.Load.EgressMbps = max(capacity.Load.EgressMbps, egressBPS/1_000_000)
		capacity.Load.IngressMbps = max(capacity.Load.IngressMbps, ingressBPS/1_000_000)
		report.Workers = append(report.Workers, capacity)
		region := ""
		if capacity.RegionClaim != nil {
			region = *capacity.RegionClaim
		}
		served := make([]string, 0, len(capacity.Certificates))
		for _, certificate := range capacity.Certificates {
			served = append(served, certificate.Hostname+":"+certificate.SHA256[:min(12, len(certificate.SHA256))])
		}
		fingerprint = append(fingerprint, capacity.WorkerID+"="+strings.Join(capacity.Roles, "+")+"@"+region+"#"+strings.Join(served, "+")+
			"!"+strings.Join(capacity.Features, "+"))
	}
	if len(report.Workers) > 4096 {
		report.Workers = report.Workers[:4096]
	}
	return report, strings.Join(fingerprint, ",")
}

func (m *Manager) MediaCapabilities() []string {
	if m.mediaNodes.current().stopped() || m.mediaNodes.withheld.Load() {
		return nil
	}
	var capabilities []string
	for _, capability := range contracts.RoomMediaNodeCapabilities() {
		if m.CapacityCapabilityAvailable(domain.KindRoomMedia, capability) {
			capabilities = append(capabilities, capability)
		}
	}
	return capabilities
}
