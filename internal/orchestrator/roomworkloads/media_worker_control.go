package roomworkloads

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type MediaWorkerControl interface {
	SendMediaControl(workerID, messageType string, payload json.RawMessage) error
	MediaCapable(workerID string) bool
}

func (m *Manager) RegisterMediaWorkers(workers MediaWorkerControl) {
	m.mediaNodes.mu.Lock()
	m.mediaNodes.workers = workers
	m.mediaNodes.mu.Unlock()
}

func (m *Manager) ForwardMediaCapacityAck(ack mediacontracts.MediaAck, workerIDs []string) {
	var contained error
	defer m.mediaNodes.recoverInto(m.mediaNodes.current(), "capacity ack", &contained)
	encoded, err := mediacontracts.MarshalWire(ack)
	if err != nil {
		return
	}
	for _, workerID := range workerIDs {
		_ = m.mediaNodes.sendWorker(workerID, mediacontracts.CapacityReportAckType, encoded)
	}
}

func (m *Manager) HandleMediaTopologyUpdate(payload json.RawMessage) (err error) {
	nodes := m.mediaNodes
	defer nodes.recoverInto(nodes.current(), "topology update", &err)
	update, topology, err := contracts.DecodeMediaTopologyUpdate(payload)
	if err != nil {
		return err
	}
	life, key, entry, latest := nodes.keepTopology(update, topology, payload)
	if !latest {
		return nil
	}
	delivered, err := nodes.deliverTopology(life, nodes.activeByAttempt(), key, entry)
	if errors.Is(err, errTopologyNotThisNode) {
		return err
	}
	if err != nil || !delivered {
		nodes.ensureTopologyFlusher(life)
	}
	return nil
}

var mediaRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func (m *Manager) HandleMediaCertResult(payload json.RawMessage) (err error) {
	defer m.mediaNodes.recoverInto(m.mediaNodes.current(), "certificate result", &err)
	var result mediacontracts.CertResult
	if err := mediacontracts.UnmarshalStrict(payload, &result); err != nil {
		return fmt.Errorf("invalid media_cert_result: %w", err)
	}
	if result.Type != mediacontracts.CertResultType || result.SchemaVersion != mediacontracts.EdgeCertSchema ||
		!mediaRequestIDPattern.MatchString(result.RequestID) || (result.Status != "issued" && result.Status != "refused") ||
		(result.Status == "issued") != (result.CertificatePEM != nil) {
		return errors.New("invalid media_cert_result")
	}
	return m.mediaNodes.sendWorker(result.WorkerID, mediacontracts.CertResultType, payload)
}

func (n *mediaNodes) sendWorker(workerID, messageType string, payload json.RawMessage) error {
	n.mu.RLock()
	workers := n.workers
	n.mu.RUnlock()
	if workers == nil {
		return errors.New("media Worker control is not connected")
	}
	if !workers.MediaCapable(workerID) {
		return fmt.Errorf("Worker %s is not connected with media roles", workerID)
	}
	return workers.SendMediaControl(workerID, messageType, payload)
}

func (m *Manager) HandleWorkerMediaControl(workerID, messageType string, payload json.RawMessage) (err error) {
	defer m.mediaNodes.recoverInto(m.mediaNodes.current(), "Worker media control", &err)
	switch messageType {
	case mediacontracts.SessionEventType:
		var event mediacontracts.SessionEvent
		if err := mediacontracts.UnmarshalStrict(payload, &event); err != nil {
			return fmt.Errorf("invalid media_session_event: %w", err)
		}
		if event.OccurredAtMS < 0 {
			return errors.New("invalid media_session_event envelope")
		}
		if err := mediacontracts.ValidateSessionEvent(event); err != nil {
			return fmt.Errorf("invalid media_session_event: %w", err)
		}
		if _, err := m.mediaNodes.boundNodes(workerID, event.WorkloadID, event.NodeID, event.SessionID); err != nil {
			return err
		}
		var body struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(event.Event, &body)
		return m.mediaNodes.upstream.enqueue("event\x00"+event.WorkloadID+"\x00"+event.NodeID, workerID, false,
			MediaMessage{Type: messageType, Payload: payload, expires: m.now().Add(mediaControlRetryWindow),
				final:    mediaTerminalEvents[body.Kind],
				describe: fmt.Sprintf("node %s of %s, event %s %s", event.NodeID, event.WorkloadID, event.EventID, body.Kind)})
	case mediacontracts.UsageReportType:
		var message mediacontracts.UsageReportMessage
		if err := mediacontracts.UnmarshalStrict(payload, &message); err != nil {
			return fmt.Errorf("invalid media_usage_report: %w", err)
		}
		if message.Type != mediacontracts.UsageReportType || message.SchemaVersion != message.Report.Schema ||
			(message.Report.Schema != mediacontracts.UsageReportSchema && message.Report.Schema != mediacontracts.UsageReportV2Schema) {
			return errors.New("invalid media_usage_report envelope")
		}
		candidates, err := m.mediaNodes.boundNodes(workerID, message.WorkloadID, message.NodeID, "")
		if err != nil {
			return err
		}
		report, node, signed, err := verifyUsageReport(message, workerID, candidates)
		if err != nil {
			return err
		}
		if !signed && report.TopologyEpoch < node.topologyEpoch {
			return fmt.Errorf("media_usage_report topology epoch %d predates attempt %d of %s", report.TopologyEpoch,
				node.Attempt, node.NodeID)
		}
		if report.NodeID != node.NodeID || report.WorkerID != workerID || report.SessionID != node.SessionID || report.Role != node.Role {
			return errors.New("media_usage_report payload names another node")
		}
		if report.Sequence < 1 {
			return errors.New("media_usage_report sequence starts at 1")
		}
		if err := m.mediaNodes.upstream.admitUsage(node.attemptKey(), uint64(report.Sequence), report.PeriodStartMS, m.now()); err != nil {
			return fmt.Errorf("refuse media_usage_report of %s: %w", node.NodeID, err)
		}
		expires := m.now().Add(MediaUsageReportMaxAge)
		if report.PeriodEndMS > 0 {
			expires = time.UnixMilli(report.PeriodEndMS).Add(MediaUsageReportMaxAge)
		}
		return m.mediaNodes.upstream.enqueue("usage\x00"+node.attemptKey(), workerID, true,
			MediaMessage{Type: messageType, Payload: payload, expires: expires, final: node.ended,
				periodStart: time.UnixMilli(report.PeriodStartMS),
				describe:    fmt.Sprintf("node %s of %s attempt %d, sequence %d", node.NodeID, node.WorkloadID, node.Attempt, report.Sequence)})
	case mediacontracts.CertRequestType:
		var request mediacontracts.CertRequest
		if err := mediacontracts.UnmarshalStrict(payload, &request); err != nil {
			return fmt.Errorf("invalid media_cert_request: %w", err)
		}
		if request.Type != mediacontracts.CertRequestType || request.SchemaVersion != mediacontracts.EdgeCertSchema ||
			!mediaRequestIDPattern.MatchString(request.RequestID) || request.WorkerID != workerID ||
			!mediacontracts.ValidHostname(request.Hostname) || len(request.CSRPEM) > 16_384 ||
			!strings.HasPrefix(request.CSRPEM, "-----BEGIN CERTIFICATE REQUEST-----") {
			return errors.New("invalid media_cert_request")
		}
		return m.mediaNodes.upstream.enqueue("cert\x00"+workerID, workerID, false,
			MediaMessage{Type: messageType, Payload: payload, expires: m.now().Add(mediaControlRetryWindow),
				describe: fmt.Sprintf("host %s, request %s", request.Hostname, request.RequestID)})
	default:
		return fmt.Errorf("unsupported Worker media control %q", messageType)
	}
}

