package mediacontracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	JoinSchema              = "media-join/v1"
	NodeRuntimeSchema       = "media-node-runtime/v1"
	CapacitySchema          = "media-capacity/v1"
	QoESchema               = "beam-media-qoe/v1"
	KeysSchema              = "beam-media-keys/v1"
	EdgeCertSchema          = "media-edge-cert/v1"
	EventsChannelLabel      = "beam.events.v1"
	TopologyUpdateType      = "media_topology_update"
	CapacityReportType      = "media_capacity_report"
	UsageReportType         = "media_usage_report"
	CertRequestType         = "media_cert_request"
	CertResultType          = "media_cert_result"
	SessionEventAckType     = "media_session_event_ack"
	CapacityReportAckType   = "media_capacity_report_ack"
	UsageReportAckType      = "media_usage_report_ack"
	CertRequestAckType      = "media_cert_request_ack"
	NodeOfferRequestType    = "media_node_offer_request"
	NodeOfferRequestAckType = "media_node_offer_request_ack"
	TrackSourcesHeader      = "Beam-Track-Sources"
	HLSTokenQueryParameter  = "token"
)

const WHEPReceiveSlotsMax = 32

const (
	OpusFmtpVoice    = "minptime=10;useinbandfec=1;usedtx=1"
	OpusFmtpMusic    = "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1"
	OpusFmtpWHEPSlot = "minptime=10;useinbandfec=1;stereo=1"
)

const WorkerFeatureMediaBots = "media_bots"

const (
	WorkerFeatureUsageV2       = "usage_v2"
	WorkerFeatureMediaReceipts = "media_receipts"
)

func OpusFmtpFor(source string) string {
	if source == TrackSourceMusic {
		return OpusFmtpMusic
	}
	return OpusFmtpVoice
}

const (
	ActivityRecording          = "recording"
	ActivityTranscribing       = "transcribing"
	ActivityTranslating        = "translating"
	ActivityAssisting          = "assisting"
	ParticipantActivitiesMax   = 4
	SetActivitiesMinIntervalMS = 1000
)

var (
	ParticipantActivities = []string{ActivityRecording, ActivityTranscribing, ActivityTranslating, ActivityAssisting}
	BotOnlyActivities     = []string{ActivityRecording, ActivityTranscribing}
	ErrParticipantKind    = errors.New("mediacontracts: participant kind is human or bot")
	ErrActivities         = errors.New("mediacontracts: activities are at most 4 unique values of recording, transcribing, translating, assisting")
)

func ValidateActivities(activities []string) error {
	if len(activities) > ParticipantActivitiesMax {
		return ErrActivities
	}
	seen := map[string]bool{}
	for _, activity := range activities {
		if !contains(ParticipantActivities, activity) || seen[activity] {
			return ErrActivities
		}
		seen[activity] = true
	}
	return nil
}

func validParticipantKind(kind string) bool { return kind == "" || contains(ParticipantKinds, kind) }

func SetActivitiesError(kind string, activities []string) string {
	if ValidateActivities(activities) != nil {
		return "invalid_command"
	}
	if kind != KindBot {
		for _, activity := range activities {
			if contains(BotOnlyActivities, activity) {
				return "forbidden"
			}
		}
	}
	return ""
}

type RTTHint struct {
	Region string `json:"region"`
	RTTMS  int64  `json:"rtt_ms"`
}

type JoinClient struct {
	SDK      string `json:"sdk"`
	Platform string `json:"platform"`
}

type JoinRequest struct {
	SchemaVersion  string      `json:"schema_version"`
	Transport      string      `json:"transport"`
	RTTHints       []RTTHint   `json:"rtt_hints,omitempty"`
	ExcludeNodeIDs []string    `json:"exclude_node_ids,omitempty"`
	ExcludeEpoch   *int64      `json:"exclude_epoch,omitempty"`
	Client         *JoinClient `json:"client,omitempty"`
	ReceiptKey     *string     `json:"receipt_key,omitempty"`
}

type JoinEndpoint struct {
	NodeID string `json:"node_id"`
	Role   string `json:"role"`
	Region string `json:"region"`
	URL    string `json:"url"`
	Rank   int64  `json:"rank"`
}

type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   *string  `json:"username"`
	Credential *string  `json:"credential"`
}

type JoinRTC struct {
	Publish    []JoinEndpoint `json:"publish"`
	Subscribe  []JoinEndpoint `json:"subscribe"`
	ICEServers []ICEServer    `json:"ice_servers"`
}

const RecordingMaxGenerations = 64

type ManifestKey struct {
	NodeID     string `json:"node_id"`
	SessionID  string `json:"session_id"`
	PublicKey  string `json:"public_key"`
	Generation *int64 `json:"generation,omitempty"`
}

type JoinHLS struct {
	Edges         []JoinEndpoint `json:"edges"`
	ManifestKeys  []ManifestKey  `json:"manifest_keys"`
	ContentKeyURL *string        `json:"content_key_url"`
}

type JoinBeacon struct {
	URL        string `json:"url"`
	IntervalMS int64  `json:"interval_ms"`
}

type JoinResponse struct {
	SchemaVersion   string             `json:"schema_version"`
	SessionID       string             `json:"session_id"`
	ProjectID       *string            `json:"project_id"`
	DeliveryProfile string             `json:"delivery_profile"`
	Role            string             `json:"role"`
	Identity        string             `json:"identity"`
	TopologyEpoch   int64              `json:"topology_epoch"`
	ExpiresAtMS     int64              `json:"expires_at_ms"`
	Protection      TopologyProtection `json:"protection"`
	RTC             *JoinRTC           `json:"rtc"`
	HLS             *JoinHLS           `json:"hls"`
	Beacon          JoinBeacon         `json:"beacon"`
	Receipts        *JoinReceipts      `json:"receipts,omitempty"`
}

type JoinReceipts struct {
	Grant      string `json:"grant"`
	IntervalMS int64  `json:"interval_ms"`
}

const (
	ReceiptGrantQueryParameter = "receipt_grant"
	ReceiptConnectionHeader    = "Beam-Receipt-Connection"
)

type ReceiptPost struct {
	Receipt string `json:"receipt"`
}

