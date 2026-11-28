package mediafleet

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type Config struct {
	WorkerID                     string
	Roles                        []string
	Region                       string
	EgressMbps                   int64
	IngressMbps                  int64
	SelfTest                     bool
	SelfTestURL                  string
	HTTPSAddr                    string
	RTMPPort, RTMPSPort, SRTPort int
	CacheDir                     string
	CacheBytes                   int64
	CacheMemoryBytes             int64
	GPUEncoderSessions           int
	GPUs                         GPUSelection
	GPUShare                     bool
	SeatGPUs                     int64
	GPULockDirectory             string
	PublicIP                     string
	UDPPortMin                   uint16
	UDPPortMax                   uint16
	EdgeDomain                   string
	StateDir                     string
	ProbeTrustedCIDRs            []*net.IPNet
	RecordingCheckpointS         int
}

const RecordingCheckpointDefaultS = mediacontracts.RecordingCheckpointDefaultS

func ParseRoles(value string) ([]string, error) {
	var roles []string
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !strings.HasPrefix(entry, "media.") {
			entry = "media." + entry
		}
		if !slices.Contains(contracts.MediaWorkerRoles, entry) {
			return nil, fmt.Errorf("unknown media role %q: want %s", entry, strings.Join(contracts.MediaWorkerRoles, ", "))
		}
		if !slices.Contains(roles, entry) {
			roles = append(roles, entry)
		}
	}
	slices.SortFunc(roles, func(a, b string) int {
		return slices.Index(contracts.MediaWorkerRoles, a) - slices.Index(contracts.MediaWorkerRoles, b)
	})
	return roles, nil
}

func (c Config) Enabled() bool { return len(c.Roles) > 0 }

func (c Config) Has(nodeRole string) bool {
	return slices.Contains(c.Roles, mediacontracts.WorkerRoles[nodeRole])
}

func (c Config) Capabilities() []string {
	if !c.Enabled() {
		return nil
	}
	capabilities := []string{"room.media"}
	for _, role := range c.Roles {
		capabilities = append(capabilities, mediacontracts.RoleCapabilities[contracts.NodeRoleOfWorkerRole(role)])
	}
	return capabilities
}

func (c Config) HTTPSPort() (int, error) {
	address := strings.TrimSpace(c.HTTPSAddr)
	if address == "" {
		return 443, nil
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return 0, fmt.Errorf("invalid --media-https-addr %q: %w", c.HTTPSAddr, err)
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65_535 {
		return 0, fmt.Errorf("--media-https-addr %q needs a port between 1 and 65535", c.HTTPSAddr)
	}
	return value, nil
}

func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}
	var problems []string
	if !slices.Contains(contracts.MediaBroadRegions, c.Region) {
		problems = append(problems, fmt.Sprintf("--media-region must be one of %s", strings.Join(contracts.MediaBroadRegions, ", ")))
	}
	if c.EgressMbps <= 0 {
		problems = append(problems, "--media-egress-mbps must declare the egress bandwidth")
	}
	if c.IngressMbps < 0 {
		problems = append(problems, "--media-ingress-mbps cannot be negative")
	}
	if c.Has(mediacontracts.RoleIngest) && c.IngressMbps <= 0 {
		problems = append(problems, "media.ingest needs --media-ingress-mbps (the bandwidth encoders send in)")
	}
	if ip := net.ParseIP(strings.TrimSpace(c.PublicIP)); ip == nil || ip.IsUnspecified() {
		problems = append(problems, "--media-public-ip must be the Worker's public IP (every media role needs one; NAT Workers cannot take media roles yet)")
	}
	if (c.Has(mediacontracts.RoleSFU) || c.Has(mediacontracts.RoleRelay)) && (c.UDPPortMin == 0 || c.UDPPortMax < c.UDPPortMin) {
		problems = append(problems, "media.sfu and media.relay need --media-udp-port-min and --media-udp-port-max")
	}
	if _, err := c.HTTPSPort(); err != nil {
		problems = append(problems, err.Error())
	}
	if c.Has(mediacontracts.RoleIngest) {
		for name, port := range map[string]int{"--media-rtmp-port": c.RTMPPort, "--media-rtmps-port": c.RTMPSPort, "--media-srt-port": c.SRTPort} {
			if port < 1 || port > 65_535 {
				problems = append(problems, name+" must be between 1 and 65535")
			}
		}
	}
	if (c.Has(mediacontracts.RoleShield) || c.Has(mediacontracts.RoleEdge)) && (strings.TrimSpace(c.CacheDir) == "" || c.CacheBytes <= 0) {
		problems = append(problems, "media.shield and media.edge need --media-cache-dir and --media-cache-bytes")
	}
	if c.Has(mediacontracts.RolePackage) && strings.TrimSpace(c.CacheDir) == "" {
		problems = append(problems, "media.package needs --media-cache-dir (its DVR store keeps the sessions' dvr_window_s on disk)")
	}
	if c.CacheBytes < 0 || c.CacheMemoryBytes < 0 {
		problems = append(problems, "media cache sizes cannot be negative")
	}
	if c.RecordingCheckpointS != 0 && (c.RecordingCheckpointS < 60 || c.RecordingCheckpointS > 3600) {
		problems = append(problems, "--media-recording-checkpoint-s must be between 60 and 3600")
	}
	if c.EdgeDomain != "" && !mediacontracts.ValidHostname(c.EdgeDomain) {
		problems = append(problems, "BEAM_MEDIA_EDGE_DOMAIN must be a lowercase domain with two or more labels")
	}
	problems = append(problems, c.validateGPUs()...)
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func (c Config) PrivateIP() bool {
	ip := net.ParseIP(strings.TrimSpace(c.PublicIP))
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}
