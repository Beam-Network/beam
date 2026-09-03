package routing

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"time"

	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

var ErrNoRoute = errors.New("no feasible distribution route")

type AccessMetric struct {
	WorkerID       string  `json:"worker_id"`
	RTTMillis      float64 `json:"rtt_millis"`
	ThroughputMbps float64 `json:"throughput_mbps"`
}

type LinkMetric struct {
	FromWorkerID   string    `json:"from_worker_id"`
	ToWorkerID     string    `json:"to_worker_id"`
	RTTMillis      float64   `json:"rtt_millis"`
	ThroughputMbps float64   `json:"throughput_mbps"`
	LossPPM        float64   `json:"loss_ppm,omitempty"`
	ObservedAt     time.Time `json:"observed_at,omitempty"`
}

type Destination struct {
	DestinationID string                 `json:"destination_id"`
	Endpoint      contracts.HTTPEndpoint `json:"endpoint"`
	Access        []AccessMetric         `json:"access,omitempty"`
}

type Request struct {
	TransferID        string                 `json:"transfer_id"`
	Source            contracts.HTTPEndpoint `json:"source"`
	Offset            int64                  `json:"offset"`
	Length            int64                  `json:"length"`
	ExpectedSHA256    string                 `json:"expected_sha256,omitempty"`
	Destinations      []Destination          `json:"destinations"`
	SourceAccess      []AccessMetric         `json:"source_access,omitempty"`
	Links             []LinkMetric           `json:"links,omitempty"`
	MaxObservationAge time.Duration          `json:"-"`
	Resources         domain.Resources       `json:"resources,omitempty"`
	TTLSeconds        int64                  `json:"ttl_seconds,omitempty"`
}

type Node struct {
	WorkerID       string                              `json:"worker_id"`
	NodeID         string                              `json:"node_id"`
	Endpoint       string                              `json:"circuit_endpoint"`
	ParentWorkerID string                              `json:"parent_worker_id,omitempty"`
	Children       []string                            `json:"children,omitempty"`
	Destinations   []contracts.DistributionDestination `json:"destinations,omitempty"`
	ArrivalMillis  float64                             `json:"arrival_millis"`
}

type Plan struct {
	TransferID                string    `json:"transfer_id"`
	WorkloadID                string    `json:"workload_id"`
	CircuitID                 string    `json:"circuit_id,omitempty"`
	RootWorkerID              string    `json:"root_worker_id"`
	Nodes                     []Node    `json:"nodes"`
	EstimatedCompletionMillis float64   `json:"estimated_completion_millis"`
	PlanVersion               uint64    `json:"plan_version,omitempty"`
	ExpiresAt                 time.Time `json:"expires_at,omitempty"`
}

type Planner struct {
	registry  *registry.Registry
	connected func(string) bool
}

func New(orchestratorRegistry *registry.Registry, connected func(string) bool) *Planner {
	return &Planner{registry: orchestratorRegistry, connected: connected}
}