type HLSReceiptPost struct {
	Grant   string  `json:"grant"`
	Receipt *string `json:"receipt"`
}

type HLSReceiptAnswer struct {
	NodeID       string `json:"node_id"`
	ConnectionID string `json:"connection_id"`
	Sequence     int64  `json:"sequence"`
}

type ReceiptCommand struct {
	CommandHeader
	Receipt string `json:"receipt"`
}

func (command ReceiptCommand) Validate() error { return validateReceiptText(command.Receipt) }

type APIError struct {
	Error            string   `json:"error"`
	Detail           *string  `json:"detail"`
	RetryAfterMS     *int64   `json:"retry_after_ms,omitempty"`
	CreditsRequired  *float64 `json:"credits_required,omitempty"`
	CreditsAvailable *float64 `json:"credits_available,omitempty"`
	Field            *string  `json:"field,omitempty"`
}

const (
	UnknownFieldError     = "unknown_field"
	UnknownFieldMaxLength = 128
)

func UnknownFieldName(key string) string {
	units := 0
	for index, point := range key {
		width := 1
		if point > 0xFFFF {
			width = 2
		}
		if units+width > UnknownFieldMaxLength {
			return key[:index]
		}
		units += width
	}
	if key == "" {
		return `""`
	}
	return key
}

var (
	apiErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	ackReasonPattern    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	ackFeaturePattern   = regexp.MustCompile(`^[a-z0-9_.]{1,64}$`)
)

func ValidateAPIError(body APIError) error {
	if !apiErrorCodePattern.MatchString(body.Error) || (body.Detail != nil && len(*body.Detail) > 1024) ||
		(body.RetryAfterMS != nil && *body.RetryAfterMS < 0) {
		return ErrStrictJSON
	}
	credits := body.CreditsRequired != nil || body.CreditsAvailable != nil
	if credits && (body.Error != "insufficient_credits" || body.CreditsRequired == nil || body.CreditsAvailable == nil ||
		*body.CreditsRequired < 0 || *body.CreditsAvailable < 0) {
		return ErrStrictJSON
	}
	if (body.Error == UnknownFieldError) != (body.Field != nil) {
		return ErrStrictJSON
	}
	if body.Field != nil && (*body.Field == "" || utf16Length(*body.Field) > UnknownFieldMaxLength) {
		return ErrStrictJSON
	}
	return nil
}

func utf16Length(value string) int {
	units := 0
	for _, point := range value {
		units++
		if point > 0xFFFF {
			units++
		}
	}
	return units
}

func RetryAfterSeconds(retryAfterMS int64) int64 {
	seconds := (retryAfterMS + 999) / 1000
	if seconds < 1 {
		return 1
	}
	return seconds
}

type NodeRuntime struct {
	SchemaVersion        string               `json:"schema_version"`
	Capability           string               `json:"capability"`
	NodeID               string               `json:"node_id"`
	WorkerID             string               `json:"worker_id"`
	NodeKey              string               `json:"node_key"`
	Endpoints            []TopologyEndpoint   `json:"endpoints"`
	Hostname             *string              `json:"hostname"`
	TLSCertificateSHA256 *string              `json:"tls_certificate_sha256"`
	ExpiresAt            string               `json:"expires_at"`
	Topology             *NodeTopologyVersion `json:"topology,omitempty"`
}

type NodeTopologyVersion struct {
	Epoch    int64 `json:"epoch"`
	Revision int64 `json:"revision"`
}

type NodeLoad struct {
	Publishers  int64   `json:"publishers"`
	Subscribers int64   `json:"subscribers"`
	IngressBPS  int64   `json:"ingress_bps"`
	EgressBPS   int64   `json:"egress_bps"`
	CPURatio    float64 `json:"cpu_ratio"`
}

type TopologyUpdate struct {
	Type          string   `json:"type"`
	SchemaVersion string   `json:"schema_version"`
	Kind          string   `json:"kind"`
	WorkloadID    string   `json:"workload_id"`
	UnitID        string   `json:"unit_id"`
	Epoch         int64    `json:"epoch"`
	Attempt       int64    `json:"attempt"`
	Topology      Topology `json:"topology"`
	SentAt        string   `json:"sent_at"`
}

type PortRange struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

type MediaPorts struct {
	HTTPS int64  `json:"https"`
	RTMP  *int64 `json:"rtmp"`
	RTMPS *int64 `json:"rtmps"`
	SRT   *int64 `json:"srt"`
}

type Bandwidth struct {
	DeclaredMbps int64  `json:"declared_mbps"`
	MeasuredMbps *int64 `json:"measured_mbps"`
	MeasuredAtMS *int64 `json:"measured_at_ms"`
}

type GPUEncoder struct {
	Vendor             string `json:"vendor"`
	Model              string `json:"model"`
	Encoder            string `json:"encoder"`
	MaxEncoderSessions int64  `json:"max_encoder_sessions"`
}

type CacheCapacity struct {
	DiskBytes   int64 `json:"disk_bytes"`
	MemoryBytes int64 `json:"memory_bytes"`
}

type WorkerLoad struct {
	CPURatio        float64 `json:"cpu_ratio"`
	EgressMbps      int64   `json:"egress_mbps"`
	IngressMbps     int64   `json:"ingress_mbps"`
	Sessions        int64   `json:"sessions"`
	Subscribers     int64   `json:"subscribers"`
	EncoderSessions int64   `json:"encoder_sessions"`
	CacheUsedBytes  int64   `json:"cache_used_bytes"`
}

type WorkerCapacity struct {
	WorkerID     string              `json:"worker_id"`
	Roles        []string            `json:"roles"`
	RegionClaim  *string             `json:"region_claim"`
	PublicIPv4   *string             `json:"public_ipv4"`
	PublicIPv6   *string             `json:"public_ipv6"`
	UDPPortRange *PortRange          `json:"udp_port_range"`
	Ports        MediaPorts          `json:"ports"`
	Egress       Bandwidth           `json:"egress"`
	Ingress      Bandwidth           `json:"ingress"`
	GPU          *GPUEncoder         `json:"gpu"`
	Cache        *CacheCapacity      `json:"cache"`
	Load         WorkerLoad          `json:"load"`
	EdgeHostname *string             `json:"edge_hostname"`
	Certificates []ServedCertificate `json:"certificates,omitempty"`
	Features     []string            `json:"features,omitempty"`
}

