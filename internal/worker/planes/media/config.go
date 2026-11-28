package media

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/worker/mediafleet"
)

type Config struct {
	WorkerID                string        `json:"worker_id"`
	StateDir                string        `json:"state_dir"`
	WebRTC                  bool          `json:"webrtc,omitempty"`
	ListenAddress           string        `json:"listen_address,omitempty"`
	AdvertiseURL            string        `json:"advertise_url,omitempty"`
	PublicIP                string        `json:"public_ip,omitempty"`
	UDPPortMin              uint16        `json:"udp_port_min,omitempty"`
	UDPPortMax              uint16        `json:"udp_port_max,omitempty"`
	ICEServers              []string      `json:"ice_servers,omitempty"`
	TURNSecret              string        `json:"turn_secret,omitempty"`
	TURNCredentialTTL       time.Duration `json:"turn_credential_ttl,omitempty"`
	TURNUsernamePrefix      string        `json:"turn_username_prefix,omitempty"`
	MaxViewers              int           `json:"max_viewers,omitempty"`
	PublisherReconnectGrace time.Duration `json:"publisher_reconnect_grace,omitempty"`
	AllowPublicListeners    bool          `json:"allow_public_listeners,omitempty"`
	LegacyProcess           bool          `json:"legacy_process,omitempty"`
	Roles                   Roles         `json:"roles"`
	SeatGPUs                int64         `json:"seat_gpus,omitempty"`
	GPULockDirectory        string        `json:"gpu_lock_directory,omitempty"`
}

type Roles struct {
	Roles                string `json:"roles,omitempty"`
	Region               string `json:"region,omitempty"`
	EgressMbps           int64  `json:"egress_mbps,omitempty"`
	IngressMbps          int64  `json:"ingress_mbps,omitempty"`
	SelfTest             bool   `json:"self_test,omitempty"`
	SelfTestURL          string `json:"self_test_url,omitempty"`
	HTTPSAddr            string `json:"https_addr,omitempty"`
	RTMPPort             int    `json:"rtmp_port,omitempty"`
	RTMPSPort            int    `json:"rtmps_port,omitempty"`
	SRTPort              int    `json:"srt_port,omitempty"`
	CacheDir             string `json:"cache_dir,omitempty"`
	CacheBytes           int64  `json:"cache_bytes,omitempty"`
	CacheMemoryBytes     int64  `json:"cache_memory_bytes,omitempty"`
	GPUEncoderSessions   int    `json:"gpu_encoder_sessions"`
	GPUs                 string `json:"gpus,omitempty"`
	GPUShare             bool   `json:"gpu_share,omitempty"`
	EdgeDomain           string `json:"edge_domain,omitempty"`
	ProbeTrustedCIDRs    string `json:"probe_trusted_cidrs,omitempty"`
	RecordingCheckpointS int    `json:"recording_checkpoint_s,omitempty"`
}

func (c Config) FleetConfig() (mediafleet.Config, error) {
	roles, err := mediafleet.ParseRoles(c.Roles.Roles)
	if err != nil {
		return mediafleet.Config{}, err
	}
	if len(roles) == 0 {
		return mediafleet.Config{}, nil
	}
	trusted, err := mediafleet.ParseProbeCIDRs(c.Roles.ProbeTrustedCIDRs)
	if err != nil {
		return mediafleet.Config{}, fmt.Errorf("--media-probe-trusted-cidrs: %v", err)
	}
	gpus, err := mediafleet.ParseGPUs(c.Roles.GPUs)
	if err != nil {
		return mediafleet.Config{}, err
	}
	flags := c.Roles
	return mediafleet.Config{GPUs: gpus, GPUShare: flags.GPUShare, SeatGPUs: c.SeatGPUs, GPULockDirectory: c.GPULockDirectory,
		WorkerID: c.WorkerID, Roles: roles, Region: strings.TrimSpace(flags.Region),
		EgressMbps: flags.EgressMbps, IngressMbps: flags.IngressMbps, SelfTest: flags.SelfTest, SelfTestURL: flags.SelfTestURL,
		HTTPSAddr: flags.HTTPSAddr, RTMPPort: flags.RTMPPort, RTMPSPort: flags.RTMPSPort, SRTPort: flags.SRTPort,
		CacheDir: strings.TrimSpace(flags.CacheDir), CacheBytes: flags.CacheBytes, CacheMemoryBytes: flags.CacheMemoryBytes,
		GPUEncoderSessions: flags.GPUEncoderSessions, PublicIP: c.PublicIP, UDPPortMin: c.UDPPortMin, UDPPortMax: c.UDPPortMax,
		EdgeDomain: strings.ToLower(strings.TrimSpace(flags.EdgeDomain)), StateDir: c.StateDir, ProbeTrustedCIDRs: trusted,
		RecordingCheckpointS: flags.RecordingCheckpointS}, nil
}

func (c Config) Check() error {
	fleet, err := c.FleetConfig()
	if err != nil {
		return err
	}
	if err := fleet.Validate(); err != nil {
		return fmt.Errorf("media roles: %v", err)
	}
	return nil
}

func (c Config) CheckGPUs(ctx context.Context) (warnings []string, err error) {
	fleet, err := c.FleetConfig()
	if err != nil || !fleet.Enabled() {
		return nil, err
	}
	_, warning, err := mediafleet.CheckGPU(ctx, fleet)
	if err != nil {
		return nil, fmt.Errorf("media roles: %v", err)
	}
	if warning != "" {
		warnings = append(warnings, warning)
	}
	return warnings, nil
}

func (c Config) RoleCapabilities() []string {
	fleet, err := c.FleetConfig()
	if err != nil {
		return nil
	}
	return fleet.Capabilities()
}

func (c Config) Hosted() bool { return c.WebRTC || len(c.RoleCapabilities()) > 0 }