func (p *Planner) Build(request Request, now time.Time) (Plan, error) {
	if request.TransferID == "" || request.Length <= 0 || request.Offset < 0 || len(request.Destinations) == 0 {
		return Plan{}, errors.New("transfer_id, positive length, non-negative offset, and destinations are required")
	}
	if len(request.Destinations) > 256 {
		return Plan{}, errors.New("a distribution route supports at most 256 destinations")
	}
	if err := validEndpoint(request.Source); err != nil {
		return Plan{}, fmt.Errorf("source: %w", err)
	}
	destinationIDs := make(map[string]struct{}, len(request.Destinations))
	for _, destination := range request.Destinations {
		if destination.DestinationID == "" {
			return Plan{}, errors.New("destination_id is required")
		}
		if _, exists := destinationIDs[destination.DestinationID]; exists {
			return Plan{}, fmt.Errorf("duplicate destination %s", destination.DestinationID)
		}
		destinationIDs[destination.DestinationID] = struct{}{}
		if err := validEndpoint(destination.Endpoint); err != nil {
			return Plan{}, fmt.Errorf("destination %s: %w", destination.DestinationID, err)
		}
	}
	if request.MaxObservationAge <= 0 {
		request.MaxObservationAge = 30 * time.Second
	}
	observations := make(map[string]orchestratordomain.WorkerObservation)
	required := request.Resources
	if required.CPUMillis == 0 {
		required.CPUMillis = 100
	}
	if required.MemoryBytes == 0 {
		required.MemoryBytes = 4 << 20
	}
	if required.BandwidthMbps == 0 {
		required.BandwidthMbps = 10
	}
	for _, observation := range p.registry.Observations() {
		membership, ok := p.registry.Membership(observation.WorkerID)
		if !ok || membership.Status != "active" || observation.Status != "active" ||
			membership.Validate(now) != nil || membership.NodeID != observation.NodeID || !observation.Available.Fits(required) ||
			observation.CircuitEndpoint == "" || now.Sub(observation.ObservedAt) > request.MaxObservationAge ||
			!slices.Contains(observation.Capabilities, "transfer.distribute") ||
			(p.connected != nil && !p.connected(observation.WorkerID)) {
			continue
		}
		observations[observation.WorkerID] = observation
	}
	if len(observations) == 0 {
		return Plan{}, ErrNoRoute
	}
	workers := make([]string, 0, len(observations))
	for workerID := range observations {
		workers = append(workers, workerID)
	}
	sort.Strings(workers)
	links := indexLinks(request.Links, request.MaxObservationAge, now)

	bestScore := math.Inf(1)
	bestRoot := ""
	var bestDistances map[string]float64
	var bestParents map[string]string
	var bestEgress map[string]string
	for _, root := range workers {
		distances, parents := shortestPaths(root, workers, observations, links, request.Length)
		sourceCost := accessCost(findAccess(request.SourceAccess, root), observations[root], request.Length)
		egress := make(map[string]string, len(request.Destinations))
		egressLoad := make(map[string]int, len(workers))
		completion := sourceCost
		feasible := true
		destinations := append([]Destination(nil), request.Destinations...)
		sort.Slice(destinations, func(i, j int) bool { return destinations[i].DestinationID < destinations[j].DestinationID })
		for _, destination := range destinations {
			worker, cost := bestDestinationEgress(destination, workers, observations, distances, egressLoad, request.Length)
			if worker == "" {
				feasible = false
				break
			}
			egress[destination.DestinationID] = worker
			egressLoad[worker]++
			completion = max(completion, sourceCost+cost)
		}
		if feasible && (completion < bestScore || (completion == bestScore && root < bestRoot)) {
			bestScore, bestRoot, bestDistances, bestParents, bestEgress = completion, root, distances, parents, egress
		}
	}
	if bestRoot == "" {
		return Plan{}, ErrNoRoute
	}

	used := map[string]bool{bestRoot: true}
	for _, egress := range bestEgress {
		for current := egress; current != "" && !used[current]; current = bestParents[current] {
			used[current] = true
		}
	}
	nodes := make(map[string]*Node, len(used))
	for _, workerID := range workers {
		if !used[workerID] {
			continue
		}
		observation := observations[workerID]
		nodes[workerID] = &Node{
			WorkerID: workerID, NodeID: observation.NodeID, Endpoint: observation.CircuitEndpoint,
			ArrivalMillis: bestDistances[workerID],
		}
	}
	for workerID, node := range nodes {
		if workerID == bestRoot {
			continue
		}
		parent := bestParents[workerID]
		if nodes[parent] == nil {
			return Plan{}, fmt.Errorf("%w: disconnected route for %s", ErrNoRoute, workerID)
		}
		node.ParentWorkerID = parent
		nodes[parent].Children = append(nodes[parent].Children, workerID)
	}
	for _, destination := range request.Destinations {
		workerID := bestEgress[destination.DestinationID]
		nodes[workerID].Destinations = append(nodes[workerID].Destinations, contracts.DistributionDestination{
			DestinationID: destination.DestinationID, Endpoint: destination.Endpoint,
		})
	}
	result := Plan{
		TransferID: request.TransferID, WorkloadID: "distribution_" + request.TransferID,
		RootWorkerID: bestRoot, EstimatedCompletionMillis: bestScore,
	}
	if len(nodes) > 1 {
		result.CircuitID = "distribution_" + request.TransferID
	}
	for _, workerID := range workers {
		if node := nodes[workerID]; node != nil {
			sort.Strings(node.Children)
			sort.Slice(node.Destinations, func(i, j int) bool { return node.Destinations[i].DestinationID < node.Destinations[j].DestinationID })
			result.Nodes = append(result.Nodes, *node)
		}
	}
	return result, nil
}

