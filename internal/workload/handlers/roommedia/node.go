package roommedia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/delivery"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

type NodeRecording = mediacontracts.NodeRecording

type NodeDetails struct {
	SessionID          string                      `json:"session_id"`
	Profile            string                      `json:"profile"`
	NodeID             string                      `json:"node_id"`
	Role               string                      `json:"role"`
	Topology           json.RawMessage             `json:"topology"`
	ServiceToken       *string                     `json:"service_token"`
	ContentKeys        []mediacontracts.ContentKey `json:"content_keys"`
	PlaybackSecret     *string                     `json:"playback_secret"`
	Recording          *NodeRecording              `json:"recording"`
	ResumeFromSequence uint64                      `json:"resume_from_sequence"`
	Replay             bool                        `json:"replay"`
	HeartbeatTimeoutMS int64                       `json:"heartbeat_timeout_ms"`
}

type NodeRunner func(ctx context.Context, node *Node) (string, error)

var (
	nodeRolesMu sync.RWMutex
	nodeRoles   = map[string]NodeRunner{}
)

func RegisterNodeRole(profile string, runner NodeRunner) {
	nodeRolesMu.Lock()
	defer nodeRolesMu.Unlock()
	nodeRoles[profile] = runner
}

var nodeValidators = map[string]func(domain.Spec) error{}

func RegisterNodeValidator(profile string, validate func(domain.Spec) error) {
	nodeRolesMu.Lock()
	defer nodeRolesMu.Unlock()
	nodeValidators[profile] = validate
}

func nodeValidator(profile string) func(domain.Spec) error {
	nodeRolesMu.RLock()
	defer nodeRolesMu.RUnlock()
	return nodeValidators[profile]
}

func nodeRunner(profile string) NodeRunner {
	nodeRolesMu.RLock()
	defer nodeRolesMu.RUnlock()
	return nodeRoles[profile]
}

func isNodeProfile(profile string) bool {
	for _, candidate := range mediacontracts.RoleProfiles {
		if candidate == profile {
			return true
		}
	}
	return false
}

func peekProfile(spec domain.Spec) string {
	var value struct {
		Details struct {
			Profile string `json:"profile"`
		} `json:"details"`
	}
	_ = json.Unmarshal(spec.Payload, &value)
	return value.Details.Profile
}

