package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/circuit"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

type Control interface {
	Offer(context.Context, string, domain.Spec) (runtime.Decision, error)
	Commit(context.Context, string, domain.Commit) error
	Cancel(context.Context, string, string, string) error
	Connected(string) bool
	UpsertCircuit(context.Context, circuit.Plan) error
	RevokeCircuit(context.Context, circuit.Revocation) error
}

type Orchestrator struct {
	registry *registry.Registry
	control  Control
	planner  *Planner
	topology *Topology
}

func NewOrchestrator(orchestratorRegistry *registry.Registry, control Control) *Orchestrator {
	return &Orchestrator{registry: orchestratorRegistry, control: control, planner: New(orchestratorRegistry, control.Connected), topology: NewTopology()}
}

func (o *Orchestrator) Plan(request Request, now time.Time) (Plan, error) {
	return o.planner.Build(o.withTopology(request), now)
}

func (o *Orchestrator) ObserveLinks(metrics []LinkMetric, now time.Time) error {
	return o.topology.Observe(metrics, now)
}

func (o *Orchestrator) Dispatch(ctx context.Context, request Request, now time.Time) (Plan, error) {
	plan, err := o.planner.Build(o.withTopology(request), now)
	if err != nil {
		return Plan{}, err
	}
	ttl := time.Duration(request.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	plan.ExpiresAt = now.Add(ttl)
	plan.PlanVersion = uint64(now.UnixNano())
	if plan.PlanVersion == 0 {
		plan.PlanVersion = 1
	}

	if plan.CircuitID != "" {
		peers := make([]circuit.Peer, 0, len(plan.Nodes))
		for _, node := range plan.Nodes {
			roles := []string{"relay"}
			if node.WorkerID == plan.RootWorkerID {
				roles = []string{"root"}
			}
			if len(node.Destinations) > 0 {
				roles = append(roles, "egress")
			}
			peers = append(peers, circuit.Peer{
				WorkerID: node.WorkerID, NodeID: node.NodeID, Endpoint: node.Endpoint,
				Roles: roles, Capabilities: []string{"transfer.distribute"},
			})
		}
		if err := o.control.UpsertCircuit(ctx, circuit.Plan{
			CircuitID: plan.CircuitID, WorkloadID: plan.WorkloadID, PlanVersion: plan.PlanVersion,
			Peers: peers, ExpiresAt: plan.ExpiresAt, IssuedAt: now,
		}); err != nil {
			return Plan{}, fmt.Errorf("authorize distribution circuit: %w", err)
		}
	}

	nodeByWorker := make(map[string]Node, len(plan.Nodes))
	for _, node := range plan.Nodes {
		nodeByWorker[node.WorkerID] = node
	}
	accepted := make([]Node, 0, len(plan.Nodes))
	specs := make(map[string]domain.Spec, len(plan.Nodes))
	for _, node := range plan.Nodes {
		payload := contracts.DistributionTransfer{
			TransferID: request.TransferID, CircuitID: plan.CircuitID,
			Root: node.WorkerID == plan.RootWorkerID, Offset: request.Offset, Length: request.Length,
			ExpectedSHA256: request.ExpectedSHA256, Destinations: node.Destinations,
		}
		if payload.Root {
			source := request.Source
			payload.Source = &source
		} else {
			payload.ParentNodeID = nodeByWorker[node.ParentWorkerID].NodeID
		}
		for _, childWorkerID := range node.Children {
			child := nodeByWorker[childWorkerID]
			payload.Children = append(payload.Children, contracts.DistributionChild{WorkerID: child.WorkerID, NodeID: child.NodeID})
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return Plan{}, err
		}
		resources := request.Resources
		if resources.CPUMillis == 0 {
			resources.CPUMillis = 100
		}
		if resources.MemoryBytes == 0 {
			resources.MemoryBytes = 4 << 20
		}
		if resources.BandwidthMbps == 0 {
			resources.BandwidthMbps = 10
		}
		resources.Connections = max(resources.Connections, int64(len(payload.Children)+len(payload.Destinations)+1))
		resources.Streams = max(resources.Streams, int64(len(payload.Children)+1))
		spec := domain.Spec{
			WorkloadID: plan.WorkloadID, AttemptID: "route_" + node.WorkerID,
			Identity: domain.Identity{WorkerID: node.WorkerID, OrchestratorID: membershipOrchestratorID(o.registry, node.WorkerID), NodeID: node.NodeID},
			Kind:     domain.KindTransferDistribute, Class: domain.ClassJob,
			Source:               domain.Source{System: "orchestrator-distribution", Reference: request.TransferID},
			RequiredCapabilities: []string{"transfer.distribute"}, Resources: resources,
			Lease:    domain.Lease{OfferExpiresAt: now.Add(30 * time.Second)},
			Evidence: domain.EvidencePolicy{Commitments: []string{"sha256", "bytes"}}, Payload: encoded,
		}
		decision, offerErr := o.control.Offer(ctx, node.WorkerID, spec)
		if offerErr != nil || !decision.Accepted {
			o.rollback(ctx, plan, accepted)
			if offerErr != nil {
				return Plan{}, fmt.Errorf("offer distribution to %s: %w", node.WorkerID, offerErr)
			}
			return Plan{}, fmt.Errorf("offer distribution to %s rejected: %s", node.WorkerID, decision.Reason)
		}
		accepted = append(accepted, node)
		specs[node.WorkerID] = spec
	}

	ordered := append([]Node(nil), plan.Nodes...)
	sort.Slice(ordered, func(i, j int) bool {
		return depth(ordered[i], nodeByWorker) > depth(ordered[j], nodeByWorker)
	})
	for _, node := range ordered {
		token, tokenErr := circuit.NewToken()
		if tokenErr != nil {
			o.rollback(ctx, plan, accepted)
			return Plan{}, tokenErr
		}
		spec := specs[node.WorkerID]
		if err := o.control.Commit(ctx, node.WorkerID, domain.Commit{
			WorkloadID: spec.WorkloadID, AttemptID: spec.AttemptID, PlanVersion: plan.PlanVersion,
			AssignmentToken: token, AssignmentExpiresAt: plan.ExpiresAt,
		}); err != nil {
			o.rollback(ctx, plan, accepted)
			return Plan{}, fmt.Errorf("commit distribution to %s: %w", node.WorkerID, err)
		}
	}
	return plan, nil
}

func (o *Orchestrator) withTopology(request Request) Request {
	stored := o.topology.Links()
	request.Links = append(stored, request.Links...)
	return request
}

func (o *Orchestrator) rollback(ctx context.Context, plan Plan, nodes []Node) {
	for _, node := range nodes {
		_ = o.control.Cancel(ctx, node.WorkerID, plan.WorkloadID, "route_"+node.WorkerID)
	}
	if plan.CircuitID != "" {
		_ = o.control.RevokeCircuit(ctx, circuit.Revocation{
			CircuitID: plan.CircuitID, PlanVersion: plan.PlanVersion + 1, Reason: "distribution orchestration rolled back",
		})
	}
}

func membershipOrchestratorID(orchestratorRegistry *registry.Registry, workerID string) string {
	membership, _ := orchestratorRegistry.Membership(workerID)
	return membership.OrchestratorID
}

func depth(node Node, nodes map[string]Node) int {
	result := 0
	for node.ParentWorkerID != "" {
		result++
		next, ok := nodes[node.ParentWorkerID]
		if !ok {
			break
		}
		node = next
	}
	return result
}