type ServedCertificate struct {
	Hostname string `json:"hostname"`
	SHA256   string `json:"sha256"`
}

const ServedCertificatesMax = 64

type CapacityReport struct {
	Type               string           `json:"type"`
	SchemaVersion      string           `json:"schema_version"`
	OrchestratorHotkey string           `json:"orchestrator_hotkey"`
	ReportedAtMS       int64            `json:"reported_at_ms"`
	Workers            []WorkerCapacity `json:"workers"`
}

var MediaAckRetryableReasons = []string{"retry_later", "media_control_unavailable", "media_metering_unavailable",
	"media_usage_reports_unavailable", "event_failed"}

func MediaAckRetryable(reason string) bool { return contains(MediaAckRetryableReasons, reason) }

type MediaAck struct {
	Type                 string          `json:"type"`
	Accepted             bool            `json:"accepted"`
	Duplicate            *bool           `json:"duplicate,omitempty"`
	Reason               *string         `json:"reason,omitempty"`
	Workers              *int64          `json:"workers,omitempty"`
	EdgeDomain           *string         `json:"edge_domain,omitempty"`
	CertificatesRequired []string        `json:"certificates_required,omitempty"`
	HandledBy            *string         `json:"handled_by,omitempty"`
	Offer                json.RawMessage `json:"offer,omitempty"`
	Features             []string        `json:"features,omitempty"`
}

func ValidateMediaAck(ack MediaAck) error {
	const ackFeaturesMax = 16
	switch ack.Type {
	case CapacityReportAckType, UsageReportAckType, SessionEventAckType, CertRequestAckType, NodeOfferRequestAckType:
	default:
		return ErrStrictJSON
	}
	if ack.Accepted == (ack.Reason != nil) || (ack.Reason != nil && !ackReasonPattern.MatchString(*ack.Reason)) ||
		(!ack.Accepted && ack.Duplicate != nil && *ack.Duplicate) {
		return ErrStrictJSON
	}
	if ack.Type != CapacityReportAckType && (ack.Workers != nil || ack.EdgeDomain != nil || ack.CertificatesRequired != nil || ack.Features != nil) {
		return ErrStrictJSON
	}
	if ack.Features != nil && (len(ack.Features) == 0 || len(ack.Features) > ackFeaturesMax) {
		return ErrStrictJSON
	}
	for index, feature := range ack.Features {
		if !ackFeaturePattern.MatchString(feature) || contains(ack.Features[:index], feature) {
			return ErrStrictJSON
		}
	}
	if ack.CertificatesRequired != nil && (len(ack.CertificatesRequired) == 0 || len(ack.CertificatesRequired) > CertificatesRequiredMax) {
		return ErrStrictJSON
	}
	for _, hostname := range ack.CertificatesRequired {
		if !ValidHostname(hostname) {
			return ErrStrictJSON
		}
	}
	if ack.EdgeDomain != nil && (!ValidHostname(*ack.EdgeDomain) || !strings.Contains(*ack.EdgeDomain, ".")) {
		return ErrStrictJSON
	}
	if ack.Workers != nil && *ack.Workers < 0 {
		return ErrStrictJSON
	}
	if ack.Type != CertRequestAckType && ack.HandledBy != nil {
		return ErrStrictJSON
	}
	if ack.Type != NodeOfferRequestAckType && len(ack.Offer) != 0 {
		return ErrStrictJSON
	}
	return nil
}

var MediaNodeOfferRequestReasons = []string{"worker_reconnected", "worker_restarted", "orchestrator_restarted", "redelivery"}

var MediaNodeOfferRefusals = []string{"invalid_payload", "unknown_workload", "workload_ended", "workload_retired", "stale_attempt",
	"orchestrator_binding_mismatch"}

type MediaNodeOfferRequest struct {
	Type          string  `json:"type"`
	SchemaVersion string  `json:"schema_version"`
	Kind          string  `json:"kind"`
	WorkloadID    string  `json:"workload_id"`
	UnitID        string  `json:"unit_id"`
	Epoch         int64   `json:"epoch"`
	Attempt       int64   `json:"attempt"`
	WorkerID      *string `json:"worker_id"`
	Reason        string  `json:"reason"`
	RequestedAt   string  `json:"requested_at"`
}

func ValidateMediaNodeOfferAck(ack MediaAck, request MediaNodeOfferRequest) error {
	if ack.Type != NodeOfferRequestAckType || ack.Duplicate != nil || ValidateMediaAck(ack) != nil {
		return ErrStrictJSON
	}
	if !ack.Accepted {
		if ack.Reason == nil || *ack.Reason == "" || len(ack.Offer) != 0 {
			return ErrStrictJSON
		}
		return nil
	}
	if ack.Reason != nil || len(ack.Offer) == 0 {
		return ErrStrictJSON
	}
	var head struct {
		Type       string `json:"type"`
		Kind       string `json:"kind"`
		WorkloadID string `json:"workload_id"`
		UnitID     string `json:"unit_id"`
		Epoch      int64  `json:"epoch"`
		Attempt    int64  `json:"attempt"`
	}
	if err := json.Unmarshal(ack.Offer, &head); err != nil {
		return err
	}
	if head.Type != "room_workload_offer" || head.Kind != "room.media" || head.WorkloadID != request.WorkloadID ||
		head.UnitID != request.UnitID || head.Epoch != request.Epoch || head.Attempt != request.Attempt {
		return ErrStrictJSON
	}
	return nil
}

type NodeRecording struct {
	RecordingID  string `json:"recording_id"`
	BucketPrefix string `json:"bucket_prefix"`
	Generation   *int64 `json:"generation,omitempty"`
	Generations  *int64 `json:"generations,omitempty"`
}

type BucketAccess struct {
	Prefix     string            `json:"prefix"`
	Methods    []string          `json:"methods"`
	Signer     *BucketSigner     `json:"signer"`
	Credential *BucketCredential `json:"credential"`
}

const RecordingFileSchema = "beam-media-recording/v1"