var mediaTerminalEvents = map[string]bool{"recording_started": true, "recording_ready": true, "recording_failed": true,
	"ingest_disconnected": true, "participant_left": true}

func (n *mediaNodes) boundNodes(workerID, workloadID, nodeID, sessionID string) ([]MediaNode, error) {
	var live, ended []MediaNode
	for _, node := range n.collect(n.now().Add(-mediaEndedGrace)) {
		if node.WorkloadID != workloadID || node.WorkerID != workerID {
			continue
		}
		if node.NodeID != nodeID || (sessionID != "" && node.SessionID != sessionID) {
			return nil, errors.New("media message names another node of the workload")
		}
		if node.ended {
			ended = append(ended, node)
		} else {
			live = append(live, node)
		}
	}
	if len(live)+len(ended) == 0 {
		return nil, fmt.Errorf("Worker %s holds no active media node %s", workerID, workloadID)
	}
	return append(live, ended...), nil
}

func verifyUsageReport(message mediacontracts.UsageReportMessage, workerID string,
	candidates []MediaNode) (usageReportHead, MediaNode, bool, error) {
	anyKey := false
	for _, node := range candidates {
		anyKey = anyKey || len(node.nodeKey) == 32
	}
	var lastErr error
	var unkeyed *MediaNode
	for index, node := range candidates {
		if len(node.nodeKey) != 32 {
			if unkeyed == nil && (!node.ended || !anyKey) {
				unkeyed = &candidates[index]
			}
			continue
		}
		report, err := decodeUsageReport(message.Report, node.nodeKey)
		if err == nil {
			return report, node, true, nil
		}
		lastErr = err
	}
	if unkeyed == nil {
		return usageReportHead{}, MediaNode{}, false, fmt.Errorf("media_usage_report is not signed by the node key: %w", lastErr)
	}
	decoded, err := mediacontracts.DecodeB64URL(message.Report.Payload)
	if err != nil {
		return usageReportHead{}, MediaNode{}, false, errors.New("invalid media_usage_report payload")
	}
	report, err := parseUsageReport(message.Report.Schema, decoded)
	if err != nil {
		return usageReportHead{}, MediaNode{}, false, errors.New("invalid media_usage_report payload")
	}
	return report, *unkeyed, false, nil
}

type usageReportHead struct {
	SessionID, NodeID, WorkerID, Role                   string
	TopologyEpoch, Sequence, PeriodStartMS, PeriodEndMS int64
}

func decodeUsageReport(envelope mediacontracts.SignedEnvelope, nodeKey []byte) (usageReportHead, error) {
	payload, err := mediacontracts.OpenEnvelope(envelope, envelope.Schema, nodeKey)
	if err != nil {
		return usageReportHead{}, err
	}
	return parseUsageReport(envelope.Schema, payload)
}

func parseUsageReport(schema string, payload []byte) (usageReportHead, error) {
	if schema == mediacontracts.UsageReportV2Schema {
		report, err := mediacontracts.ParseUsageReportV2(payload)
		return usageReportHead{report.SessionID, report.NodeID, report.WorkerID, report.Role, report.TopologyEpoch, report.Sequence,
			report.PeriodStartMS, report.PeriodEndMS}, err
	}
	var report mediacontracts.UsageReport
	if mediacontracts.UnmarshalStrict(payload, &report) != nil || report.SchemaVersion != mediacontracts.UsageReportSchema {
		return usageReportHead{}, errors.New("invalid beam-media-usage/v1 payload")
	}
	return usageReportHead{report.SessionID, report.NodeID, report.WorkerID, report.Role, report.TopologyEpoch, report.Sequence,
		report.PeriodStartMS, report.PeriodEndMS}, nil
}
