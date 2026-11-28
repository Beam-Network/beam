package roommedia

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/worker/mediafleet"
	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/edge"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/ingest"
	packager "github.com/Beam-Network/beam/internal/workload/handlers/roommedia/package"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/transcode"
)

func init() {
	for _, profile := range []string{mc.ProfileIngestV1, mc.ProfileTranscodeV1, mc.ProfilePackageLLHLSV1, mc.ProfileEdgeHTTPV1} {
		RegisterNodeRole(profile, runBroadcastNode)
		RegisterNodeValidator(profile, validateBroadcastNode)
	}
}

func validateBroadcastNode(spec domain.Spec) error {
	decoded, err := broadcast.DecodeSpec(spec, time.Now().UTC())
	if err != nil {
		return err
	}
	if decoded.Details.Role == mc.RoleTranscode {
		if err := transcode.Validate(decoded.Topology); err != nil {
			return err
		}
		if fleet := mediafleet.Default(); fleet != nil && fleet.EncoderSessions() == 0 && mc.LadderNeedsEncoder(decoded.Topology.Ladder) {
			return errors.New("this Worker has no NVENC encoder: its media.transcode role serves source and audio-only ladders only")
		}
	}
	return nil
}

var broadcastRouter = &broadcast.Router{}

var broadcastMounts sync.Map

var broadcastFamilies = []string{"/v1/ingest/", "/v1/pull/", "/v1/hls/"}

func mountBroadcastRouter(listener *MediaListener) error {
	value, _ := broadcastMounts.LoadOrStore(listener.server, &sync.Once{})
	var err error
	value.(*sync.Once).Do(func() {
		for _, family := range broadcastFamilies {
			if _, mountErr := listener.MountRouter(family, broadcastRouter); mountErr != nil {
				err = errors.Join(err, mountErr)
			}
		}
	})
	return err
}

var broadcastUsageOnce sync.Once

func registerBroadcastUsage(config broadcast.Config) {
	fleet := mediafleet.Default()
	if fleet == nil {
		return
	}
	broadcastUsageOnce.Do(func() {
		transcoder := config.Transcode
		fleet.AddUsageSource(func() (int64, int64) {
			inUse, _ := transcode.EncoderSessions(transcoder)
			return inUse, edge.CacheUsedBytes()
		})
	})
}

func broadcastConfig(node *Node) (broadcast.Config, error) {
	config := broadcast.ConfigFromEnv(node.WorkerID())
	handler := node.Config()
	if config.PublicIP == "" {
		config.PublicIP = handler.PublicIP
	}
	if config.UDPPortMin == 0 {
		config.UDPPortMin, config.UDPPortMax = handler.UDPPortMin, handler.UDPPortMax
	}
	if len(config.ICEServers) == 0 {
		config.ICEServers = handler.ICEServers
	}
	config.TURNSecret, config.TURNCredentialTTL, config.TURNUsernamePrefix =
		handler.TURNSecret, handler.TURNCredentialTTL, handler.TURNUsernamePrefix
	if config.PublisherReconnectGrace <= 0 && handler.PublisherReconnectGrace > 0 {
		config.PublisherReconnectGrace = handler.PublisherReconnectGrace
	}
	if fleet := mediafleet.Default(); fleet != nil {
		operator := fleet.Config()
		config.RTMPPort, config.RTMPSPort, config.SRTPort = operator.RTMPPort, operator.RTMPSPort, operator.SRTPort
		config.CacheDir, config.CacheBytes, config.CacheMemoryBytes = operator.CacheDir, operator.CacheBytes, operator.CacheMemoryBytes
		if domain := fleet.EdgeDomain(); domain != "" {
			config.EdgeDomain = domain
		}
		if operator.PublicIP != "" {
			config.PublicIP = operator.PublicIP
		}
		if operator.RecordingCheckpointS > 0 {
			config.RecordingCheckpoint = time.Duration(operator.RecordingCheckpointS) * time.Second
		}
		devices, all := fleet.EncoderGPUs()
		config.Transcode.FleetGPUs, config.Transcode.GPUAllDevices = true, all
		config.Transcode.GPUDevices = nil
		for _, device := range devices {
			config.Transcode.GPUDevices = append(config.Transcode.GPUDevices, broadcast.GPUDevice{Index: device.Index, Sessions: device.Sessions})
		}
		config.Transcode.GPULockDirectory, config.Transcode.SeatGPUs = fleet.GPULockDirectory(), operator.SeatGPUs
		if operator.GPUEncoderSessions > 0 {
			config.Transcode.EncoderSessions = operator.GPUEncoderSessions
		} else if capacity := fleet.Capacity(); capacity != nil && capacity.GPU != nil && capacity.GPU.MaxEncoderSessions > 0 {
			config.Transcode.EncoderSessions = int(capacity.GPU.MaxEncoderSessions)
		}
	}
	return config, config.Normalize()
}

