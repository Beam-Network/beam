package media

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/worker/mediafleet"
	"github.com/Beam-Network/beam/internal/worker/subsystem"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	roomhandler "github.com/Beam-Network/beam/internal/workload/handlers/room"
	roommediahandler "github.com/Beam-Network/beam/internal/workload/handlers/roommedia"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/transcode"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

type Plane struct {
	Handler runtime.Handler
	Fleet   *mediafleet.Fleet
	media   *roommediahandler.Handler
}

func (p *Plane) Close() error {
	if p == nil || p.media == nil {
		return nil
	}
	return p.media.Close()
}

func Build(config Config) (*Plane, error) {
	fleetConfig, err := config.FleetConfig()
	if err != nil {
		return nil, err
	}
	plane := &Plane{}
	if fleetConfig.Enabled() {
		if err := PrepareCacheDir(fleetConfig.CacheDir); err != nil {
			return nil, fmt.Errorf("--media-cache-dir: %v", err)
		}
		fleet, err := mediafleet.New(context.Background(), fleetConfig)
		if err != nil {
			return nil, fmt.Errorf("media roles: %v", err)
		}
		if fleetConfig.PrivateIP() {
			log.Printf("warning: --media-public-ip %s is not publicly routable; only clients on this network reach the media roles", config.PublicIP)
		}
		mediafleet.SetDefault(fleet)
		log.Printf("media roles %s in %s, declared egress %d Mbps", strings.Join(fleetConfig.Roles, ","), fleetConfig.Region, fleetConfig.EgressMbps)
		plane.Fleet = fleet
		if fleetConfig.Has(mediacontracts.RoleTranscode) {
			removeTranscoderLeftovers(config.WorkerID, fleet)
		}
	}
	plane.media = roommediahandler.NewHandler(withMediaFleet(roommediahandler.Config{ListenAddress: config.ListenAddress,
		AdvertiseURL: config.AdvertiseURL, PublicIP: config.PublicIP, UDPPortMin: config.UDPPortMin,
		UDPPortMax: config.UDPPortMax, ICEServers: config.ICEServers, TURNSecret: config.TURNSecret,
		TURNCredentialTTL: config.TURNCredentialTTL, TURNUsernamePrefix: config.TURNUsernamePrefix,
		MaxViewers: config.MaxViewers, PublisherReconnectGrace: config.PublisherReconnectGrace}, plane.Fleet))
	if config.WebRTC {
		if err := plane.media.PrepareListener(); err != nil {
			_ = plane.Close()
			return nil, err
		}
	}
	if plane.Fleet != nil {
		if err := plane.media.PrepareMediaListener(); err != nil {
			_ = plane.Close()
			return nil, err
		}
	}
	if fleetConfig.Has(mediacontracts.RoleSFU) || fleetConfig.Has(mediacontracts.RoleRelay) {
		if err := plane.media.PrepareRealtime(); err != nil {
			log.Printf("warning: media roles: the WebRTC UDP mux did not bind at start (%v); sfu and relay nodes bind it on first use", err)
		}
	}
	var legacy runtime.Handler = roomhandler.NewHandler(roomhandler.Config{AllowPublicListeners: config.AllowPublicListeners})
	if config.LegacyProcess {
		isolated, err := subsystem.NewProcessHandler(legacy, config.legacySubsystem())
		if err != nil {
			_ = plane.Close()
			return nil, err
		}
		legacy = isolated
	}
	plane.Handler = Dispatch{Worker: plane.media, Legacy: legacy}
	return plane, nil
}

const leftoverSettleTimeout = 30 * time.Second

func removeTranscoderLeftovers(workerID string, fleet *mediafleet.Fleet) {
	transcoder := broadcast.ConfigFromEnv(workerID).Transcode
	devices, all := fleet.EncoderGPUs()
	log.Printf("media.transcode GPUs: %s", describeEncoderGPUs(devices, all, fleet.GPULockDirectory()))
	settled := transcode.StartLeftoverRemoval(context.Background(), transcoder, log.Printf)
	select {
	case <-settled:
	case <-time.After(leftoverSettleTimeout):
		log.Printf("media.transcode: the removal of the transcoder containers a previous media plane left is still running; GPU encoding waits for it")
	}
}

func describeEncoderGPUs(devices []mediafleet.EncoderGPU, all bool, lockDirectory string) string {
	switch {
	case all:
		return "every GPU (devices unknown without nvidia-smi), no GPU lock"
	case len(devices) == 0:
		return "none (no NVENC: source and audio-only ladders only)"
	}
	parts := make([]string, len(devices))
	for i, device := range devices {
		parts[i] = fmt.Sprintf("%d (%s, %d NVENC sessions)", device.Index, device.Model, device.Sessions)
	}
	locks := "locked shared in " + lockDirectory
	if lockDirectory == "" {
		locks = "without GPU locks"
	}
	return strings.Join(parts, ", ") + ", " + locks
}

func (c Config) legacySubsystem() subsystem.Config {
	return subsystem.Config{MediaListenAddress: c.ListenAddress, MediaAdvertiseURL: c.AdvertiseURL, MediaPublicIP: c.PublicIP,
		MediaUDPPortMin: c.UDPPortMin, MediaUDPPortMax: c.UDPPortMax, MediaICEServers: c.ICEServers,
		MediaTURNSecret: c.TURNSecret, MediaTURNCredentialTTL: c.TURNCredentialTTL,
		MediaTURNUsernamePrefix: c.TURNUsernamePrefix, MediaMaxViewers: c.MaxViewers,
		AllowPublicListeners: c.AllowPublicListeners}.ForKind(domain.KindRoomMedia)
}

func PrepareCacheDir(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := broadcast.RemovePackageStores(dir); err != nil {
		return fmt.Errorf("remove the DVR stores of an earlier process: %w", err)
	}
	return nil
}