const RecordingCheckpointDefaultS = 300

type RecordingFile struct {
	SchemaVersion   string   `json:"schema_version"`
	RecordingID     string   `json:"recording_id"`
	SessionID       string   `json:"session_id"`
	Generation      int64    `json:"generation"`
	PackagerNodeID  string   `json:"packager_node_id"`
	PackagerNodeKey string   `json:"packager_node_key"`
	Complete        bool     `json:"complete"`
	DurationMS      int64    `json:"duration_ms"`
	Bytes           int64    `json:"bytes"`
	Renditions      []string `json:"renditions"`
	UpdatedAtMS     int64    `json:"updated_at_ms"`
}

type BucketSigner struct {
	URL         string `json:"url"`
	Token       string `json:"token"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
}

type BucketCredential struct {
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	ExpiresAtMS     int64  `json:"expires_at_ms"`
}

type BucketSignRequest struct {
	Method        string  `json:"method"`
	Key           string  `json:"key"`
	ContentType   *string `json:"content_type"`
	ContentLength *int64  `json:"content_length"`
}

type BucketSignResponse struct {
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	ExpiresAtMS int64             `json:"expires_at_ms"`
}

type UsageReportMessage struct {
	Type          string         `json:"type"`
	SchemaVersion string         `json:"schema_version"`
	WorkloadID    string         `json:"workload_id"`
	NodeID        string         `json:"node_id"`
	Report        SignedEnvelope `json:"report"`
}

type CertRequest struct {
	Type          string `json:"type"`
	SchemaVersion string `json:"schema_version"`
	RequestID     string `json:"request_id"`
	WorkerID      string `json:"worker_id"`
	Hostname      string `json:"hostname"`
	CSRPEM        string `json:"csr_pem"`
	RequestedAtMS int64  `json:"requested_at_ms"`
}

type CertResult struct {
	Type           string  `json:"type"`
	SchemaVersion  string  `json:"schema_version"`
	RequestID      string  `json:"request_id"`
	WorkerID       string  `json:"worker_id"`
	Hostname       string  `json:"hostname"`
	Status         string  `json:"status"`
	CertificatePEM *string `json:"certificate_pem"`
	NotBeforeMS    *int64  `json:"not_before_ms"`
	NotAfterMS     *int64  `json:"not_after_ms"`
	RenewAfterMS   *int64  `json:"renew_after_ms"`
	Reason         *string `json:"reason"`
}

type EpochKey struct {
	Epoch   int64  `json:"epoch"`
	BaseKey string `json:"base_key"`
}

type MediaKeys struct {
	SchemaVersion  string     `json:"schema_version"`
	SessionID      string     `json:"session_id"`
	CipherSuite    int64      `json:"cipher_suite"`
	SenderIndex    int64      `json:"sender_index"`
	SenderSlot     int64      `json:"sender_slot"`
	CurrentEpoch   int64      `json:"current_epoch"`
	Keys           []EpochKey `json:"keys"`
	RefreshAfterMS int64      `json:"refresh_after_ms"`
}

type ClearKeyRequest struct {
	KIDs []string `json:"kids"`
	Type string   `json:"type"`
}

type ClearKey struct {
	KTY string `json:"kty"`
	KID string `json:"kid"`
	K   string `json:"k"`
}

type ClearKeyResponse struct {
	Keys []ClearKey `json:"keys"`
	Type string     `json:"type"`
}

type ContentKey struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
	IV    string `json:"iv"`
}

type QoEErrorCount struct {
	Code  string `json:"code"`
	Count int64  `json:"count"`
}

type QoEBeacon struct {
	SchemaVersion string          `json:"schema_version"`
	SessionID     string          `json:"session_id"`
	NodeID        string          `json:"node_id"`
	Transport     string          `json:"transport"`
	PeriodStartMS int64           `json:"period_start_ms"`
	PeriodEndMS   int64           `json:"period_end_ms"`
	BytesReceived int64           `json:"bytes_received"`
	MediaMS       int64           `json:"media_ms"`
	Stalls        int64           `json:"stalls"`
	StallMS       int64           `json:"stall_ms"`
	RTTMS         *int64          `json:"rtt_ms"`
	JitterMS      *int64          `json:"jitter_ms"`
	PacketLossPPM *int64          `json:"packet_loss_ppm"`
	LatencyMS     *int64          `json:"latency_ms"`
	FramesDecoded *int64          `json:"frames_decoded"`
	FramesDropped *int64          `json:"frames_dropped"`
	BitrateBPS    *int64          `json:"bitrate_bps"`
	RenditionID   *string         `json:"rendition_id"`
	EdgeSwitches  int64           `json:"edge_switches"`
	Errors        []QoEErrorCount `json:"errors"`
	Client        JoinClient      `json:"client"`
}

type Participant struct {
	ParticipantID string   `json:"participant_id"`
	Identity      string   `json:"identity"`
	Name          *string  `json:"name"`
	Role          string   `json:"role"`
	Meta          *string  `json:"meta"`
	JoinedAtMS    int64    `json:"joined_at_ms"`
	Kind          string   `json:"kind,omitempty"`
	Activities    []string `json:"activities,omitempty"`
}

func (participant Participant) ParticipantKind() string {
	if participant.Kind == "" {
		return KindHuman
	}
	return participant.Kind
}

func ValidateParticipant(participant Participant) error {
	if !validParticipantKind(participant.Kind) {
		return ErrParticipantKind
	}
	return ValidateActivities(participant.Activities)
}

type SimulcastLayer struct {
	RID           string `json:"rid"`
	Spatial       int64  `json:"spatial"`
	Width         int64  `json:"width"`
	Height        int64  `json:"height"`
	MaxBitrateBPS int64  `json:"max_bitrate_bps"`
}

type Track struct {
	TrackID       string           `json:"track_id"`
	ParticipantID string           `json:"participant_id"`
	Kind          string           `json:"kind"`
	Source        string           `json:"source"`
	Codec         string           `json:"codec"`
	Muted         bool             `json:"muted"`
	Layers        []SimulcastLayer `json:"layers"`
	State         string           `json:"state,omitempty"`
	GraceUntilMS  *int64           `json:"grace_until_ms,omitempty"`
}

const (
	TrackLive     = "live"
	TrackDetached = "detached"
)

func (track Track) Detached() bool { return track.State == TrackDetached }

type Speaker struct {
	ParticipantID string `json:"participant_id"`
	TrackID       string `json:"track_id"`
	LevelDBov     int64  `json:"level_dbov"`
}

type Layer struct {
	Spatial  int64 `json:"spatial"`
	Temporal int64 `json:"temporal"`
}

type EventHeader struct {
	Type string `json:"type"`
	Seq  int64  `json:"seq"`
	TSMS int64  `json:"ts_ms"`
}

type WelcomeEvent struct {
	EventHeader
	ParticipantID    string        `json:"participant_id"`
	Identity         string        `json:"identity"`
	Role             string        `json:"role"`
	SessionID        string        `json:"session_id"`
	NodeID           string        `json:"node_id"`
	TopologyEpoch    int64         `json:"topology_epoch"`
	Participants     []Participant `json:"participants"`
	Tracks           []Track       `json:"tracks"`
	Listeners        int64         `json:"listeners"`
	SnapshotComplete bool          `json:"snapshot_complete"`
}

const (
	EventMessageMaxBytes         = 262_144
	EventSnapshotMaxParticipants = 100
	EventSnapshotMaxTracks       = 200
)

type SnapshotEvent struct {
	EventHeader
	Participants []Participant `json:"participants"`
	Tracks       []Track       `json:"tracks"`
	Complete     bool          `json:"complete"`
}

type ListenerCountEvent struct {
	EventHeader
	Listeners int64 `json:"listeners"`
}

type ParticipantEvent struct {
	EventHeader
	Participant Participant `json:"participant"`
}

type ParticipantLeftEvent struct {
	EventHeader
	ParticipantID string `json:"participant_id"`
	Reason        string `json:"reason"`
}

type TrackPublishedEvent struct {
	EventHeader
	Track Track `json:"track"`
}

type TrackUnpublishedEvent struct {
	EventHeader
	TrackID       string `json:"track_id"`
	ParticipantID string `json:"participant_id"`
}

type TrackMutedEvent struct {
	EventHeader
	TrackID       string `json:"track_id"`
	ParticipantID string `json:"participant_id"`
	Muted         bool   `json:"muted"`
}

type TrackStateEvent struct {
	EventHeader
	TrackID       string `json:"track_id"`
	ParticipantID string `json:"participant_id"`
	State         string `json:"state"`
	GraceUntilMS  *int64 `json:"grace_until_ms"`
}

var ErrTrackState = errors.New("mediacontracts: track_state grace_until_ms is set exactly when state is detached")

func (event TrackStateEvent) Validate() error {
	if (event.State != TrackLive && event.State != TrackDetached) || (event.State == TrackDetached) != (event.GraceUntilMS != nil) {
		return ErrTrackState
	}
	return nil
}

type ActiveSpeakersEvent struct {
	EventHeader
	Speakers   []Speaker `json:"speakers"`
	DominantID *string   `json:"dominant_id"`
}

type SlotAssignedEvent struct {
	EventHeader
	MID           string `json:"mid"`
	TrackID       string `json:"track_id"`
	ParticipantID string `json:"participant_id"`
}

type SlotReleasedEvent struct {
	EventHeader
	MID string `json:"mid"`
}

type LayerChangedEvent struct {
	EventHeader
	TrackID       string `json:"track_id"`
	ParticipantID string `json:"participant_id"`
	Layer         Layer  `json:"layer"`
	Reason        string `json:"reason"`
}

type MigrateEvent struct {
	EventHeader
	Reason  string `json:"reason"`
	AfterMS int64  `json:"after_ms"`
}

type ConnectionClosedEvent struct {
	EventHeader
	Reason       string  `json:"reason"`
	ConnectionID *string `json:"connection_id,omitempty"`
}

type KickedEvent struct {
	EventHeader
	Reason string `json:"reason"`
}

type AnswerEvent struct {
	EventHeader
	ID  string `json:"id"`
	SDP string `json:"sdp"`
}

type AckEvent struct {
	EventHeader
	ID string `json:"id"`
}

type ErrorEvent struct {
	EventHeader
	ID     *string `json:"id"`
	Code   string  `json:"code"`
	Detail *string `json:"detail"`
}

type CommandHeader struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type SubscribeCommand struct {
	CommandHeader
	TrackID     string  `json:"track_id"`
	MID         *string `json:"mid"`
	MaxSpatial  *int64  `json:"max_spatial"`
	MaxTemporal *int64  `json:"max_temporal"`
	Width       *int64  `json:"width"`
	Height      *int64  `json:"height"`
	RID         *string `json:"rid"`
}

type UnsubscribeCommand struct {
	CommandHeader
	TrackID string `json:"track_id"`
}

type MuteCommand struct {
	CommandHeader
	TrackID string `json:"track_id"`
	Muted   bool   `json:"muted"`
}

type MuteParticipantCommand struct {
	CommandHeader
	ParticipantID string  `json:"participant_id"`
	TrackID       *string `json:"track_id"`
}

type KickCommand struct {
	CommandHeader
	ParticipantID string  `json:"participant_id"`
	Reason        *string `json:"reason"`
}

type OfferCommand struct {
	CommandHeader
	SDP string `json:"sdp"`
}

const ListenerReportMax = 1_000_000

type ListenerReportCommand struct {
	CommandHeader
	Listeners int64 `json:"listeners"`
}

type SetActivitiesCommand struct {
	CommandHeader
	Activities []string `json:"activities"`
}

func (command SetActivitiesCommand) Validate() error { return ValidateActivities(command.Activities) }

type ICEResponse struct {
	ICEServers  []ICEServer `json:"ice_servers"`
	ExpiresAtMS int64       `json:"expires_at_ms"`
}

const (
	SessionEventType   = "media_session_event"
	SessionEventSchema = "media-session-event/v1"
)

var SessionEventIDPattern = regexp.MustCompile(`^ev-e[1-9][0-9]{0,8}a[1-9][0-9]{0,8}-[A-Za-z0-9_-]{1,40}$`)

func SessionEventID(epoch, attempt int64, part string) string {
	return "ev-e" + strconv.FormatInt(epoch, 10) + "a" + strconv.FormatInt(attempt, 10) + "-" + part
}

type SessionEvent struct {
	Type          string          `json:"type"`
	SchemaVersion string          `json:"schema_version"`
	WorkloadID    string          `json:"workload_id"`
	NodeID        string          `json:"node_id"`
	SessionID     string          `json:"session_id"`
	EventID       string          `json:"event_id"`
	OccurredAtMS  int64           `json:"occurred_at_ms"`
	Event         json.RawMessage `json:"event"`
}

type ParticipantJoinedBody struct {
	Kind            string  `json:"kind"`
	ParticipantID   string  `json:"participant_id"`
	Identity        string  `json:"identity"`
	Role            string  `json:"role"`
	TokenNonce      *string `json:"token_nonce"`
	Name            *string `json:"name"`
	Meta            *string `json:"meta"`
	ParticipantKind string  `json:"participant_kind,omitempty"`
}

type ParticipantLeftBody struct {
	Kind          string `json:"kind"`
	ParticipantID string `json:"participant_id"`
	Identity      string `json:"identity"`
	Reason        string `json:"reason"`
}

type ParticipantUpdatedBody struct {
	Kind            string   `json:"kind"`
	ParticipantID   string   `json:"participant_id"`
	Identity        string   `json:"identity"`
	Role            string   `json:"role"`
	ParticipantKind string   `json:"participant_kind,omitempty"`
	Activities      []string `json:"activities,omitempty"`
}

type TrackPublishedBody struct {
	Kind          string `json:"kind"`
	ParticipantID string `json:"participant_id"`
	Identity      string `json:"identity"`
	TrackID       string `json:"track_id"`
	TrackKind     string `json:"track_kind"`
	Source        string `json:"source"`
	Codec         string `json:"codec"`
}

type TrackUnpublishedBody struct {
	Kind          string `json:"kind"`
	ParticipantID string `json:"participant_id"`
	TrackID       string `json:"track_id"`
}

type KickBody struct {
	Kind          string  `json:"kind"`
	ParticipantID string  `json:"participant_id"`
	Identity      string  `json:"identity"`
	ByIdentity    string  `json:"by_identity"`
	TokenExp      int64   `json:"token_exp"`
	Reason        *string `json:"reason"`
}

type IngestConnectedBody struct {
	Kind        string  `json:"kind"`
	Protocol    string  `json:"protocol"`
	StreamKeyID *string `json:"stream_key_id"`
	TokenNonce  *string `json:"token_nonce"`
}

var IngestDisconnectReasons = []string{"ended", "revoked", "timeout", "replaced", IngestSourceIncompatible}

const IngestSourceIncompatible = "source_incompatible"

type IngestDisconnectedBody struct {
	Kind        string  `json:"kind"`
	Protocol    string  `json:"protocol"`
	StreamKeyID *string `json:"stream_key_id"`
	Reason      string  `json:"reason"`
}

type RecordingStartedBody struct {
	Kind        string `json:"kind"`
	RecordingID string `json:"recording_id"`
}

type RecordingReadyBody struct {
	Kind         string `json:"kind"`
	RecordingID  string `json:"recording_id"`
	DurationMS   int64  `json:"duration_ms"`
	Bytes        int64  `json:"bytes"`
	ManifestPath string `json:"manifest_path"`
}

type RecordingFailedBody struct {
	Kind        string `json:"kind"`
	RecordingID string `json:"recording_id"`
	Reason      string `json:"reason"`
}

var sessionEventBodies = map[string]func() any{
	"participant_joined": func() any { return &ParticipantJoinedBody{} }, "participant_left": func() any { return &ParticipantLeftBody{} },
	"participant_updated": func() any { return &ParticipantUpdatedBody{} }, "track_published": func() any { return &TrackPublishedBody{} },
	"track_unpublished": func() any { return &TrackUnpublishedBody{} }, "kick": func() any { return &KickBody{} },
	"ingest_connected": func() any { return &IngestConnectedBody{} }, "ingest_disconnected": func() any { return &IngestDisconnectedBody{} },
	"recording_started": func() any { return &RecordingStartedBody{} }, "recording_ready": func() any { return &RecordingReadyBody{} },
	"recording_failed": func() any { return &RecordingFailedBody{} },
}

var recordingManifestPathPattern = regexp.MustCompile(`^media/[A-Za-z0-9_/-]{1,512}/g([1-9]|[1-5][0-9]|6[0-4])/master\.m3u8$`)

func ValidateSessionEvent(event SessionEvent) error {
	if event.Type != SessionEventType || event.SchemaVersion != SessionEventSchema || len(event.EventID) > 64 ||
		!SessionEventIDPattern.MatchString(event.EventID) {
		return ErrStrictJSON
	}
	body, err := DecodeSessionEventBody(event.Event)
	if err != nil {
		return err
	}
	if ready, ok := body.(*RecordingReadyBody); ok && !recordingManifestPathPattern.MatchString(ready.ManifestPath) {
		return ErrStrictJSON
	}
	if disconnected, ok := body.(*IngestDisconnectedBody); ok && !contains(IngestDisconnectReasons, disconnected.Reason) {
		return ErrStrictJSON
	}
	if joined, ok := body.(*ParticipantJoinedBody); ok && !validParticipantKind(joined.ParticipantKind) {
		return ErrParticipantKind
	}
	if updated, ok := body.(*ParticipantUpdatedBody); ok {
		if !validParticipantKind(updated.ParticipantKind) {
			return ErrParticipantKind
		}
		return ValidateActivities(updated.Activities)
	}
	return nil
}

func ValidateMediaNodeOfferRequest(request MediaNodeOfferRequest) error {
	if request.Type != NodeOfferRequestType || request.SchemaVersion != "room-workload/v1" || request.Kind != "room.media" ||
		request.WorkloadID == "" || len(request.WorkloadID) > 256 || request.UnitID == "" || len(request.UnitID) > 256 ||
		request.Epoch < 1 || request.Attempt < 1 || !contains(MediaNodeOfferRequestReasons, request.Reason) ||
		(request.WorkerID != nil && !printablePattern.MatchString(*request.WorkerID)) {
		return ErrStrictJSON
	}
	if _, err := time.Parse(time.RFC3339Nano, request.RequestedAt); err != nil {
		return err
	}
	return nil
}

func DecodeSessionEventBody(raw json.RawMessage) (any, error) {
	var head struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, err
	}
	factory, ok := sessionEventBodies[head.Kind]
	if !ok {
		return nil, ErrStrictJSON
	}
	body := factory()
	return body, UnmarshalStrict(raw, body)
}

type WebhookEvent struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt string          `json:"created_at"`
	ProjectID string          `json:"project_id"`
	Data      json.RawMessage `json:"data"`
}

type SessionStartedData struct {
	SessionID       string `json:"session_id"`
	DeliveryProfile string `json:"delivery_profile"`
	StartedAt       string `json:"started_at"`
}
type SessionEndedData struct {
	SessionID string `json:"session_id"`
	EndedAt   string `json:"ended_at"`
	Reason    string `json:"reason"`
}
type ParticipantJoinedData struct {
	SessionID     string `json:"session_id"`
	ParticipantID string `json:"participant_id"`
	Identity      string `json:"identity"`
	Role          string `json:"role"`
	NodeID        string `json:"node_id"`
	Kind          string `json:"kind,omitempty"`
}

type ParticipantUpdatedData struct {
	SessionID     string   `json:"session_id"`
	ParticipantID string   `json:"participant_id"`
	Identity      string   `json:"identity"`
	Role          string   `json:"role"`
	Kind          string   `json:"kind"`
	Activities    []string `json:"activities"`
}

func (data ParticipantUpdatedData) MarshalJSON() ([]byte, error) {
	type plain ParticipantUpdatedData
	if data.Kind == "" {
		data.Kind = KindHuman
	}
	if data.Activities == nil {
		data.Activities = []string{}
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(plain(data)); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

func ValidateWebhookData(event WebhookEvent) error {
	factory := WebhookDataTypes[event.Type]
	if factory == nil {
		return ErrStrictJSON
	}
	data := factory()
	if err := UnmarshalStrict(event.Data, data); err != nil {
		return err
	}
	switch typed := data.(type) {
	case *ParticipantJoinedData:
		if !validParticipantKind(typed.Kind) {
			return ErrParticipantKind
		}
	case *ParticipantUpdatedData:
		if !contains(ParticipantKinds, typed.Kind) {
			return ErrParticipantKind
		}
		return ValidateActivities(typed.Activities)
	}
	return nil
}

type ParticipantLeftData struct {
	SessionID     string `json:"session_id"`
	ParticipantID string `json:"participant_id"`
	Identity      string `json:"identity"`
	Reason        string `json:"reason"`
}
type TrackPublishedData struct {
	SessionID     string `json:"session_id"`
	ParticipantID string `json:"participant_id"`
	Identity      string `json:"identity"`
	TrackID       string `json:"track_id"`
	Kind          string `json:"kind"`
	Source        string `json:"source"`
}
type TrackUnpublishedData struct {
	SessionID     string `json:"session_id"`
	ParticipantID string `json:"participant_id"`
	TrackID       string `json:"track_id"`
}
type IngestConnectedData struct {
	SessionID   string  `json:"session_id"`
	StreamKeyID *string `json:"stream_key_id"`
	Protocol    string  `json:"protocol"`
	NodeID      string  `json:"node_id"`
}
type IngestDisconnectedData struct {
	SessionID   string  `json:"session_id"`
	StreamKeyID *string `json:"stream_key_id"`
	Reason      string  `json:"reason"`
}
type RecordingStartedData struct {
	SessionID   string `json:"session_id"`
	RecordingID string `json:"recording_id"`
}
type RecordingReadyData struct {
	SessionID   string `json:"session_id"`
	RecordingID string `json:"recording_id"`
	DurationMS  int64  `json:"duration_ms"`
	Bytes       int64  `json:"bytes"`
}
type RecordingFailedData struct {
	SessionID   string `json:"session_id"`
	RecordingID string `json:"recording_id"`
	Reason      string `json:"reason"`
}

var WebhookDataTypes = map[string]func() any{
	"session.started": func() any { return &SessionStartedData{} }, "session.ended": func() any { return &SessionEndedData{} },
	"participant.joined": func() any { return &ParticipantJoinedData{} }, "participant.left": func() any { return &ParticipantLeftData{} },
	"participant.updated": func() any { return &ParticipantUpdatedData{} },
	"track.published":     func() any { return &TrackPublishedData{} }, "track.unpublished": func() any { return &TrackUnpublishedData{} },
	"ingest.connected": func() any { return &IngestConnectedData{} }, "ingest.disconnected": func() any { return &IngestDisconnectedData{} },
	"recording.started": func() any { return &RecordingStartedData{} }, "recording.ready": func() any { return &RecordingReadyData{} },
	"recording.failed": func() any { return &RecordingFailedData{} },
}

type MediaProject struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	TokenAudience string  `json:"token_audience"`
	RoomID        *string `json:"room_id"`
	CreatedAt     string  `json:"created_at"`
}

type SigningKey struct {
	KID       string  `json:"kid"`
	ProjectID string  `json:"project_id"`
	PublicKey string  `json:"public_key"`
	Label     *string `json:"label"`
	Managed   bool    `json:"managed"`
	Status    string  `json:"status"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at"`
}

