package main

import (
	"flag"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/beamlink/wcp"
	"github.com/Beam-Network/beam/internal/worker/mediafleet"
	mediaplane "github.com/Beam-Network/beam/internal/worker/planes/media"
)

type mediaRoleFlags struct {
	roles              *string
	region             *string
	egressMbps         *int64
	ingressMbps        *int64
	selfTest           *bool
	selfTestURL        *string
	httpsAddr          *string
	rtmpPort           *int
	rtmpsPort          *int
	srtPort            *int
	cacheDir           *string
	cacheBytes         *int64
	cacheMemoryBytes   *int64
	gpuEncoderSessions *int
	gpus               *string
	edgeDomain         *string
	probeTrustedCIDRs  *string
	checkpointS        *int
}

func registerMediaRoleFlags(flags *flag.FlagSet) mediaRoleFlags {
	return mediaRoleFlags{
		roles: flags.String("media-roles", os.Getenv("BEAM_MEDIA_ROLES"),
			"comma-separated media roles this Worker serves: media.sfu, media.relay, media.ingest, media.transcode, media.package, media.shield, media.edge (empty: none)"),
		region: flags.String("media-region", os.Getenv("BEAM_MEDIA_REGION"),
			"media region claim, a broad region: wnam, enam, sam, weur, eeur, apac, oc, afr, me"),
		egressMbps:  flags.Int64("media-egress-mbps", envInt64("BEAM_MEDIA_EGRESS_MBPS", 0), "declared media egress bandwidth in Mbit/s"),
		ingressMbps: flags.Int64("media-ingress-mbps", envInt64("BEAM_MEDIA_INGRESS_MBPS", 0), "declared media ingress bandwidth in Mbit/s (required with media.ingest)"),
		selfTest: flags.Bool("media-self-test", os.Getenv("BEAM_MEDIA_SELF_TEST") != "false",
			"measure media bandwidth at start and every 6 h and report it beside the declared values"),
		selfTestURL: flags.String("media-self-test-url", os.Getenv("BEAM_MEDIA_SELF_TEST_URL"),
			"speed test server of the bandwidth self test, with the Cloudflare speed test API (POST /__up, GET /__down?bytes=N); empty: no self test, only the declared bandwidth is reported"),
		httpsAddr: flags.String("media-https-addr", envOrDefault("BEAM_MEDIA_HTTPS_ADDR", ":443"),
			"browser-facing HTTPS listener of media nodes (edge host certificate)"),
		rtmpPort:  flags.Int("media-rtmp-port", envIntOrDefault("BEAM_MEDIA_RTMP_PORT", 1935), "RTMP ingest port (media.ingest)"),
		rtmpsPort: flags.Int("media-rtmps-port", envIntOrDefault("BEAM_MEDIA_RTMPS_PORT", 1936), "RTMPS ingest port (media.ingest)"),
		srtPort:   flags.Int("media-srt-port", envIntOrDefault("BEAM_MEDIA_SRT_PORT", 9000), "SRT ingest UDP port (media.ingest)"),
		cacheDir:  flags.String("media-cache-dir", os.Getenv("BEAM_MEDIA_CACHE_DIR"), "disk directory of the media.shield and media.edge cache and of the media.package DVR store (required for those roles)"),
		cacheBytes: flags.Int64("media-cache-bytes", envInt64("BEAM_MEDIA_CACHE_BYTES", 0),
			"disk cache size in bytes of media.shield and media.edge"),
		cacheMemoryBytes: flags.Int64("media-cache-memory-bytes", envInt64("BEAM_MEDIA_CACHE_MEMORY_BYTES", 0),
			"memory cache size in bytes of media.shield and media.edge"),
		gpuEncoderSessions: flags.Int("media-gpu-encoder-sessions", envIntOrDefault("BEAM_MEDIA_GPU_ENCODER_SESSIONS", -1),
			"NVENC sessions offered to media.transcode (-1: detected with nvidia-smi; 0: no GPU encoding, the Worker serves source and audio-only ladders only)"),
		gpus: flags.String("media-gpus", envOrDefault("BEAM_MEDIA_GPUS", "auto"),
			"NVIDIA GPUs media.transcode encodes on: auto (the NVENC GPUs nvidia-smi lists), none, or a comma list of nvidia-smi indexes"),
		edgeDomain: flags.String("media-edge-domain", os.Getenv("BEAM_MEDIA_EDGE_DOMAIN"),
			"local override of the edge domain; the network's domain arrives with each media capacity acknowledgement and wins; this Worker's edge host is <label>.edge.<domain>"),
		checkpointS: flags.Int("media-recording-checkpoint-s", envIntOrDefault("BEAM_MEDIA_RECORDING_CHECKPOINT_S", mediafleet.RecordingCheckpointDefaultS),
			"checkpoint interval in seconds of a recording generation (media.package), 60-3600"),
		probeTrustedCIDRs: flags.String("media-probe-trusted-cidrs", os.Getenv("BEAM_MEDIA_PROBE_TRUSTED_CIDRS"),
			"comma-separated CIDRs or addresses of the network's bandwidth probe sources; they get a reserved probe slot and budget"),
	}
}

type mediaListenerFlags struct {
	workerID, stateDir, listenAddress, advertiseURL, publicIP string
	udpPortMin, udpPortMax                                    uint16
	iceServers                                                []string
	turnSecret, turnUsernamePrefix                            string
	turnCredentialTTL, publisherReconnectGrace                time.Duration
	maxViewers                                                int
	allowPublicListeners, legacyProcess, webRTC               bool
}

func mediaPlaneConfig(listener mediaListenerFlags, roles mediaRoleFlags) mediaplane.Config {
	return mediaplane.Config{WorkerID: listener.workerID, StateDir: listener.stateDir, WebRTC: listener.webRTC,
		ListenAddress: listener.listenAddress, AdvertiseURL: listener.advertiseURL, PublicIP: listener.publicIP,
		UDPPortMin: listener.udpPortMin, UDPPortMax: listener.udpPortMax, ICEServers: listener.iceServers,
		TURNSecret: listener.turnSecret, TURNCredentialTTL: listener.turnCredentialTTL, TURNUsernamePrefix: listener.turnUsernamePrefix,
		MaxViewers: listener.maxViewers, PublisherReconnectGrace: listener.publisherReconnectGrace,
		AllowPublicListeners: listener.allowPublicListeners, LegacyProcess: listener.legacyProcess,
		Roles: mediaplane.Roles{Roles: *roles.roles, Region: *roles.region, EgressMbps: *roles.egressMbps,
			IngressMbps: *roles.ingressMbps, SelfTest: *roles.selfTest, SelfTestURL: *roles.selfTestURL,
			HTTPSAddr: *roles.httpsAddr, RTMPPort: *roles.rtmpPort, RTMPSPort: *roles.rtmpsPort, SRTPort: *roles.srtPort,
			CacheDir: *roles.cacheDir, CacheBytes: *roles.cacheBytes, CacheMemoryBytes: *roles.cacheMemoryBytes,
			GPUEncoderSessions: *roles.gpuEncoderSessions, GPUs: *roles.gpus, EdgeDomain: *roles.edgeDomain,
			ProbeTrustedCIDRs: *roles.probeTrustedCIDRs, RecordingCheckpointS: *roles.checkpointS}}
}

func mediaBinder(fleet *mediafleet.Fleet) wcp.MediaBinder {
	if fleet == nil {
		return nil
	}
	return fleet
}

func envInt64(name string, fallback int64) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil {
		return fallback
	}
	return value
}