func runBroadcastNode(ctx context.Context, runtime *Node) (string, error) {
	decoded, err := broadcast.DecodeSpec(runtime.spec, time.Now().UTC())
	if err != nil {
		return "worker_failed", err
	}
	listener, err := runtime.handler.MediaListener()
	if err != nil {
		return "worker_failed", err
	}
	if err := mountBroadcastRouter(listener); err != nil {
		return "worker_failed", fmt.Errorf("mount broadcast paths: %w", err)
	}
	config, err := broadcastConfig(runtime)
	if err != nil {
		return "worker_failed", err
	}
	registerBroadcastUsage(config)
	node, err := broadcast.NewNode(decoded, runtime.WorkerID(), runtime.Usage(), time.Now())
	if err != nil {
		return "worker_failed", err
	}
	node.Apply(runtime.View())
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case view := <-runtime.Updates():
				node.Apply(view)
			}
		}
	}()
	meter := broadcast.NewUsage(runtime.Usage())
	meterDone := make(chan struct{})
	go func() {
		meter.Run(runCtx)
		close(meterDone)
	}()
	defer func() {
		cancel()
		<-meterDone
	}()
	hooks := broadcast.Hooks{URL: runtime.EndpointURL, Hostname: runtime.Hostname, SetEndpoints: runtime.SetEndpoints,
		Event: runtime.EmitSessionEvent, SetLoad: runtime.SetLoad}
	if tlsConfig := runtime.handler.config.NodeTLS; tlsConfig != nil {
		hooks.GetCertificate = tlsConfig.GetCertificate
	}
	env := &broadcast.Env{Config: config, Router: broadcastRouter, Node: node, Emitter: broadcast.NewEmitterWithFinal(runtime.EmitSessionEvent, runtime.EmitFinalSessionEvent),
		Usage: meter, Hooks: hooks}
	if state := runtime.AttemptState(); state != nil {
		env.StateDir, env.Resumed, env.Redelivery = state.Dir, state.Resumed, state.Redelivery
	}
	if recording := decoded.Details.Recording; node.Role == mc.RolePackage && recording != nil {
		env.Bucket, env.BucketErr = broadcast.OpenRecordingBucket(decoded.Details.BucketAccess, *recording,
			decoded.Details.VODPackage(decoded.Topology), config.Bucket, decoded.Identity.ExpiresAt)
		if env.BucketErr != nil {
			log.Printf("room media %s: recording bucket: %v", node.NodeID, env.BucketErr)
		}
	}
	switch node.Role {
	case mc.RoleIngest:
		err = ingest.Run(runCtx, env)
	case mc.RoleTranscode:
		err = transcode.Run(runCtx, env)
	case mc.RolePackage:
		err = packager.Run(runCtx, env)
	case mc.RoleShield, mc.RoleEdge:
		err = edge.Run(runCtx, env)
	default:
		err = errors.New("unsupported broadcast role")
	}
	if err != nil && ctx.Err() == nil {
		switch {
		case errors.Is(err, packager.ErrPackagerStateLost):
			return "packager_state_lost", err
		case errors.Is(err, packager.ErrRecordingStateLost):
			return "recording_state_lost", err
		}
		return "worker_failed", err
	}
	return "", err
}