type SessionProtection struct {
	Mode        string  `json:"mode"`
	KeyProvider *string `json:"key_provider"`
}

type RecordingSettings struct {
	Enabled    bool  `json:"enabled"`
	DVRWindowS int64 `json:"dvr_window_s"`
}

type MediaSession struct {
	ID              string            `json:"id"`
	ProjectID       *string           `json:"project_id"`
	Room            *TopologyRoom     `json:"room"`
	DeliveryProfile string            `json:"delivery_profile"`
	Name            *string           `json:"name"`
	Status          string            `json:"status"`
	Protection      SessionProtection `json:"protection"`
	HLSOutput       bool              `json:"hls_output"`
	Ladder          *TranscodeLadder  `json:"ladder"`
	Recording       RecordingSettings `json:"recording"`
	Regions         *[]string         `json:"regions"`
	MaxParticipants *int64            `json:"max_participants"`
	Metadata        *string           `json:"metadata"`
	TopologyEpoch   *int64            `json:"topology_epoch"`
	StartGraceS     *int64            `json:"start_grace_s"`
	IdleEndS        *int64            `json:"idle_end_s"`
	ChannelID       *string           `json:"channel_id,omitempty"`
	CreatedAt       string            `json:"created_at"`
	StartedAt       *string           `json:"started_at"`
	EndedAt         *string           `json:"ended_at"`
}