func decodeNodeSpec(spec domain.Spec) (contracts.RoomWorkerSpec[NodeDetails], *mediacontracts.Topology, error) {
	var envelope contracts.RoomWorkerSpec[json.RawMessage]
	decoder := json.NewDecoder(bytes.NewReader(spec.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return contracts.RoomWorkerSpec[NodeDetails]{}, nil, err
	}
	value := contracts.RoomWorkerSpec[NodeDetails]{Schema: envelope.Schema, Identity: envelope.Identity,
		Source: envelope.Source, Targets: envelope.Targets, Redelivery: envelope.Redelivery}
	if err := json.Unmarshal(envelope.Details, &value.Details); err != nil {
		return value, nil, err
	}
	if value.Schema != contracts.RoomWorkloadSchema || value.Identity.Kind != domain.KindRoomMedia {
		return value, nil, errors.New("invalid room media node spec")
	}
	view, err := mediacontracts.ValidateTopologyJSON(value.Details.Topology)
	if err != nil {
		return value, nil, fmt.Errorf("invalid node topology: %w", err)
	}
	if validate := nodeValidator(value.Details.Profile); validate != nil {
		if err := validate(spec); err != nil {
			return value, nil, err
		}
	}
	return value, view, nil
}

func validateNode(worker contracts.RoomWorkerSpec[NodeDetails], view *mediacontracts.Topology, now time.Time) error {
	details := worker.Details
	if mediacontracts.RoleProfiles[details.Role] != details.Profile {
		return errors.New("media node profile does not match its role")
	}
	if details.Replay {
		return errors.New("media nodes cannot replay")
	}
	if !now.Before(worker.Identity.ExpiresAt) || worker.Identity.ExpiresAt.After(now.Add(23*time.Hour)) {
		return errors.New("media node lifetime must be at most 23 hours")
	}
	if view.View == nil || view.View.NodeID != details.NodeID {
		return errors.New("a media node carries its own view of the topology")
	}
	if view.SessionID != details.SessionID {
		return errors.New("media node session_id must be the topology's session")
	}
	var self *mediacontracts.TopologyNode
	for index := range view.Nodes {
		if view.Nodes[index].NodeID == details.NodeID {
			self = &view.Nodes[index]
		}
	}
	if self == nil || self.Role != details.Role {
		return errors.New("media node is not a node of its role in the topology")
	}
	upstream := details.Role != mediacontracts.RoleSFU && details.Role != mediacontracts.RoleIngest &&
		!(details.Role == mediacontracts.RolePackage && view.DeliveryProfile == mediacontracts.DeliveryVOD)
	if upstream != (details.ServiceToken != nil) {
		return errors.New("only media nodes with a parent carry a service token")
	}
	if details.ServiceToken != nil {
		claims, _, _, err := mediacontracts.ParseTokenUnverified(*details.ServiceToken)
		if err != nil || !strings.HasPrefix(claims.KeyID, mediacontracts.ServiceKeyIDPrefix) || claims.Role != mediacontracts.RoleSubscribe ||
			claims.Identity != mediacontracts.NodeIdentityPrefix+details.NodeID || claims.Session != details.SessionID {
			return errors.New("the service token is not this node's")
		}
	}
	servesHLS := details.Role == mediacontracts.RolePackage || details.Role == mediacontracts.RoleShield || details.Role == mediacontracts.RoleEdge
	if servesHLS != (details.PlaybackSecret != nil) {
		return errors.New("only package, shield and edge nodes carry the playback secret")
	}
	protectedPackage := details.Role == mediacontracts.RolePackage && view.Protection.Mode == "protected"
	if protectedPackage != (details.ContentKeys != nil) {
		return errors.New("only a protected package node carries content keys")
	}
	vodPackage := details.Role == mediacontracts.RolePackage && view.DeliveryProfile == mediacontracts.DeliveryVOD
	recording := details.Role == mediacontracts.RolePackage && view.Packaging != nil && view.Packaging.Recording && !vodPackage
	if (recording || vodPackage) != (details.Recording != nil) {
		return errors.New("only a recording package node and a vod package node carry the recording")
	}
	if details.Recording != nil {
		if err := mediacontracts.ValidateNodeRecording(*details.Recording, vodPackage); err != nil {
			return err
		}
	}
	if details.HeartbeatTimeoutMS < 0 || details.HeartbeatTimeoutMS > 300_000 {
		return errors.New("media node heartbeat timeout is invalid")
	}
	return nil
}

type Node struct {
	handler  *Handler
	spec     domain.Spec
	identity contracts.RoomWorkloadIdentity
	details  NodeDetails
	usage    *usage.Reporter

	view         atomic.Pointer[mediacontracts.Topology]
	updates      chan *mediacontracts.Topology
	state        *AttemptState
	events       *eventIDs
	saveMu       sync.Mutex
	stateRemoved bool

	mu        sync.Mutex
	endpoints []mediacontracts.TopologyEndpoint
	load      func() mediacontracts.NodeLoad
	counters  func() NodeCounters
	mounts    []string
}

type NodeCounters struct {
	Sequence uint64
	Packets  int64
	Bytes    int64
	Dropped  int64
}

func (h *Handler) newNode(spec domain.Spec, worker contracts.RoomWorkerSpec[NodeDetails], view *mediacontracts.Topology,
	resumed bool) (*Node, error) {
	state := h.attemptState(worker.Identity, resumed, worker.Redelivery)
	key, err := nodeKey(state)
	if err != nil {
		return nil, err
	}
	node := &Node{handler: h, spec: spec, identity: worker.Identity, details: worker.Details,
		updates: make(chan *mediacontracts.Topology, 1), state: state, events: newEventIDs(worker.Identity, state)}
	if saved, err := state.loadView(view, h.now()); err == nil {
		log.Printf("room media %s: resumes on its saved topology view (epoch %d revision %d, offer epoch %d revision %d)",
			worker.Details.NodeID, saved.Epoch, saved.Revision, view.Epoch, view.Revision)
		view = saved
	} else if !errors.Is(err, errNoSavedView) {
		log.Printf("room media %s: saved topology view ignored: %v", worker.Details.NodeID, err)
	}
	node.view.Store(view)
	reporter, err := usage.New(usage.Config{WorkloadID: worker.Identity.WorkloadID, SessionID: worker.Details.SessionID,
		NodeID: worker.Details.NodeID, WorkerID: h.workerID(spec), Role: worker.Details.Role,
		Epoch: func() int64 { return node.View().Epoch }, Sink: node.sendUsage,
		Sequences: h.sequenceStore(worker.Identity), Features: h.usageFeatures}, key)
	if err != nil {
		return nil, err
	}
	node.usage = reporter
	return node, nil
}

func (n *Node) Details() NodeDetails { return n.details }

func (n *Node) Identity() contracts.RoomWorkloadIdentity { return n.identity }

func (n *Node) WorkerID() string { return n.handler.workerID(n.spec) }

func (n *Node) View() *mediacontracts.Topology { return n.view.Load() }

func (n *Node) Updates() <-chan *mediacontracts.Topology { return n.updates }

func (n *Node) Usage() *usage.Reporter { return n.usage }

func (n *Node) Config() Config { return n.handler.config }

func (n *Node) offerView(view *mediacontracts.Topology) bool {
	n.saveMu.Lock()
	current := n.view.Load()
	if current != nil && (view.Epoch < current.Epoch || view.Epoch == current.Epoch && view.Revision <= current.Revision) {
		n.saveMu.Unlock()
		return false
	}
	defer n.saveMu.Unlock()
	n.saveView(view)
	n.view.Store(view)
	for {
		select {
		case n.updates <- view:
			return true
		default:
		}
		select {
		case <-n.updates:
		default:
		}
	}
}

func (n *Node) saveView(view *mediacontracts.Topology) {
	if n.state == nil || n.stateRemoved {
		return
	}
	if err := n.state.saveView(view); err != nil {
		log.Printf("room media %s: topology view (epoch %d revision %d) not saved: %v", n.details.NodeID, view.Epoch, view.Revision, err)
	}
}

func (n *Node) removeState() {
	n.saveMu.Lock()
	defer n.saveMu.Unlock()
	n.stateRemoved = true
	n.state.Remove()
}

func (n *Node) EmitSessionEvent(body any) { n.emitSessionEvent(context.Background(), body, false) }

const finalEventWait = 30 * time.Second

func (n *Node) EmitFinalSessionEvent(ctx context.Context, body any) {
	n.emitSessionEvent(ctx, body, true)
}

type reliableFleet interface {
	SendSessionEventWait(ctx context.Context, event mediacontracts.SessionEvent, timeout time.Duration) error
}

func (n *Node) emitSessionEvent(ctx context.Context, body any, final bool) {
	encoded, err := mediacontracts.MarshalWire(body)
	if err != nil {
		return
	}
	event := mediacontracts.SessionEvent{Type: mediacontracts.SessionEventType, SchemaVersion: mediacontracts.SessionEventSchema,
		WorkloadID: n.identity.WorkloadID, NodeID: n.details.NodeID, SessionID: n.details.SessionID,
		EventID: n.nextEventID(), OccurredAtMS: time.Now().UnixMilli(), Event: encoded}
	fleet := n.handler.fleet()
	if fleet == nil {
		return
	}
	if reliable, ok := fleet.(reliableFleet); ok && final {
		err = reliable.SendSessionEventWait(ctx, event, finalEventWait)
	} else {
		err = fleet.SendSessionEvent(event)
	}
	if err != nil {
		log.Printf("room media %s: session event %s not sent: %v", n.details.NodeID, event.EventID, err)
	}
}

func (n *Node) nextEventID() string { return n.events.Next() }

func (n *Node) AttemptState() *AttemptState { return n.state }

func (n *Node) sendUsage(message mediacontracts.UsageReportMessage) error {
	fleet := n.handler.fleet()
	if fleet == nil {
		return errors.New("no media fleet")
	}
	err := fleet.SendUsageReport(message)
	if err != nil {
		log.Printf("room media %s: usage report not sent: %v", n.details.NodeID, err)
	}
	return err
}

func (h *Handler) usageFeatures() (usageV2, receipts bool) {
	if fleet, ok := h.config.Fleet.(interface {
		UsageFeatures() (usageV2, receipts bool)
	}); ok {
		return fleet.UsageFeatures()
	}
	return false, false
}

func (n *Node) SetEndpoints(endpoints []mediacontracts.TopologyEndpoint) {
	n.mu.Lock()
	n.endpoints = append([]mediacontracts.TopologyEndpoint(nil), endpoints...)
	n.mu.Unlock()
}

func (n *Node) SetLoad(load func() mediacontracts.NodeLoad) {
	n.mu.Lock()
	n.load = load
	n.mu.Unlock()
}

func (n *Node) SetCounters(counters func() NodeCounters) {
	n.mu.Lock()
	n.counters = counters
	n.mu.Unlock()
}

func (n *Node) Mount(prefix string, handler http.Handler) error {
	shared, err := n.handler.nodeListener()
	if err != nil {
		return err
	}
	if err := shared.mount(prefix, n.mountOwner(), handler); err != nil {
		return err
	}
	n.mu.Lock()
	n.mounts = append(n.mounts, prefix)
	n.mu.Unlock()
	return nil
}

func (n *Node) mountOwner() mountOwner {
	return mountOwner{nodeID: n.details.NodeID, instance: n.identity.WorkloadID, attempt: n.identity.Epoch<<32 | n.identity.Attempt}
}

func (n *Node) EndpointURL(path string) (string, error) {
	shared, err := n.handler.nodeListener()
	if err != nil {
		return "", err
	}
	base, err := url.Parse(shared.baseURL)
	if err != nil {
		return "", err
	}
	if host := n.Hostname(); host != nil {
		port := base.Port()
		base.Host = *host
		if port != "" && port != "443" {
			base.Host = net.JoinHostPort(*host, port)
		}
	}
	base.Host = strings.ToLower(base.Host)
	return strings.TrimRight(base.String(), "/") + path, nil
}

func (n *Node) Hostname() *string {
	if fleet := n.handler.config.Fleet; fleet != nil {
		if host := fleet.EdgeHostname(); host != "" {
			return &host
		}
	}
	shared, err := n.handler.nodeListener()
	if err != nil {
		return nil
	}
	if parsed, err := url.Parse(shared.baseURL); err == nil {
		host := strings.ToLower(parsed.Hostname())
		if mediacontracts.ValidHostname(host) && net.ParseIP(host) == nil {
			return &host
		}
	}
	return nil
}

func (n *Node) runtime(pin func() *string) *mediacontracts.NodeRuntime {
	n.mu.Lock()
	endpoints := append([]mediacontracts.TopologyEndpoint{}, n.endpoints...)
	n.mu.Unlock()
	runtime := &mediacontracts.NodeRuntime{SchemaVersion: mediacontracts.NodeRuntimeSchema,
		Capability: mediacontracts.RoleCapabilities[n.details.Role], NodeID: n.details.NodeID, WorkerID: n.WorkerID(),
		NodeKey: n.usage.NodeKey(), Endpoints: endpoints, Hostname: n.Hostname(), TLSCertificateSHA256: pin(),
		ExpiresAt: n.identity.ExpiresAt.UTC().Format(time.RFC3339)}
	runtime.Topology = viewVersion(n.view.Load())
	return runtime
}

func viewVersion(view *mediacontracts.Topology) *mediacontracts.NodeTopologyVersion {
	if view == nil || view.Epoch < 1 || view.Revision < 1 {
		return nil
	}
	return &mediacontracts.NodeTopologyVersion{Epoch: view.Epoch, Revision: view.Revision}
}

func (n *Node) progress(pin func() *string) contracts.MediaProgressDetails {
	n.mu.Lock()
	load, counters := n.load, n.counters
	n.mu.Unlock()
	value := contracts.MediaProgressDetails{Tracks: []contracts.MediaTrackCounters{}}
	if counters != nil {
		current := counters()
		value.Sequence, value.Packets, value.Bytes, value.Dropped = current.Sequence, current.Packets, current.Bytes, current.Dropped
	}
	value.NodeRuntime = n.runtime(pin)
	if load != nil {
		current := load()
		value.Load = &current
	}
	return value
}

func (n *Node) result(terminal string) contracts.MediaResultDetails {
	n.mu.Lock()
	counters := n.counters
	n.mu.Unlock()
	value := contracts.MediaResultDetails{Tracks: []contracts.MediaTrackCounters{}, TerminalReason: terminal}
	if counters != nil {
		current := counters()
		value.Sequence, value.Packets, value.Bytes, value.Dropped = current.Sequence, current.Packets, current.Bytes, current.Dropped
	}
	return value
}

func (h *Handler) runNode(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	worker, view, err := decodeNodeSpec(spec)
	if err != nil {
		return domain.Result{}, err
	}
	if err := validateNode(worker, view, h.now().UTC()); err != nil {
		return domain.Result{}, err
	}
	runner := nodeRunner(worker.Details.Profile)
	if runner == nil {
		return domain.Result{}, fmt.Errorf("this Worker does not serve %s nodes", worker.Details.Profile)
	}
	listener, err := h.MediaListener()
	if err != nil {
		return domain.Result{}, err
	}
	shared := listener.server
	node, err := h.newNode(spec, worker, view, delivery.Resumed(ctx))
	if err != nil {
		return domain.Result{}, err
	}
	started := false
	defer func() {
		if !started && !errors.Is(ctx.Err(), context.Canceled) {
			node.removeState()
		}
	}()
	if _, err := listener.PinFor(node.Hostname(), worker.Identity.ExpiresAt); err != nil {
		return domain.Result{}, err
	}
	var lastPin *string
	pin := func() *string {
		if value, err := listener.PinFor(node.Hostname(), worker.Identity.ExpiresAt); err == nil {
			lastPin = value
		}
		return lastPin
	}
	workloadID := worker.Identity.WorkloadID
	attempt := node.mountOwner().attempt
	h.beginAttempt(workloadID, attempt)
	if fleet := h.config.Fleet; fleet != nil {
		defer func() {
			if h.endAttempt(workloadID, attempt) {
				fleet.ForgetNode(workloadID)
			}
		}()
		if host := node.Hostname(); host != nil {
			if err := fleet.EnsureHost(workloadID, *host); err != nil {
				log.Printf("room media %s: certificate for %s: %v", worker.Details.NodeID, *host, err)
			}
		}
	} else {
		defer h.endAttempt(workloadID, attempt)
	}
	if h.config.WatchTopology != nil {
		updates, stop := h.config.WatchTopology(workloadID, worker.Identity.Epoch, worker.Identity.Attempt)
		defer stop()
		go func() {
			for next := range updates {
				if next != nil && next.View != nil && next.View.NodeID == worker.Details.NodeID && next.SessionID == worker.Details.SessionID {
					node.offerView(next)
				}
			}
		}()
	}
	defer func() {
		node.mu.Lock()
		mounts := node.mounts
		node.mu.Unlock()
		for _, prefix := range mounts {
			shared.unmount(prefix, node.mountOwner())
		}
	}()
	runContext, cancel := context.WithDeadline(ctx, worker.Identity.ExpiresAt)
	defer cancel()
	usageDone := make(chan struct{})
	go func() {
		node.usage.Run(runContext)
		close(usageDone)
	}()
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runContext.Done():
				return
			case <-ticker.C:
				current := node.progress(pin)
				if fleet := h.config.Fleet; fleet != nil && current.Load != nil {
					fleet.ReportNodeLoad(workloadID, *current.Load)
				}
				reportNodeProgress(ctx, current)
			}
		}
	}()
	started = true
	terminal, runErr := runner(runContext, node)
	cancel()
	<-progressDone
	<-usageDone
	if !errors.Is(ctx.Err(), context.Canceled) {
		node.removeState()
	}
	if terminal == "" {
		terminal = "ended"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(runContext.Err(), context.DeadlineExceeded) {
			terminal = "expired"
		}
	}
	final := node.result(terminal)
	encoded, _ := json.Marshal(final)
	result := domain.Result{BytesProcessed: final.Bytes, Outputs: map[string]string{"room_result_details": string(encoded)}}
	if runErr != nil && ctx.Err() == nil {
		return result, runErr
	}
	return result, ctx.Err()
}

func reportNodeProgress(ctx context.Context, value contracts.MediaProgressDetails) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	workloadprogress.Report(ctx, map[string]string{"room_progress_details": string(encoded)})
}

func (h *Handler) beginAttempt(workloadID string, attempt uint64) {
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()
	if current, ok := h.attempts[workloadID]; !ok || attempt >= current {
		h.attempts[workloadID] = attempt
	}
}

func (h *Handler) endAttempt(workloadID string, attempt uint64) bool {
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()
	if current, ok := h.attempts[workloadID]; ok && current == attempt {
		delete(h.attempts, workloadID)
		return true
	}
	return false
}

func (h *Handler) workerID(spec domain.Spec) string {
	return spec.Identity.WorkerID
}

func ServesMediaNodes(capabilities []string) bool {
	for _, capability := range capabilities {
		for _, roleCapability := range mediacontracts.RoleCapabilities {
			if capability == roleCapability {
				return true
			}
		}
	}
	return false
}