func validEndpoint(endpoint contracts.HTTPEndpoint) error {
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("endpoint must be an absolute HTTP(S) URL")
	}
	return nil
}

func shortestPaths(root string, workers []string, observations map[string]orchestratordomain.WorkerObservation, links map[string]LinkMetric, length int64) (map[string]float64, map[string]string) {
	distance := make(map[string]float64, len(workers))
	parents := make(map[string]string, len(workers))
	visited := make(map[string]bool, len(workers))
	for _, worker := range workers {
		distance[worker] = math.Inf(1)
	}
	distance[root] = 0
	for range workers {
		current := ""
		for _, worker := range workers {
			if !visited[worker] && (current == "" || distance[worker] < distance[current]) {
				current = worker
			}
		}
		if current == "" || math.IsInf(distance[current], 1) {
			break
		}
		visited[current] = true
		for _, target := range workers {
			if target == current || visited[target] {
				continue
			}
			cost := linkCost(current, target, observations, links, length)
			candidate := distance[current] + cost
			if candidate < distance[target] || (candidate == distance[target] && current < parents[target]) {
				distance[target], parents[target] = candidate, current
			}
		}
	}
	return distance, parents
}

func linkCost(from, to string, observations map[string]orchestratordomain.WorkerObservation, links map[string]LinkMetric, length int64) float64 {
	if metric, ok := links[from+"\x00"+to]; ok {
		return metric.RTTMillis + transferMillis(length, metric.ThroughputMbps)*(1+metric.LossPPM/1_000_000)
	}
	throughput := float64(min(observations[from].Available.BandwidthMbps, observations[to].Available.BandwidthMbps))
	rtt := 50.0
	if observations[from].Region != "" && observations[from].Region == observations[to].Region {
		rtt = 5
	}
	return rtt + transferMillis(length, throughput)
}

func bestDestinationEgress(destination Destination, workers []string, observations map[string]orchestratordomain.WorkerObservation, distances map[string]float64, load map[string]int, length int64) (string, float64) {
	bestWorker, best := "", math.Inf(1)
	for _, worker := range workers {
		metric, found := findAccessOK(destination.Access, worker)
		if len(destination.Access) > 0 && !found {
			continue
		}
		observation := observations[worker]
		share := float64(observation.Available.BandwidthMbps) / float64(load[worker]+1)
		if metric.ThroughputMbps <= 0 || metric.ThroughputMbps > share {
			metric.ThroughputMbps = share
		}
		cost := distances[worker] + accessCost(metric, observation, length)
		if cost < best || (cost == best && worker < bestWorker) {
			bestWorker, best = worker, cost
		}
	}
	return bestWorker, best
}

func accessCost(metric AccessMetric, observation orchestratordomain.WorkerObservation, length int64) float64 {
	throughput := metric.ThroughputMbps
	capacity := float64(observation.Available.BandwidthMbps)
	if throughput <= 0 || throughput > capacity {
		throughput = capacity
	}
	rtt := metric.RTTMillis
	if rtt <= 0 {
		rtt = 25
	}
	return rtt + transferMillis(length, throughput)
}

func transferMillis(bytes int64, mbps float64) float64 {
	if mbps <= 0 {
		return math.Inf(1)
	}
	return float64(bytes) * 8 / (mbps * 1000)
}

func indexLinks(metrics []LinkMetric, maxAge time.Duration, now time.Time) map[string]LinkMetric {
	result := make(map[string]LinkMetric)
	for _, metric := range metrics {
		if metric.FromWorkerID == "" || metric.ToWorkerID == "" || metric.RTTMillis < 0 || metric.ThroughputMbps <= 0 ||
			(!metric.ObservedAt.IsZero() && now.Sub(metric.ObservedAt) > maxAge) {
			continue
		}
		result[metric.FromWorkerID+"\x00"+metric.ToWorkerID] = metric
	}
	return result
}

func findAccess(metrics []AccessMetric, workerID string) AccessMetric {
	metric, _ := findAccessOK(metrics, workerID)
	return metric
}

func findAccessOK(metrics []AccessMetric, workerID string) (AccessMetric, bool) {
	for _, metric := range metrics {
		if metric.WorkerID == workerID {
			return metric, true
		}
	}
	return AccessMetric{}, false
}