const CertificatesRequiredMax = 64

var CertRefusalReasons = []string{"hostname_not_assigned", "csr_invalid", "worker_evicted", "ca_unavailable", "rate_limited", "busy"}

const IngestRetryAfterMaxMS = 60_000

type IngestURLs struct {
	WHIPURL        *string `json:"whip_url"`
	RTMPServerURL  *string `json:"rtmp_server_url"`
	RTMPSServerURL *string `json:"rtmps_server_url"`
	SRTURL         *string `json:"srt_url"`
	Ready          bool    `json:"ready"`
	RetryAfterMS   *int64  `json:"retry_after_ms"`
}

type StreamKey struct {
	ID         string     `json:"id"`
	SessionID  *string    `json:"session_id"`
	ChannelID  *string    `json:"channel_id,omitempty"`
	Label      *string    `json:"label"`
	Status     string     `json:"status"`
	CreatedAt  string     `json:"created_at"`
	RevokedAt  *string    `json:"revoked_at"`
	LastUsedAt *string    `json:"last_used_at"`
	Ingest     IngestURLs `json:"ingest"`
	Key        *string    `json:"key,omitempty"`
}

type Recording struct {
	ID         string  `json:"id"`
	SessionID  string  `json:"session_id"`
	Status     string  `json:"status"`
	StartedAt  string  `json:"started_at"`
	EndedAt    *string `json:"ended_at"`
	DurationMS *int64  `json:"duration_ms"`
	Bytes      *int64  `json:"bytes"`
	CreatedAt  string  `json:"created_at"`
}

