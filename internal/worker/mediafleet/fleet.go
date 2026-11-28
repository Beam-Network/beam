package mediafleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/wcp"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const outboundQueue = 1024

const (
	loadSampleInterval    = 5 * time.Second
	encoderSampleInterval = 15 * time.Second
	cacheSampleInterval   = time.Minute
)

type TopologyUpdate struct {
	Wire     contracts.MediaTopologyUpdateWire
	Topology *mediacontracts.Topology
}

type measurement struct {
	mbps int64
	at   time.Time
}

type Fleet struct {
	config   Config
	outbound chan wcp.MediaControl
	host     hostSampler
	probes   probeLimiter
	certs    *Certificates

	mu           sync.RWMutex
	gpu          *GPUInfo
	egressTest   *measurement
	ingressTest  *measurement
	encoders     int64
	cacheUsed    int64
	nodeLoads    map[string]mediacontracts.NodeLoad
	hostOwners   map[string]map[string]bool
	watchers     map[string]map[uint64]chan TopologyUpdate
	nextWatcher  uint64
	heldTopology map[string]heldUpdate
	sources      []UsageSource

	coreEdgeDomain atomic.Pointer[string]
	announced      atomic.Pointer[[]string]
}

type UsageSource func() (encoderSessions, cacheUsedBytes int64)

func (f *Fleet) AddUsageSource(source UsageSource) {
	if f == nil || source == nil {
		return
	}
	f.mu.Lock()
	f.sources = append(f.sources, source)
	f.mu.Unlock()
}

var defaultFleet atomic.Pointer[Fleet]

func SetDefault(fleet *Fleet) { defaultFleet.Store(fleet) }

func Default() *Fleet { return defaultFleet.Load() }

func New(ctx context.Context, config Config) (*Fleet, error) {
	fleet := &Fleet{config: config, outbound: make(chan wcp.MediaControl, outboundQueue),
		nodeLoads: map[string]mediacontracts.NodeLoad{}, watchers: map[string]map[uint64]chan TopologyUpdate{},
		heldTopology: map[string]heldUpdate{},
		hostOwners:   map[string]map[string]bool{}}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	detected, probed := DetectGPUs(ctx)
	gpu, _, err := resolveGPU(config, detected, probed)
	if err != nil {
		return nil, err
	}
	if gpu != nil && gpu.MaxEncoderSessions > 0 {
		fleet.gpu = gpu
		if err := fleet.prepareGPULocks(); err != nil {
			return nil, err
		}
	}
	if config.Has(mediacontracts.RoleTranscode) && fleet.EncoderSessions() == 0 {
		log.Printf("media.transcode without NVENC: this Worker reports gpu: null and serves source and audio-only ladders only (no GPU encoding)")
	}
	certs, err := newCertificates(config.WorkerID, config.StateDir, fleet.send)
	if err != nil {
		return nil, err
	}
	fleet.certs = certs
	fleet.probes.trusted = config.ProbeTrustedCIDRs
	if domain := loadEdgeDomain(config.StateDir); domain != "" {
		fleet.coreEdgeDomain.Store(&domain)
	}
	fleet.loadFeatures()
	return fleet, nil
}

func (f *Fleet) Config() Config { return f.config }

func (f *Fleet) EncoderSessions() int64 {
	if f.gpu == nil {
		return 0
	}
	return f.gpu.MaxEncoderSessions
}

func (f *Fleet) Certificates() *Certificates { return f.certs }

func (f *Fleet) EdgeDomain() string {
	if f == nil {
		return ""
	}
	if domain := f.coreEdgeDomain.Load(); domain != nil {
		return *domain
	}
	return f.config.EdgeDomain
}

func (f *Fleet) EdgeHostname() string {
	domain := f.EdgeDomain()
	if domain == "" {
		return ""
	}
	host, err := mediacontracts.EdgeHostname(f.config.WorkerID, domain)
	if err != nil {
		return ""
	}
	return host
}

