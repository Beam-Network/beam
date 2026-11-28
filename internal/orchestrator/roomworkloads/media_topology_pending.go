package roomworkloads

import (
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const (
	pendingTopologyTTL      = 10 * time.Minute
	pendingTopologyMax      = 4096
	pendingTopologyBytes    = 64 << 20
	pendingTopologyFlush    = time.Second
	pendingTopologyLogEvery = time.Minute
)

type pendingTopology struct {
	payload   json.RawMessage
	epoch     int64
	revision  int64
	nodeID    string
	sessionID string
	keptAt    time.Time
	expires   time.Time
	delivered bool
	loggedAt  time.Time
}

func newerView(epoch, revision, thanEpoch, thanRevision int64) bool {
	return epoch > thanEpoch || (epoch == thanEpoch && revision > thanRevision)
}

func (n *mediaNodes) keepTopology(update contracts.MediaTopologyUpdateWire, topology *mediacontracts.Topology,
	payload json.RawMessage) (*mediaLife, string, pendingTopology, bool) {
	key := attemptKey(update.WorkloadID, update.UnitID, update.Epoch, update.Attempt)
	now := n.now()
	expires := now.Add(pendingTopologyTTL)
	if topology.ExpiresAtMS > 0 {
		viewExpiry := time.UnixMilli(topology.ExpiresAtMS)
		if !viewExpiry.After(now) {
			return nil, key, pendingTopology{}, false
		}
		if viewExpiry.Before(expires) {
			expires = viewExpiry
		}
	}
	life := n.lockCurrentTopologies()
	defer life.topologyMu.Unlock()
	life.pruneTopologyLocked(now)
	if current := life.topologies[key]; current != nil && !newerView(topology.Epoch, topology.Revision, current.epoch, current.revision) {
		return life, key, pendingTopology{}, false
	}
	if previous := life.topologies[key]; previous != nil {
		life.topologyBytes -= len(previous.payload)
	}
	entry := &pendingTopology{payload: append(json.RawMessage(nil), payload...), epoch: topology.Epoch, revision: topology.Revision,
		nodeID: topology.View.NodeID, sessionID: topology.SessionID, keptAt: now, expires: expires}
	life.topologies[key] = entry
	life.topologyBytes += len(entry.payload)
	for len(life.topologies) > pendingTopologyMax || life.topologyBytes > pendingTopologyBytes {
		oldestKey, oldest := "", time.Time{}
		for candidate, kept := range life.topologies {
			if candidate != key && (oldestKey == "" || kept.keptAt.Before(oldest)) {
				oldestKey, oldest = candidate, kept.keptAt
			}
		}
		if oldestKey == "" {
			break
		}
		life.dropTopologyLocked(oldestKey)
	}
	return life, key, *entry, true
}

func (n *mediaNodes) lockCurrentTopologies() *mediaLife {
	for {
		life := n.current()
		life.topologyMu.Lock()
		if n.current() == life {
			return life
		}
		life.topologyMu.Unlock()
	}
}

func (l *mediaLife) withTopologies(fn func()) {
	l.topologyMu.Lock()
	defer l.topologyMu.Unlock()
	fn()
}

func (l *mediaLife) dropTopologyLocked(key string) {
	if entry := l.topologies[key]; entry != nil {
		l.topologyBytes -= len(entry.payload)
		delete(l.topologies, key)
	}
}

func (l *mediaLife) pruneTopologyLocked(now time.Time) {
	for key, entry := range l.topologies {
		if now.After(entry.expires) {
			l.dropTopologyLocked(key)
		}
	}
}

func (l *mediaLife) settleTopology(key string, epoch, revision int64) {
	l.topologyMu.Lock()
	defer l.topologyMu.Unlock()
	if entry := l.topologies[key]; entry != nil && entry.epoch == epoch && entry.revision == revision {
		l.topologyBytes -= len(entry.payload)
		entry.payload, entry.delivered = nil, true
	}
}

func (n *mediaNodes) activeByAttempt() map[string]MediaNode {
	nodes := map[string]MediaNode{}
	for _, node := range n.active() {
		if !node.ended {
			nodes[node.attemptKey()] = node
		}
	}
	return nodes
}

var errTopologyNotThisNode = errors.New("media_topology_update is not this node's view")

func (n *mediaNodes) deliverTopology(life *mediaLife, nodes map[string]MediaNode, key string, entry pendingTopology) (bool, error) {
	node, ok := nodes[key]
	if !ok {
		return false, nil
	}
	if entry.nodeID != node.NodeID || entry.sessionID != node.SessionID {
		life.withTopologies(func() { life.dropTopologyLocked(key) })
		return false, errTopologyNotThisNode
	}
	if err := n.sendWorker(node.WorkerID, mediacontracts.TopologyUpdateType, entry.payload); err != nil {
		return false, err
	}
	life.settleTopology(key, entry.epoch, entry.revision)
	return true, nil
}

func (n *mediaNodes) ensureTopologyFlusher(life *mediaLife) {
	start := false
	life.withTopologies(func() {
		if !life.topologyFlushing && !life.stopped() {
			life.topologyFlushing, start = true, true
		}
	})
	if !start {
		return
	}
	go func() {
		defer func() {
			if value := recover(); value != nil {
				n.contain(life, "topology updates", value)
			}
			life.withTopologies(func() { life.topologyFlushing = false })
		}()
		ticker := time.NewTicker(pendingTopologyFlush)
		defer ticker.Stop()
		for {
			select {
			case <-life.stop:
				return
			case <-ticker.C:
				if !n.flushTopologies(life) {
					return
				}
			}
		}
	}()
}

func (n *mediaNodes) flushTopologies(life *mediaLife) bool {
	now := n.now()
	waiting := map[string]pendingTopology{}
	life.withTopologies(func() {
		life.pruneTopologyLocked(now)
		for key, entry := range life.topologies {
			if !entry.delivered {
				waiting[key] = *entry
			}
		}
	})
	if len(waiting) == 0 {
		return false
	}
	nodes := n.activeByAttempt()
	for key, entry := range waiting {
		delivered, err := n.deliverTopology(life, nodes, key, entry)
		switch {
		case delivered:
			log.Printf("media_topology_update for %s (epoch %d revision %d) delivered once the attempt became active",
				key, entry.epoch, entry.revision)
		case errors.Is(err, errTopologyNotThisNode):
			log.Printf("media_topology_update for %s dropped: %v", key, err)
		case err != nil:
			logNow := false
			life.withTopologies(func() {
				if kept := life.topologies[key]; kept != nil && now.Sub(kept.loggedAt) >= pendingTopologyLogEvery {
					kept.loggedAt, logNow = now, true
				}
			})
			if logNow {
				log.Printf("media_topology_update for %s (epoch %d revision %d) not delivered yet: %v", key, entry.epoch, entry.revision, err)
			}
		}
	}
	return true
}

func (n *mediaNodes) withLatestView(definition contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails]) contracts.RoomWorkloadDefinition[contracts.MediaUnitDetails] {
	if n == nil || !contracts.IsRoomMediaNodeProfile(definition.Details.Profile) {
		return definition
	}
	identity := definition.Identity
	key := attemptKey(identity.WorkloadID, identity.UnitID, identity.Epoch, identity.Attempt)
	life := n.current()
	var payload json.RawMessage
	var epoch, revision int64
	life.withTopologies(func() {
		if entry := life.topologies[key]; entry != nil && !entry.delivered && entry.nodeID == definition.Details.NodeID &&
			entry.sessionID == definition.Details.SessionID {
			payload, epoch, revision = entry.payload, entry.epoch, entry.revision
		}
	})
	if payload == nil {
		return definition
	}
	offered, err := definition.Details.NodeTopology()
	if err != nil || !newerView(epoch, revision, offered.Epoch, offered.Revision) {
		return definition
	}
	update, _, err := contracts.DecodeMediaTopologyUpdate(payload)
	if err != nil {
		return definition
	}
	updated := definition
	updated.Details.Topology = append(json.RawMessage(nil), update.Topology...)
	return updated
}