type WebhookEndpoint struct {
	ID        string   `json:"id"`
	ProjectID string   `json:"project_id"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
	Secret    *string  `json:"secret,omitempty"`
}

type UsageRow struct {
	ProjectID   *string `json:"project_id"`
	SessionID   *string `json:"session_id"`
	Product     string  `json:"product"`
	Unit        string  `json:"unit"`
	Quantity    float64 `json:"quantity"`
	PeriodStart string  `json:"period_start"`
	PeriodEnd   string  `json:"period_end"`
}

type TokenAudience struct {
	Enabled bool     `json:"enabled"`
	MaxRole string   `json:"max_role"`
	KeyIDs  []string `json:"key_ids"`
}

type RoomChannelMediaSettings struct {
	RoomID             string            `json:"room_id"`
	ChannelID          string            `json:"channel_id"`
	ProjectID          string            `json:"project_id"`
	DeliveryProfile    string            `json:"delivery_profile"`
	Protection         SessionProtection `json:"protection"`
	ProtectionExplicit *bool             `json:"protection_explicit,omitempty"`
	TokenAudience      TokenAudience     `json:"token_audience"`
	Recording          RecordingSettings `json:"recording"`
	HLSOutput          bool              `json:"hls_output"`
	Ladder             *TranscodeLadder  `json:"ladder"`
	Regions            *[]string         `json:"regions"`
	MaxParticipants    *int64            `json:"max_participants"`
	AuthorizationEpoch int64             `json:"authorization_epoch"`
	UpdatedAt          string            `json:"updated_at"`
}

type RegionViewers struct {
	Region  string `json:"region"`
	Viewers int64  `json:"viewers"`
}

type LiveQoE struct {
	StallRatioPPM    *int64 `json:"stall_ratio_ppm"`
	LatencyMSP50     *int64 `json:"latency_ms_p50"`
	LatencyMSP95     *int64 `json:"latency_ms_p95"`
	PacketLossPPMP50 *int64 `json:"packet_loss_ppm_p50"`
}

type ByteTotals struct {
	Ingress int64 `json:"ingress"`
	Egress  int64 `json:"egress"`
}

type NodeFailure struct {
	NodeID string `json:"node_id"`
	AtMS   int64  `json:"at_ms"`
	Reason string `json:"reason"`
}

type MediaLiveView struct {
	SessionID       string          `json:"session_id"`
	Status          string          `json:"status"`
	Topology        *Topology       `json:"topology"`
	Participants    int64           `json:"participants"`
	ViewersByRegion []RegionViewers `json:"viewers_by_region"`
	QoE             LiveQoE         `json:"qoe"`
	Bytes           ByteTotals      `json:"bytes"`
	Failures        []NodeFailure   `json:"failures"`
	UpdatedAtMS     int64           `json:"updated_at_ms"`
}

const (
	SessionStartGraceDefaultS = 86_400
	SessionStartGraceMinS     = 600
	SessionStartGraceMaxS     = 604_800
)

const (
	SessionIdleEndMinS = 600
	SessionIdleEndMaxS = 604_800
)

var MemberClientPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

const MemberClientMaxCount = 8

type MemberTokenRequest struct {
	Role   *string `json:"role,omitempty"`
	TTLS   *int64  `json:"ttl_s,omitempty"`
	Name   *string `json:"name,omitempty"`
	Client *string `json:"client,omitempty"`
}

type PlaybackTokenRequest struct {
	Client *string `json:"client,omitempty"`
}

type MemberTokenResponse struct {
	Token        string `json:"token"`
	Role         string `json:"role"`
	SessionID    string `json:"session_id"`
	ProjectID    string `json:"project_id"`
	DirectoryURL string `json:"directory_url"`
	ExpiresAtMS  int64  `json:"expires_at_ms"`
}

type PlaybackTokenResponse struct {
	Token        string `json:"token"`
	RecordingID  string `json:"recording_id"`
	DirectoryURL string `json:"directory_url"`
	ExpiresAtMS  int64  `json:"expires_at_ms"`
}
