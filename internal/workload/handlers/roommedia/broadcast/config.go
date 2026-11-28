package broadcast

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type Config struct {
	WorkerID                string
	EdgeDomain              string
	PublicHost              string
	PublicIP                string
	RTMPPort                int
	RTMPSPort               int
	SRTPort                 int
	UDPPortMin              uint16
	UDPPortMax              uint16
	ICEServers              []string
	TURNSecret              string
	TURNCredentialTTL       time.Duration
	TURNUsernamePrefix      string
	PublisherReconnectGrace time.Duration

	CacheDir         string
	CacheBytes       int64
	CacheMemoryBytes int64

	Transcode           TranscodeConfig
	Bucket              BucketConfig
	RecordingCheckpoint time.Duration

	UpstreamInsecure bool
}

type TranscodeConfig struct {
	Runtime          string
	OCIBinary        string
	Image            string
	FFmpeg           string
	GPU              string
	CPUMaxHeight     int64
	EncoderSessions  int
	MemoryLimit      string
	CPUs             string
	FleetGPUs        bool
	GPUDevices       []GPUDevice
	GPUAllDevices    bool
	GPULockDirectory string
	SeatGPUs         int64
	WorkerID         string
}

type GPUDevice struct {
	Index    int
	Sessions int64
}

type BucketConfig struct {
	URL             string
	Region          string
	AccessKeyID     string
	SecretKeyFile   string
	SecretAccessKey string
}

func ConfigFromEnv(workerID string) Config {
	config := Config{
		WorkerID:         workerID,
		EdgeDomain:       strings.ToLower(strings.TrimSpace(os.Getenv("BEAM_MEDIA_EDGE_DOMAIN"))),
		PublicIP:         strings.TrimSpace(os.Getenv("BEAM_MEDIA_PUBLIC_IP")),
		PublicHost:       strings.TrimSpace(os.Getenv("BEAM_MEDIA_PUBLIC_HOST")),
		RTMPPort:         envInt("BEAM_MEDIA_RTMP_PORT", 1935),
		RTMPSPort:        envInt("BEAM_MEDIA_RTMPS_PORT", 1936),
		SRTPort:          envInt("BEAM_MEDIA_SRT_PORT", 9000),
		ICEServers:       splitList(os.Getenv("BEAM_MEDIA_ICE_SERVERS")),
		CacheDir:         strings.TrimSpace(os.Getenv("BEAM_MEDIA_CACHE_DIR")),
		CacheBytes:       int64(envInt("BEAM_MEDIA_CACHE_BYTES", 0)),
		CacheMemoryBytes: int64(envInt("BEAM_MEDIA_CACHE_MEMORY_BYTES", 0)),
		UpstreamInsecure: envBool("BEAM_MEDIA_UPSTREAM_INSECURE"),
		RecordingCheckpoint: time.Duration(envInt("BEAM_MEDIA_RECORDING_CHECKPOINT_S",
			mc.RecordingCheckpointDefaultS)) * time.Second,
		Transcode: TranscodeConfig{
			Runtime:         envOr("BEAM_MEDIA_TRANSCODE_RUNTIME", "oci"),
			OCIBinary:       envOr("BEAM_MEDIA_OCI_BINARY", "docker"),
			Image:           envOr("BEAM_MEDIA_TRANSCODE_IMAGE", "media-transcoder:latest"),
			FFmpeg:          envOr("BEAM_MEDIA_FFMPEG", "ffmpeg"),
			GPU:             envOr("BEAM_MEDIA_TRANSCODE_GPU", "auto"),
			CPUMaxHeight:    int64(envInt("BEAM_MEDIA_TRANSCODE_CPU_MAX_HEIGHT", 360)),
			EncoderSessions: envInt("BEAM_MEDIA_GPU_ENCODER_SESSIONS", 0),
			MemoryLimit:     envOr("BEAM_MEDIA_TRANSCODE_MEMORY", "4g"),
			CPUs:            envOr("BEAM_MEDIA_TRANSCODE_CPUS", "4"),
			WorkerID:        workerID,
		},
		Bucket: BucketConfig{
			URL:             strings.TrimSpace(os.Getenv("BEAM_MEDIA_BUCKET_URL")),
			Region:          envOr("BEAM_MEDIA_BUCKET_REGION", "auto"),
			AccessKeyID:     strings.TrimSpace(os.Getenv("BEAM_MEDIA_BUCKET_ACCESS_KEY_ID")),
			SecretKeyFile:   strings.TrimSpace(os.Getenv("BEAM_MEDIA_BUCKET_SECRET_FILE")),
			SecretAccessKey: "",
		},
	}
	if minimum := envInt("BEAM_MEDIA_UDP_PORT_MIN", 0); minimum > 0 && minimum <= 65535 {
		config.UDPPortMin = uint16(minimum)
	}
	if maximum := envInt("BEAM_MEDIA_UDP_PORT_MAX", 0); maximum > 0 && maximum <= 65535 {
		config.UDPPortMax = uint16(maximum)
	}
	if grace, err := time.ParseDuration(os.Getenv("BEAM_MEDIA_PUBLISHER_RECONNECT_GRACE")); err == nil && grace > 0 {
		config.PublisherReconnectGrace = grace
	}
	return config
}

func (c *Config) Normalize() error {
	if strings.TrimSpace(c.WorkerID) == "" {
		return errors.New("broadcast media roles require the worker id")
	}
	if c.EdgeDomain != "" && !validDomain(c.EdgeDomain) {
		return errors.New("BEAM_MEDIA_EDGE_DOMAIN must be a lowercase domain of two or more labels")
	}
	if c.PublicHost == "" {
		c.PublicHost = c.PublicIP
	}
	if c.PublicHost == "" {
		c.PublicHost = "127.0.0.1"
	}
	if c.PublisherReconnectGrace <= 0 || c.PublisherReconnectGrace > 30*time.Second {
		c.PublisherReconnectGrace = 30 * time.Second
	}
	if c.RecordingCheckpoint <= 0 {
		c.RecordingCheckpoint = mc.RecordingCheckpointDefaultS * time.Second
	}
	if c.RecordingCheckpoint < time.Minute || c.RecordingCheckpoint > time.Hour {
		return errors.New("the recording checkpoint interval must be between 60 and 3600 seconds")
	}
	if c.Transcode.CPUMaxHeight < 0 {
		c.Transcode.CPUMaxHeight = 0
	}
	if (c.UDPPortMin == 0) != (c.UDPPortMax == 0) || (c.UDPPortMin != 0 && c.UDPPortMin > c.UDPPortMax) {
		return errors.New("media UDP port range is invalid")
	}
	return nil
}

func validDomain(domain string) bool {
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil {
		return fallback
	}
	return int(value)
}

func envBool(name string) bool {
	value, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return value
}

func splitList(value string) []string {
	var values []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return values
}