func (f *Fleet) applyCapacityAck(ack mediacontracts.MediaAck) error {
	if ack.Type != mediacontracts.CapacityReportAckType {
		return errors.New("invalid media_capacity_report_ack")
	}
	if !ack.Accepted {
		return nil
	}
	f.announce(ack.Features)
	previousHost := f.EdgeHostname()
	if ack.EdgeDomain == nil {
		if f.coreEdgeDomain.Swap(nil) != nil {
			log.Printf("media edge domain: Core sends none; using --media-edge-domain %q", f.config.EdgeDomain)
			saveEdgeDomain(f.config.StateDir, "")
		}
	} else {
		domain := strings.ToLower(strings.TrimSpace(*ack.EdgeDomain))
		if !mediacontracts.ValidHostname(domain) {
			return fmt.Errorf("invalid edge_domain %q from Core", *ack.EdgeDomain)
		}
		if current := f.coreEdgeDomain.Load(); current == nil || *current != domain {
			f.coreEdgeDomain.Store(&domain)
			saveEdgeDomain(f.config.StateDir, domain)
			if f.config.EdgeDomain != "" && f.config.EdgeDomain != domain {
				log.Printf("media edge domain: Core's %q overrides --media-edge-domain %q", domain, f.config.EdgeDomain)
			} else {
				log.Printf("media edge domain from Core: %q", domain)
			}
		}
	}
	host := f.EdgeHostname()
	var err error
	if host != previousHost && host != "" {
		if previousHost != "" {
			f.mu.RLock()
			owned := len(f.hostOwners[previousHost]) > 0
			f.mu.RUnlock()
			if !owned {
				f.certs.Forget(previousHost)
			}
		}
		err = f.certs.Ensure(host)
	}
	for _, hostname := range ack.CertificatesRequired {
		if requireErr := f.certs.Require(strings.ToLower(hostname)); requireErr != nil && err == nil {
			err = requireErr
		}
	}
	return err
}

const edgeDomainFile = "edge-domain"

func loadEdgeDomain(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(stateDir, edgeDomainFile))
	if err != nil {
		return ""
	}
	domain := strings.TrimSpace(string(data))
	if !mediacontracts.ValidHostname(domain) {
		return ""
	}
	return domain
}

func saveEdgeDomain(stateDir, domain string) {
	if strings.TrimSpace(stateDir) == "" {
		return
	}
	path := filepath.Join(stateDir, edgeDomainFile)
	if domain == "" {
		_ = os.Remove(path)
		return
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return
	}
	temporary := path + ".tmp"
	if os.WriteFile(temporary, []byte(domain), 0o600) == nil {
		_ = os.Rename(temporary, path)
	}
}

func (f *Fleet) Run(ctx context.Context) {
	switch {
	case f.config.SelfTest && strings.TrimSpace(f.config.SelfTestURL) != "":
		go f.runSelfTests(ctx)
	case f.config.SelfTest:
		log.Printf("media bandwidth self test off: no --media-self-test-url; only the declared bandwidth is reported")
	}
	go f.certs.run(ctx)
	f.host.sample(time.Now())
	loadTicker := time.NewTicker(loadSampleInterval)
	defer loadTicker.Stop()
	var lastEncoders, lastCache time.Time
	for {
		now := time.Now()
		if f.gpu != nil && now.Sub(lastEncoders) >= encoderSampleInterval {
			inUse := EncoderSessionsInUse(ctx, f.gpu.Devices)
			f.mu.Lock()
			f.encoders = inUse
			f.mu.Unlock()
			lastEncoders = now
		}
		if f.config.CacheDir != "" && now.Sub(lastCache) >= cacheSampleInterval {
			used := cacheUsedBytes(f.config.CacheDir)
			f.mu.Lock()
			f.cacheUsed = used
			f.mu.Unlock()
			lastCache = now
		}
		select {
		case <-ctx.Done():
			return
		case now := <-loadTicker.C:
			f.host.sample(now)
		}
	}
}

