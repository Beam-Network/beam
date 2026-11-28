package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/wcp"
	"github.com/Beam-Network/beam/internal/orchestrator/connectors"
	"github.com/Beam-Network/beam/internal/orchestrator/registry"
	"github.com/Beam-Network/beam/internal/orchestrator/roomworkloads"
	orchestratorserver "github.com/Beam-Network/beam/internal/orchestrator/server"
)

type mediaFlags struct {
	roleLimits     *string
	headroom       *float64
	reportInterval *time.Duration
}

func registerMediaFlags(flags *flag.FlagSet) mediaFlags {
	headroom := 0.8
	if value, err := strconv.ParseFloat(os.Getenv("BEAM_MEDIA_HEADROOM"), 64); err == nil {
		headroom = value
	}
	interval := 30 * time.Second
	if value, err := time.ParseDuration(os.Getenv("BEAM_MEDIA_CAPACITY_REPORT_INTERVAL")); err == nil {
		interval = value
	}
	return mediaFlags{
		roleLimits: flags.String("media-role-limits", os.Getenv("BEAM_MEDIA_ROLE_LIMITS"),
			"most media nodes of a role on one Worker, <role>=<count> comma-separated (sfu, relay, ingest, transcode, package, shield, edge); roles left out keep their default (sfu=16, relay=32, ingest=8, transcode=4, package=8, shield=64, edge=64), 0 removes the cap"),
		headroom: flags.Float64("media-headroom", headroom,
			"share of a Worker's measured bandwidth media placement fills (0 < value <= 1)"),
		reportInterval: flags.Duration("media-capacity-report-interval", interval,
			"period of media_capacity_report to BeamCore; role changes are sent within 5 s"),
	}
}

type mediaWorkers struct{ server *wcp.Server }

func (w mediaWorkers) SendMediaControl(workerID, messageType string, payload json.RawMessage) error {
	return w.server.SendMediaControl(workerID, wcp.MediaControl{Type: messageType, Payload: payload})
}

func (w mediaWorkers) MediaCapable(workerID string) bool { return w.server.MediaCapable(workerID) }

func wireMedia(flags mediaFlags, manager *roomworkloads.Manager, wcpServer *wcp.Server, server *orchestratorserver.Server,
	orchestratorRegistry *registry.Registry, stateDirectory string) {
	policy := roomworkloads.DefaultMediaPolicy()
	limits, err := roomworkloads.ParseMediaRoleLimits(*flags.roleLimits, policy.MaxNodesPerRole)
	if err != nil {
		log.Fatal(err)
	}
	if *flags.headroom <= 0 || *flags.headroom > 1 {
		log.Fatal("--media-headroom must be in (0, 1]")
	}
	if *flags.reportInterval < 5*time.Second {
		log.Fatal("--media-capacity-report-interval must be at least 5s")
	}
	policy.MaxNodesPerRole, policy.HeadroomRatio = limits, *flags.headroom
	connectors.MediaCapacityReportInterval = *flags.reportInterval
	if err := manager.ConfigureMedia(policy, filepath.Join(stateDirectory, "media-drain.json")); err != nil {
		log.Fatal(err)
	}
	manager.RegisterMediaWorkers(mediaWorkers{server: wcpServer})
	wcpServer.SetMediaControlSink(func(event wcp.MediaControlEvent) {
		if err := manager.HandleWorkerMediaControl(event.WorkerID, event.Message.Type, event.Message.Payload); err != nil {
			log.Printf("refuse Worker media control worker_id=%s type=%s: %v", event.WorkerID, event.Message.Type, err)
		}
	})
	server.AttachMedia(manager.MediaHandler(orchestratorRegistry.Hotkey))
}