func (f *Fleet) runSelfTests(ctx context.Context) {
	for {
		result, err := RunSelfTest(ctx, f.config.SelfTestURL)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("media bandwidth self test failed: %v", err)
		} else {
			log.Printf("media bandwidth self test: egress %d Mbps, ingress %d Mbps", result.EgressMbps, result.IngressMbps)
			f.mu.Lock()
			if result.EgressMbps > 0 {
				f.egressTest = &measurement{mbps: result.EgressMbps, at: result.MeasuredAt}
			}
			if result.IngressMbps > 0 {
				f.ingressTest = &measurement{mbps: result.IngressMbps, at: result.MeasuredAt}
			}
			f.mu.Unlock()
		}
		wait := SelfTestInterval
		if err != nil {
			wait = 15 * time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (f *Fleet) Capacity() *mediacontracts.WorkerCapacity {
	if f == nil || !f.config.Enabled() {
		return nil
	}
	config := f.config
	f.mu.RLock()
	sources := append([]UsageSource(nil), f.sources...)
	f.mu.RUnlock()
	var sourceEncoders, sourceCache int64
	for _, source := range sources {
		encoders, cache := source()
		sourceEncoders, sourceCache = max(sourceEncoders, encoders), max(sourceCache, cache)
	}
	capacity := mediacontracts.WorkerCapacity{WorkerID: config.WorkerID, Roles: append([]string{}, config.Roles...)}
	if config.Region != "" {
		region := config.Region
		capacity.RegionClaim = &region
	}
	if ip := net.ParseIP(strings.TrimSpace(config.PublicIP)); ip != nil {
		text := ip.String()
		if ip.To4() != nil {
			capacity.PublicIPv4 = &text
		} else {
			capacity.PublicIPv6 = &text
		}
	}
	if config.UDPPortMin > 0 && config.UDPPortMax >= config.UDPPortMin {
		capacity.UDPPortRange = &mediacontracts.PortRange{Min: int64(config.UDPPortMin), Max: int64(config.UDPPortMax)}
	}
	httpsPort, _ := config.HTTPSPort()
	capacity.Ports.HTTPS = int64(httpsPort)
	if config.Has(mediacontracts.RoleIngest) {
		rtmp, rtmps, srt := int64(config.RTMPPort), int64(config.RTMPSPort), int64(config.SRTPort)
		capacity.Ports.RTMP, capacity.Ports.RTMPS, capacity.Ports.SRT = &rtmp, &rtmps, &srt
		capacity.Features = []string{mediacontracts.WorkerFeatureChannelIngest}
	}
	if config.Has(mediacontracts.RoleSFU) || config.Has(mediacontracts.RoleRelay) || config.Has(mediacontracts.RoleTranscode) {
		capacity.Features = append(capacity.Features, mediacontracts.WorkerFeatureMediaBots)
	}
	capacity.Features = append(capacity.Features, f.usageFeatures()...)
	capacity.Egress = mediacontracts.Bandwidth{DeclaredMbps: config.EgressMbps}
	capacity.Ingress = mediacontracts.Bandwidth{DeclaredMbps: config.IngressMbps}
	cpu, egress, ingress := f.host.values()
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.egressTest != nil {
		mbps, at := f.egressTest.mbps, f.egressTest.at.UnixMilli()
		capacity.Egress.MeasuredMbps, capacity.Egress.MeasuredAtMS = &mbps, &at
	}
	if f.ingressTest != nil {
		mbps, at := f.ingressTest.mbps, f.ingressTest.at.UnixMilli()
		capacity.Ingress.MeasuredMbps, capacity.Ingress.MeasuredAtMS = &mbps, &at
	}
	if f.gpu != nil && f.gpu.MaxEncoderSessions > 0 {
		capacity.GPU = &mediacontracts.GPUEncoder{Vendor: truncate(f.gpu.Vendor, 64), Model: truncate(f.gpu.Model, 128),
			Encoder: "nvenc", MaxEncoderSessions: f.gpu.MaxEncoderSessions}
	}
	if config.CacheBytes > 0 || config.CacheMemoryBytes > 0 {
		capacity.Cache = &mediacontracts.CacheCapacity{DiskBytes: config.CacheBytes, MemoryBytes: config.CacheMemoryBytes}
	}
	capacity.Load = mediacontracts.WorkerLoad{CPURatio: cpu, EgressMbps: egress, IngressMbps: ingress,
		Sessions: int64(len(f.nodeLoads)), EncoderSessions: f.encoders, CacheUsedBytes: f.cacheUsed}
	capacity.Load.EncoderSessions = max(capacity.Load.EncoderSessions, sourceEncoders)
	if capacity.GPU == nil {
		capacity.Load.EncoderSessions = 0
	}
	capacity.Load.CacheUsedBytes = max(capacity.Load.CacheUsedBytes, sourceCache)
	var nodeEgress, nodeIngress int64
	for _, load := range f.nodeLoads {
		capacity.Load.Subscribers += load.Subscribers
		nodeEgress += load.EgressBPS
		nodeIngress += load.IngressBPS
	}
	capacity.Load.EgressMbps = max(capacity.Load.EgressMbps, nodeEgress/1_000_000)
	capacity.Load.IngressMbps = max(capacity.Load.IngressMbps, nodeIngress/1_000_000)
	if host := f.edgeHostnameLocked(); host != "" {
		capacity.EdgeHostname = &host
	}
	if f.certs != nil {
		capacity.Certificates = f.certs.Served()
	}
	return &capacity
}

func (f *Fleet) edgeHostnameLocked() string { return f.EdgeHostname() }

func (f *Fleet) ReportNodeLoad(workloadID string, load mediacontracts.NodeLoad) {
	if f == nil || workloadID == "" {
		return
	}
	f.mu.Lock()
	f.nodeLoads[workloadID] = load
	f.mu.Unlock()
}

func (f *Fleet) ForgetNode(workloadID string) {
	if f == nil {
		return
	}
	edgeHost := f.EdgeHostname()
	var released []string
	f.mu.Lock()
	delete(f.nodeLoads, workloadID)
	for _, watcher := range f.watchers[workloadID] {
		close(watcher)
	}
	delete(f.watchers, workloadID)
	delete(f.heldTopology, workloadID)
	for host, owners := range f.hostOwners {
		if !owners[workloadID] {
			continue
		}
		delete(owners, workloadID)
		if len(owners) == 0 {
			delete(f.hostOwners, host)
			if host != edgeHost {
				released = append(released, host)
			}
		}
	}
	f.mu.Unlock()
	for _, host := range released {
		f.certs.Forget(host)
	}
}

func (f *Fleet) EnsureHost(workloadID, hostname string) error {
	if f == nil {
		return errors.New("media roles are not enabled")
	}
	if err := f.certs.Ensure(hostname); err != nil {
		return err
	}
	f.mu.Lock()
	if f.hostOwners[hostname] == nil {
		f.hostOwners[hostname] = map[string]bool{}
	}
	f.hostOwners[hostname][workloadID] = true
	f.mu.Unlock()
	return nil
}

func (f *Fleet) WatchTopology(workloadID string) (<-chan TopologyUpdate, func()) {
	updates := make(chan TopologyUpdate, 1)
	if f == nil {
		close(updates)
		return updates, func() {}
	}
	f.mu.Lock()
	f.nextWatcher++
	id := f.nextWatcher
	if f.watchers[workloadID] == nil {
		f.watchers[workloadID] = map[uint64]chan TopologyUpdate{}
	}
	f.watchers[workloadID][id] = updates
	if held, ok := f.heldTopology[workloadID]; ok {
		delete(f.heldTopology, workloadID)
		if time.Since(held.at) < heldTopologyTTL {
			offerLatest(updates, held.update)
		}
	}
	f.mu.Unlock()
	return updates, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if watcher, ok := f.watchers[workloadID][id]; ok {
			delete(f.watchers[workloadID], id)
			close(watcher)
		}
	}
}

func (f *Fleet) SendSessionEvent(event mediacontracts.SessionEvent) error {
	event.Type, event.SchemaVersion = mediacontracts.SessionEventType, mediacontracts.SessionEventSchema
	if _, err := mediacontracts.DecodeSessionEventBody(event.Event); err != nil {
		return fmt.Errorf("invalid media session event: %w", err)
	}
	return f.sendValue(mediacontracts.SessionEventType, event)
}

func (f *Fleet) SendSessionEventWait(ctx context.Context, event mediacontracts.SessionEvent, timeout time.Duration) error {
	if f == nil {
		return errors.New("media roles are not enabled")
	}
	event.Type, event.SchemaVersion = mediacontracts.SessionEventType, mediacontracts.SessionEventSchema
	if _, err := mediacontracts.DecodeSessionEventBody(event.Event); err != nil {
		return fmt.Errorf("invalid media session event: %w", err)
	}
	encoded, err := mediacontracts.MarshalWire(event)
	if err != nil {
		return err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case f.outbound <- wcp.MediaControl{Type: mediacontracts.SessionEventType, Payload: encoded}:
		return nil
	case <-timer.C:
		return errors.New("media control queue stayed full: the Orchestrator is unreachable")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *Fleet) SendUsageReport(message mediacontracts.UsageReportMessage) error {
	message.Type, message.SchemaVersion = mediacontracts.UsageReportType, message.Report.Schema
	if message.Report.Schema != mediacontracts.UsageReportSchema && message.Report.Schema != mediacontracts.UsageReportV2Schema {
		return errors.New("a usage report is a beam-media-usage/v1 or v2 envelope")
	}
	return f.sendValue(mediacontracts.UsageReportType, message)
}

func (f *Fleet) sendValue(messageType string, value any) error {
	encoded, err := mediacontracts.MarshalWire(value)
	if err != nil {
		return err
	}
	return f.send(messageType, encoded)
}

func (f *Fleet) send(messageType string, payload json.RawMessage) error {
	if f == nil {
		return errors.New("media roles are not enabled")
	}
	select {
	case f.outbound <- wcp.MediaControl{Type: messageType, Payload: payload}:
		return nil
	default:
		return errors.New("media control queue is full: the Orchestrator is unreachable")
	}
}

func (f *Fleet) Outbound() <-chan wcp.MediaControl { return f.outbound }

func (f *Fleet) Deliver(message wcp.MediaControl) error {
	switch message.Type {
	case mediacontracts.TopologyUpdateType:
		wire, topology, err := contracts.DecodeMediaTopologyUpdate(message.Payload)
		if err != nil {
			return err
		}
		update := TopologyUpdate{Wire: wire, Topology: topology}
		f.mu.Lock()
		watchers := len(f.watchers[wire.WorkloadID])
		for _, watcher := range f.watchers[wire.WorkloadID] {
			offerLatest(watcher, update)
		}
		if watchers == 0 {
			f.holdTopologyLocked(wire.WorkloadID, update)
		}
		f.mu.Unlock()
		if node, ok := contracts.NodeEntry(topology, topology.View.NodeID); ok && node.Hostname != nil &&
			node.WorkerID != nil && *node.WorkerID == f.config.WorkerID && node.Role != mediacontracts.RoleTranscode {
			if err := f.EnsureHost(wire.WorkloadID, *node.Hostname); err != nil {
				log.Printf("media certificate for %s: %v", *node.Hostname, err)
			}
		}
		return nil
	case mediacontracts.CapacityReportAckType:
		var ack mediacontracts.MediaAck
		if err := json.Unmarshal(message.Payload, &ack); err != nil {
			return fmt.Errorf("invalid media_capacity_report_ack: %w", err)
		}
		return f.applyCapacityAck(ack)
	case mediacontracts.CertResultType:
		var result mediacontracts.CertResult
		if err := mediacontracts.UnmarshalStrict(message.Payload, &result); err != nil {
			return fmt.Errorf("invalid media_cert_result: %w", err)
		}
		return f.certs.handleResult(result)
	default:
		return fmt.Errorf("unsupported media control %q", message.Type)
	}
}

func offerLatest(watcher chan TopologyUpdate, update TopologyUpdate) {
	select {
	case watcher <- update:
		return
	default:
	}
	select {
	case <-watcher:
	default:
	}
	select {
	case watcher <- update:
	default:
	}
}

func truncate(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

var _ wcp.MediaBinder = (*Fleet)(nil)

const (
	heldTopologyTTL = 2 * time.Minute
	heldTopologyMax = 256
)

type heldUpdate struct {
	update TopologyUpdate
	at     time.Time
}

func (f *Fleet) holdTopologyLocked(workloadID string, update TopologyUpdate) {
	now := time.Now()
	for id, held := range f.heldTopology {
		if now.Sub(held.at) >= heldTopologyTTL {
			delete(f.heldTopology, id)
		}
	}
	if held, ok := f.heldTopology[workloadID]; ok && held.update.Topology != nil && update.Topology != nil &&
		(held.update.Topology.Epoch > update.Topology.Epoch ||
			held.update.Topology.Epoch == update.Topology.Epoch && held.update.Topology.Revision >= update.Topology.Revision) {
		return
	}
	if _, ok := f.heldTopology[workloadID]; !ok && len(f.heldTopology) >= heldTopologyMax {
		oldestID, oldest := "", now
		for id, held := range f.heldTopology {
			if held.at.Before(oldest) {
				oldestID, oldest = id, held.at
			}
		}
		delete(f.heldTopology, oldestID)
	}
	f.heldTopology[workloadID] = heldUpdate{update: update, at: now}
}
