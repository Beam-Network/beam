package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

const (
	RoomMediaNodePathProtocol          = "beam.media.node/1"
	RoomMediaLegacyPathProtocol        = "webrtc"
	RoomMediaNodeRuntimeSchema         = mediacontracts.NodeRuntimeSchema
	RoomMediaNodeMaxHeartbeatTimeoutMS = 300_000
)

var RoomMediaNodeProfiles = []string{
	mediacontracts.ProfileInteractiveSFUV2, mediacontracts.ProfileRelayV1, mediacontracts.ProfileIngestV1,
	mediacontracts.ProfileTranscodeV1, mediacontracts.ProfilePackageLLHLSV1, mediacontracts.ProfileEdgeHTTPV1,
}

var RoomMediaNodeRoles = []string{
	mediacontracts.RoleSFU, mediacontracts.RoleRelay, mediacontracts.RoleIngest, mediacontracts.RoleTranscode,
	mediacontracts.RolePackage, mediacontracts.RoleShield, mediacontracts.RoleEdge,
}

func IsRoomMediaNodeProfile(profile string) bool {
	return slices.Contains(RoomMediaNodeProfiles, profile)
}

func RoomMediaPathProtocol(profile string) string {
	if IsRoomMediaNodeProfile(profile) {
		return RoomMediaNodePathProtocol
	}
	return RoomMediaLegacyPathProtocol
}

func RoomMediaNodeCapability(role string) string {
	return mediacontracts.RoleCapabilities[role]
}

func RoomMediaNodeCapabilities() []string {
	capabilities := make([]string, 0, len(RoomMediaNodeRoles))
	for _, role := range RoomMediaNodeRoles {
		capabilities = append(capabilities, mediacontracts.RoleCapabilities[role])
	}
	return capabilities
}

type MediaNodeRecording = mediacontracts.NodeRecording

type MediaNodeFields struct {
	NodeID             string                      `json:"node_id,omitempty"`
	Role               string                      `json:"role,omitempty"`
	Topology           json.RawMessage             `json:"topology,omitempty"`
	ServiceToken       *string                     `json:"service_token,omitempty"`
	ContentKeys        []mediacontracts.ContentKey `json:"content_keys,omitempty"`
	PlaybackSecret     *string                     `json:"playback_secret,omitempty"`
	Recording          *MediaNodeRecording         `json:"recording,omitempty"`
	BucketAccess       json.RawMessage             `json:"bucket_access,omitempty"`
	MediaSequenceFloor *int64                      `json:"media_sequence_floor,omitempty"`
	ExcludedWorkers    []string                    `json:"excluded_workers,omitempty"`
}

func (f MediaNodeFields) IsNode() bool { return f.NodeID != "" }

func (f MediaNodeFields) NodeTopology() (*mediacontracts.Topology, error) {
	if len(f.Topology) == 0 {
		return nil, errors.New("room.media node has no topology")
	}
	return mediacontracts.ValidateTopologyJSON(f.Topology)
}

func NodeEntry(topology *mediacontracts.Topology, nodeID string) (mediacontracts.TopologyNode, bool) {
	if topology == nil {
		return mediacontracts.TopologyNode{}, false
	}
	for _, node := range topology.Nodes {
		if node.NodeID == nodeID {
			return node, true
		}
	}
	return mediacontracts.TopologyNode{}, false
}

type MediaNodeIssue struct {
	Path    string
	Message string
}

func (issue *MediaNodeIssue) Error() string {
	return fmt.Sprintf("invalid room.media node workload at %s: %s", issue.Path, issue.Message)
}

func nodeIssue(path, message string) error { return &MediaNodeIssue{Path: path, Message: message} }

var (
	mediaNodeIDPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	mediaServiceTokenPattern = regexp.MustCompile(`^bmt1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{86}$`)
	mediaHex64Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	mediaHex32Pattern        = regexp.MustCompile(`^[0-9a-f]{32}$`)
	mediaRecordingIDPattern  = regexp.MustCompile(`^mrec_[0-9a-z]{26}$`)
	mediaBucketPrefixPattern = regexp.MustCompile(`^media/[A-Za-z0-9_-]{1,64}/[A-Za-z0-9_-]{1,64}/mrec_[0-9a-z]{26}/$`)
	mediaEndpointURLPattern  = regexp.MustCompile(`^(https|rtmps?|srt)://(\[[0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,7}\]|[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*)(:([1-9][0-9]{0,4}))?(/[A-Za-z0-9._~%/-]*)?$`)
	mediaEndpointSchemes     = map[string]string{"whip": "https", "whep": "https", "hls": "https", "pull": "https", "rtmp": "rtmp", "rtmps": "rtmps", "srt": "srt"}
)

var (
	mediaNodeDefinitionKeys = []string{"session_id", "profile", "node_id", "role", "topology", "service_token",
		"content_keys", "playback_secret", "recording", "heartbeat_timeout_ms"}
	mediaNodeUnitKeys  = append(slices.Clone(mediaNodeDefinitionKeys), "resume_from_sequence", "replay")
	mediaNodeNullable  = []string{"service_token", "content_keys", "playback_secret", "recording"}
	mediaLegacyOnlyKey = []string{"service", "tracks", "layers", "protection"}
)

var mediaNodeOptionalKeys = []string{"bucket_access", "media_sequence_floor", "excluded_workers"}

const RoomMediaNodeMaxExcludedWorkers = 256

var mediaWorkerIDPattern = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)

func MediaDetailsProfile(raw json.RawMessage) string {
	var value struct {
		Profile string `json:"profile"`
	}
	_ = json.Unmarshal(raw, &value)
	return value.Profile
}

func ValidateMediaNodeDetailsJSON(raw json.RawMessage, offer bool) (*mediacontracts.Topology, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, nodeIssue("details", "details must be an object")
	}
	keys := mediaNodeDefinitionKeys
	if offer {
		keys = mediaNodeUnitKeys
	}
	for key := range fields {
		if !slices.Contains(keys, key) && !slices.Contains(mediaNodeOptionalKeys, key) {
			return nil, nodeIssue("details."+key, "unknown field")
		}
	}
	for _, key := range keys {
		value, ok := fields[key]
		if !ok {
			return nil, nodeIssue("details."+key, "required")
		}
		if isJSONNull(value) && !slices.Contains(mediaNodeNullable, key) {
			return nil, nodeIssue("details."+key, "must not be null")
		}
	}
	sessionID, ok := jsonString(fields["session_id"])
	if !ok || sessionID == "" {
		return nil, nodeIssue("details.session_id", "non-empty string required")
	}
	profile, ok := jsonString(fields["profile"])
	if !ok || !IsRoomMediaNodeProfile(profile) {
		return nil, nodeIssue("details.profile", "unknown node profile")
	}
	nodeID, ok := jsonString(fields["node_id"])
	if !ok || !mediaNodeIDPattern.MatchString(nodeID) {
		return nil, nodeIssue("details.node_id", "invalid node id")
	}
	role, ok := jsonString(fields["role"])
	if !ok || !slices.Contains(RoomMediaNodeRoles, role) {
		return nil, nodeIssue("details.role", "unknown node role")
	}
	heartbeat, ok := jsonInteger(fields["heartbeat_timeout_ms"])
	if !ok || heartbeat < 1 || heartbeat > RoomMediaNodeMaxHeartbeatTimeoutMS {
		return nil, nodeIssue("details.heartbeat_timeout_ms", "integer between 1 and 300000 required")
	}
	if offer {
		if resume, ok := jsonInteger(fields["resume_from_sequence"]); !ok || resume < 0 {
			return nil, nodeIssue("details.resume_from_sequence", "non-negative integer required")
		}
		if replay, ok := jsonBool(fields["replay"]); !ok || replay {
			return nil, nodeIssue("details.replay", "media never replays")
		}
	}
	topology, err := mediacontracts.ValidateTopologyJSON(fields["topology"])
	if err != nil {
		var issue *mediacontracts.TopologyIssue
		if errors.As(err, &issue) {
			return nil, nodeIssue(joinPath("details.topology", issue.Path), issue.Message)
		}
		return nil, nodeIssue("details.topology", err.Error())
	}
	serviceToken, err := nullableString(fields["service_token"], "details.service_token")
	if err != nil {
		return nil, err
	}
	if serviceToken != nil && (len(*serviceToken) > mediacontracts.TokenMaxLength || !mediaServiceTokenPattern.MatchString(*serviceToken)) {
		return nil, nodeIssue("details.service_token", "malformed service token")
	}
	playbackSecret, err := nullableString(fields["playback_secret"], "details.playback_secret")
	if err != nil {
		return nil, err
	}
	if playbackSecret != nil && !mediaHex64Pattern.MatchString(*playbackSecret) {
		return nil, nodeIssue("details.playback_secret", "32 bytes of lowercase hex required")
	}
	contentKeys, err := decodeContentKeys(fields["content_keys"])
	if err != nil {
		return nil, err
	}
	recording, err := decodeNodeRecording(fields["recording"])
	if err != nil {
		return nil, err
	}
	access, err := decodeBucketAccess(fields["bucket_access"])
	if err != nil {
		return nil, err
	}
	var sequenceFloor *int64
	if raw, present := fields["media_sequence_floor"]; present {
		value, ok := jsonInteger(raw)
		if !ok || value < 0 || value > 9007199254740991 {
			return nil, nodeIssue("details.media_sequence_floor", "non-negative integer required")
		}
		sequenceFloor = &value
	}
	if raw, present := fields["excluded_workers"]; present {
		var workers []json.RawMessage
		if err := json.Unmarshal(raw, &workers); err != nil || workers == nil || len(workers) > RoomMediaNodeMaxExcludedWorkers {
			return nil, nodeIssue("details.excluded_workers", "an array of at most 256 Worker ids required")
		}
		for index, item := range workers {
			workerID, ok := jsonString(item)
			if !ok || !mediaWorkerIDPattern.MatchString(workerID) {
				return nil, nodeIssue(fmt.Sprintf("details.excluded_workers.%d", index), "printable Worker id required")
			}
		}
	}

	if mediacontracts.RoleProfiles[role] != profile {
		return nil, nodeIssue("details.profile", "profile does not match role")
	}
	if topology.SessionID != sessionID {
		return nil, nodeIssue("details.session_id", "session_id must be the topology's session")
	}
	node, found := NodeEntry(topology, nodeID)
	if !found || node.Role != role {
		return nil, nodeIssue("details.node_id", "node_id must name a node of this role in the topology")
	}
	if topology.View == nil || topology.View.NodeID != nodeID {
		return nil, nodeIssue("details.topology.view", "a node workload carries its own view of the topology")
	}
	if NodeHasParentRole(role, topology.DeliveryProfile) != (serviceToken != nil) {
		return nil, nodeIssue("details.service_token", "only nodes with a parent carry a service token")
	}
	if serviceToken != nil {
		claims, _, _, err := mediacontracts.ParseTokenUnverified(*serviceToken)
		if err != nil || !strings.HasPrefix(claims.KeyID, mediacontracts.ServiceKeyIDPrefix) ||
			claims.Identity != mediacontracts.NodeIdentityPrefix+nodeID || claims.Session != sessionID ||
			claims.Role != mediacontracts.RoleSubscribe {
			return nil, nodeIssue("details.service_token", "a service token is a beam-svc-* subscribe token for node:<node_id> in this session")
		}
	}
	if NodeServesHLS(role) != (playbackSecret != nil) {
		return nil, nodeIssue("details.playback_secret", "package, shield and edge nodes carry the playback secret")
	}
	protectedPackage := role == mediacontracts.RolePackage && topology.Protection.Mode == "protected"
	if protectedPackage != (contentKeys != nil) {
		return nil, nodeIssue("details.content_keys", "only a protected package node carries content keys")
	}
	vodPackage := role == mediacontracts.RolePackage && topology.DeliveryProfile == mediacontracts.DeliveryVOD
	recordingPackage := role == mediacontracts.RolePackage && topology.Packaging != nil && topology.Packaging.Recording && !vodPackage
	if (recordingPackage || vodPackage) != (recording != nil) {
		return nil, nodeIssue("details.recording", "only a recording package node and a vod package node carry the recording")
	}
	if recording != nil && recordingPackage {
		if recording.Generations != nil {
			return nil, nodeIssue("details.recording.generations", "a recording writer names its generation, not a count")
		}
		if !strings.HasSuffix(recording.BucketPrefix, "/"+sessionID+"/"+recording.RecordingID+"/") {
			return nil, nodeIssue("details.recording.bucket_prefix", "the bucket prefix ends with <session_id>/<recording_id>/")
		}
	}
	if recording != nil && vodPackage {
		if recording.RecordingID != sessionID {
			return nil, nodeIssue("details.recording.recording_id", "a vod session's id is its recording id")
		}
		if recording.Generations == nil || recording.Generation != nil {
			return nil, nodeIssue("details.recording.generations", "a vod package node names the recording's generation count")
		}
	}
	if access != nil && recording == nil {
		return nil, nodeIssue("details.bucket_access", "only a node carrying a recording gets bucket access")
	}
	if access != nil {
		prefix, methods := mediacontracts.BucketGrantFor(*recording, vodPackage)
		if access.Prefix != prefix {
			return nil, nodeIssue("details.bucket_access.prefix", "bucket access grants the writer's generation prefix or the vod recording prefix")
		}
		if strings.Join(access.Methods, ",") != strings.Join(methods, ",") {
			return nil, nodeIssue("details.bucket_access.methods", "a writer gets PUT only, a vod package GET only")
		}
	}
	if sequenceFloor != nil && !(role == mediacontracts.RolePackage && !vodPackage) {
		return nil, nodeIssue("details.media_sequence_floor", "only a live package node gets a media sequence floor")
	}
	return topology, nil
}

func NodeHasParentRole(role, deliveryProfile string) bool {
	return role != mediacontracts.RoleSFU && role != mediacontracts.RoleIngest &&
		!(role == mediacontracts.RolePackage && deliveryProfile == mediacontracts.DeliveryVOD)
}

func NodeServesHLS(role string) bool {
	return role == mediacontracts.RolePackage || role == mediacontracts.RoleShield || role == mediacontracts.RoleEdge
}

func ValidateMediaLegacyDetailsJSON(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nodeIssue("details", "details must be an object")
	}
	for key := range fields {
		if slices.Contains(mediaNodeDefinitionKeys, key) && !slices.Contains([]string{"session_id", "profile", "heartbeat_timeout_ms"}, key) {
			return nodeIssue("details."+key, "node field on a legacy media lease")
		}
	}
	return nil
}

func ValidateMediaNodeEnvelope(role string, capacity RoomCapacityRequirement, targets int, source *RoomPathAuthorization) error {
	if capacity != (RoomCapacityRequirement{Capability: RoomMediaNodeCapability(role), Units: 1}) {
		return nodeIssue("required_capacity", "a media node requires its role's capability, one unit")
	}
	if targets != 0 {
		return nodeIssue("targets", "media nodes have no targets")
	}
	if source != nil && source.Protocol != RoomMediaNodePathProtocol {
		return nodeIssue("path_authorizations.source.protocol", "media nodes use beam.media.node/1")
	}
	return nil
}

func ValidateMediaLegacyEnvelope(source *RoomPathAuthorization) error {
	if source != nil && source.Protocol != RoomMediaLegacyPathProtocol {
		return nodeIssue("path_authorizations.source.protocol", "legacy media uses webrtc")
	}
	return nil
}

func ValidateMediaLegacyProtection(raw json.RawMessage) error {
	if MediaDetailsProfile(raw) != RoomMediaWebRTCWorkerProfile {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nodeIssue("details", "details must be an object")
	}
	value, ok := fields["protection"]
	if !ok || isJSONNull(value) {
		return nodeIssue("details.protection", "WebRTC media requires channel E2EE")
	}
	return nil
}

func ValidateMediaNodeRuntime(runtime mediacontracts.NodeRuntime) error {
	if runtime.SchemaVersion != RoomMediaNodeRuntimeSchema || !slices.Contains(RoomMediaNodeCapabilities(), runtime.Capability) ||
		!mediaNodeIDPattern.MatchString(runtime.NodeID) || !printableText(runtime.WorkerID, 256) {
		return errors.New("invalid media-node-runtime/v1 identity")
	}
	key, err := mediacontracts.DecodeB64URL(runtime.NodeKey)
	if err != nil || len(key) != 32 {
		return errors.New("invalid media-node-runtime/v1 node_key")
	}
	if len(runtime.Endpoints) > 8 {
		return errors.New("media-node-runtime/v1 allows at most 8 endpoints")
	}
	role := ""
	for candidate, capability := range mediacontracts.RoleCapabilities {
		if capability == runtime.Capability {
			role = candidate
		}
	}
	kinds := map[string]bool{}
	for _, endpoint := range runtime.Endpoints {
		if !slices.Contains(mediacontracts.RoleEndpointKinds[role], endpoint.Kind) || kinds[endpoint.Kind] {
			return fmt.Errorf("invalid media-node-runtime/v1 endpoint kind %q", endpoint.Kind)
		}
		kinds[endpoint.Kind] = true
		if !validMediaEndpointURL(endpoint.Kind, endpoint.URL) {
			return fmt.Errorf("invalid media-node-runtime/v1 %s endpoint URL", endpoint.Kind)
		}
	}
	if runtime.Hostname != nil && !mediacontracts.ValidHostname(*runtime.Hostname) {
		return errors.New("invalid media-node-runtime/v1 hostname")
	}
	if runtime.TLSCertificateSHA256 != nil && !mediaHex64Pattern.MatchString(*runtime.TLSCertificateSHA256) {
		return errors.New("invalid media-node-runtime/v1 certificate pin")
	}
	if _, err := time.Parse(time.RFC3339Nano, runtime.ExpiresAt); err != nil {
		return errors.New("invalid media-node-runtime/v1 expires_at")
	}
	return nil
}

func MediaNodeRuntimeMismatch(details MediaUnitDetails, runtime *mediacontracts.NodeRuntime, legacy *MediaRuntime, workerID string) string {
	if !IsRoomMediaNodeProfile(details.Profile) {
		if runtime != nil {
			return "runtime_binding_mismatch"
		}
		return ""
	}
	if legacy != nil {
		return "runtime_binding_mismatch"
	}
	if runtime == nil {
		return ""
	}
	if runtime.SchemaVersion != RoomMediaNodeRuntimeSchema || runtime.Capability != RoomMediaNodeCapability(details.Role) ||
		runtime.NodeID != details.NodeID || runtime.WorkerID != workerID {
		return "runtime_binding_mismatch"
	}
	return ""
}

func ValidateMediaNodeLoad(load mediacontracts.NodeLoad) error {
	if load.Publishers < 0 || load.Subscribers < 0 || load.IngressBPS < 0 || load.EgressBPS < 0 ||
		load.CPURatio < 0 || load.CPURatio > 1 {
		return errors.New("invalid room.media node load")
	}
	return nil
}

func validMediaEndpointURL(kind, value string) bool {
	scheme, ok := mediaEndpointSchemes[kind]
	return ok && strings.HasPrefix(value, scheme+"://") && mediaEndpointURLPattern.MatchString(value)
}

type MediaTopologyUpdateWire struct {
	Type          string          `json:"type"`
	SchemaVersion string          `json:"schema_version"`
	Kind          domain.Kind     `json:"kind"`
	WorkloadID    string          `json:"workload_id"`
	UnitID        string          `json:"unit_id"`
	Epoch         uint64          `json:"epoch"`
	Attempt       uint64          `json:"attempt"`
	Topology      json.RawMessage `json:"topology"`
	SentAt        time.Time       `json:"sent_at"`
}

func DecodeMediaTopologyUpdate(encoded []byte) (MediaTopologyUpdateWire, *mediacontracts.Topology, error) {
	var value MediaTopologyUpdateWire
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, nil, err
	}
	if decoder.More() {
		return value, nil, errors.New("media_topology_update contains trailing JSON")
	}
	if value.Type != mediacontracts.TopologyUpdateType || value.SchemaVersion != RoomWorkloadSchema || value.Kind != domain.KindRoomMedia ||
		value.WorkloadID == "" || value.UnitID == "" || value.Epoch == 0 || value.Attempt == 0 || value.SentAt.IsZero() {
		return value, nil, errors.New("media_topology_update envelope is incomplete")
	}
	topology, err := mediacontracts.ValidateTopologyJSON(value.Topology)
	if err != nil {
		return value, nil, fmt.Errorf("media_topology_update topology: %w", err)
	}
	if topology.View == nil {
		return value, nil, errors.New("a topology update carries the node's view")
	}
	return value, topology, nil
}

func isJSONNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func jsonString(raw json.RawMessage) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func jsonBool(raw json.RawMessage) (bool, bool) {
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}

func jsonInteger(raw json.RawMessage) (int64, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return 0, false
	}
	if value, err := number.Int64(); err == nil {
		return value, true
	}
	value, err := number.Float64()
	if err != nil || value != float64(int64(value)) || value > 9007199254740991 || value < -9007199254740991 {
		return 0, false
	}
	return int64(value), true
}

func nullableString(raw json.RawMessage, path string) (*string, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	value, ok := jsonString(raw)
	if !ok {
		return nil, nodeIssue(path, "string or null required")
	}
	return &value, nil
}

func decodeContentKeys(raw json.RawMessage) ([]mediacontracts.ContentKey, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	var keys []mediacontracts.ContentKey
	if err := strictJSON(raw, &keys); err != nil || len(keys) < 1 || len(keys) > 4 {
		return nil, nodeIssue("details.content_keys", "1 to 4 content keys required")
	}
	for index, key := range keys {
		if !mediaHex32Pattern.MatchString(key.KeyID) || !mediaHex32Pattern.MatchString(key.Key) || !mediaHex32Pattern.MatchString(key.IV) {
			return nil, nodeIssue(fmt.Sprintf("details.content_keys.%d", index), "key_id, key and iv are 16 bytes of lowercase hex")
		}
	}
	return keys, nil
}

func decodeNodeRecording(raw json.RawMessage) (*MediaNodeRecording, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, nodeIssue("details.recording", "recording object or null required")
	}
	for key := range fields {
		if !slices.Contains([]string{"recording_id", "bucket_prefix", "generation", "generations"}, key) {
			return nil, nodeIssue("details.recording", "unknown field "+key)
		}
	}
	var recording MediaNodeRecording
	if value, ok := jsonString(fields["recording_id"]); !ok || !mediaRecordingIDPattern.MatchString(value) {
		return nil, nodeIssue("details.recording.recording_id", "invalid recording id")
	} else {
		recording.RecordingID = value
	}
	if value, ok := jsonString(fields["bucket_prefix"]); !ok || !mediaBucketPrefixPattern.MatchString(value) {
		return nil, nodeIssue("details.recording.bucket_prefix", "invalid bucket prefix")
	} else {
		recording.BucketPrefix = value
	}
	for _, key := range []string{"generation", "generations"} {
		value, present := fields[key]
		if !present {
			continue
		}
		number, ok := jsonInteger(value)
		if !ok || number < 1 || number > mediacontracts.RecordingMaxGenerations {
			return nil, nodeIssue("details.recording."+key, "integer between 1 and 64 required")
		}
		if key == "generation" {
			recording.Generation = &number
		} else {
			recording.Generations = &number
		}
	}
	if !strings.HasSuffix(recording.BucketPrefix, "/"+recording.RecordingID+"/") {
		return nil, nodeIssue("details.recording.bucket_prefix", "the bucket prefix ends with <recording_id>/")
	}
	if recording.Generation != nil && recording.Generations != nil {
		return nil, nodeIssue("details.recording.generations", "a writer names its generation, a vod package the generation count, not both")
	}
	return &recording, nil
}

func decodeBucketAccess(raw json.RawMessage) (*mediacontracts.BucketAccess, error) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, nil
	}
	const path = "details.bucket_access"
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, nodeIssue(path, "bucket access object or null required")
	}
	for key := range fields {
		if !slices.Contains([]string{"prefix", "methods", "signer", "credential"}, key) {
			return nil, nodeIssue(path, "unknown field "+key)
		}
	}
	var access mediacontracts.BucketAccess
	if value, ok := jsonString(fields["prefix"]); !ok || !mediacontracts.BucketGrantPrefixPattern.MatchString(value) {
		return nil, nodeIssue(path+".prefix", "a recording or generation prefix required")
	} else {
		access.Prefix = value
	}
	if err := json.Unmarshal(fields["methods"], &access.Methods); err != nil || len(access.Methods) < 1 || len(access.Methods) > 2 {
		return nil, nodeIssue(path+".methods", "one or two methods required")
	}
	for index, method := range access.Methods {
		if method != "PUT" && method != "GET" {
			return nil, nodeIssue(fmt.Sprintf("%s.methods.%d", path, index), "PUT or GET")
		}
	}
	signerRaw, ok := fields["signer"]
	if !ok {
		return nil, nodeIssue(path+".signer", "required")
	}
	if !isJSONNull(signerRaw) {
		signer, err := decodeBucketSigner(signerRaw, path+".signer")
		if err != nil {
			return nil, err
		}
		access.Signer = signer
	}
	credentialRaw, ok := fields["credential"]
	if !ok {
		return nil, nodeIssue(path+".credential", "required")
	}
	if !isJSONNull(credentialRaw) {
		credential, err := decodeBucketCredential(credentialRaw, path+".credential")
		if err != nil {
			return nil, err
		}
		access.Credential = credential
	}
	if (access.Signer == nil) == (access.Credential == nil) {
		return nil, nodeIssue(path+".signer", "exactly one of signer and credential")
	}
	if len(access.Methods) == 2 && access.Methods[0] == access.Methods[1] {
		return nil, nodeIssue(path+".methods", "unique methods")
	}
	if err := mediacontracts.ValidateBucketAccess(access); err != nil {
		return nil, nodeIssue(path, err.Error())
	}
	return &access, nil
}

func decodeBucketSigner(raw json.RawMessage, path string) (*mediacontracts.BucketSigner, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, nodeIssue(path, "signer object or null required")
	}
	for key := range fields {
		if !slices.Contains([]string{"url", "token", "expires_at_ms"}, key) {
			return nil, nodeIssue(path, "unknown field "+key)
		}
	}
	var signer mediacontracts.BucketSigner
	var ok bool
	if signer.URL, ok = jsonString(fields["url"]); !ok || !httpsURLString(signer.URL) {
		return nil, nodeIssue(path+".url", "https URL required")
	}
	if signer.Token, ok = jsonString(fields["token"]); !ok || signer.Token == "" || len(signer.Token) > 4096 {
		return nil, nodeIssue(path+".token", "1 to 4096 characters required")
	}
	if signer.ExpiresAtMS, ok = jsonInteger(fields["expires_at_ms"]); !ok || signer.ExpiresAtMS < 1 {
		return nil, nodeIssue(path+".expires_at_ms", "positive integer required")
	}
	return &signer, nil
}

func decodeBucketCredential(raw json.RawMessage, path string) (*mediacontracts.BucketCredential, error) {
	var credential mediacontracts.BucketCredential
	if err := strictJSON(raw, &credential); err != nil {
		return nil, nodeIssue(path, "endpoint, bucket, region, access_key_id, secret_access_key, session_token and expires_at_ms required")
	}
	if !httpsURLString(credential.Endpoint) {
		return nil, nodeIssue(path+".endpoint", "https URL required")
	}
	return &credential, nil
}

func httpsURLString(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

func strictJSON(raw json.RawMessage, target any) error {
	return mediacontracts.UnmarshalStrict(raw, target)
}

func joinPath(prefix, path string) string {
	if path == "" {
		return prefix
	}
	return prefix + "." + path
}

func printableText(value string, maxLength int) bool {
	if value == "" || len(value) > maxLength {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func ValidateRoomMediaMessage(messageType string, encoded []byte, now time.Time) error {
	switch messageType {
	case mediacontracts.NodeOfferRequestType:
		return validateMediaNodeOfferRequest(encoded)
	case mediacontracts.NodeOfferRequestAckType:
		return validateMediaNodeOfferAck(encoded, now)
	case RoomWorkloadResultType:
		return validateMediaResultMessage(encoded)
	case RoomWorkloadOffer:
		var value RoomWorkloadOfferWire[MediaUnitDetails]
		if err := decodeStrictMedia(encoded, &value); err != nil {
			return decodeIssue(err)
		}
		raw, err := rawDetails(encoded)
		if err != nil {
			return err
		}
		if value.Kind != domain.KindRoomMedia {
			return errors.New("room.media message has another kind")
		}
		if IsRoomMediaNodeProfile(MediaDetailsProfile(raw)) {
			if _, err := ValidateMediaNodeDetailsJSON(raw, true); err != nil {
				return err
			}
			if err := ValidateMediaNodeEnvelope(value.Details.Role, value.RequiredCapacity, len(value.DestinationSnapshot.Targets),
				&value.PathAuthorizations.Source); err != nil {
				return err
			}
			if want := fmt.Sprintf("%s/epoch-%d/attempt-%d/source", value.UnitID, value.Epoch, value.Attempt); value.PathAuthorizations.Source.PathID != want {
				return nodeIssue("path_authorizations.source.path_id", "node path ids keep the room.media scheme "+want)
			}
			return value.ValidateProtocol(now, true, RoomMediaNodePathProtocol)
		}
		if err := validateLegacyMediaRaw(raw, []string{"session_id", "resume_from_sequence", "replay", "service", "tracks", "layers"}); err != nil {
			return err
		}
		if value.Details.Replay {
			return nodeIssue("details.replay", "media never replays")
		}
		return value.Validate(now, value.Details.Profile == RoomMediaWebRTCWorkerProfile)
	case RoomWorkloadSubmit:
		var value RoomWorkloadSubmitWire[MediaDefinitionDetails]
		if err := decodeStrictMedia(encoded, &value); err != nil {
			return decodeIssue(err)
		}
		raw, err := rawDetails(encoded)
		if err != nil {
			return err
		}
		if value.Kind != domain.KindRoomMedia {
			return errors.New("room.media message has another kind")
		}
		if IsRoomMediaNodeProfile(MediaDetailsProfile(raw)) {
			if _, err := ValidateMediaNodeDetailsJSON(raw, false); err != nil {
				return err
			}
			if err := ValidateMediaNodeEnvelope(value.Details.Role, value.RequiredCapacity, len(value.Targets), nil); err != nil {
				return err
			}
			return value.Validate(now, true)
		}
		if err := validateLegacyMediaRaw(raw, []string{"session_id", "service", "tracks", "layers", "heartbeat_timeout_ms"}); err != nil {
			return err
		}
		return value.Validate(now, value.Details.Profile == RoomMediaWebRTCWorkerProfile)
	case RoomWorkloadProgressType:
		var value RoomWorkloadProgressWire[MediaProgressDetails]
		if err := decodeStrictMedia(encoded, &value); err != nil {
			return decodeIssue(err)
		}
		if value.Type != RoomWorkloadProgressType || value.SchemaVersion != RoomWorkloadSchema || value.Kind != domain.KindRoomMedia ||
			value.WorkloadID == "" || value.UnitID == "" || value.Epoch == 0 || value.Attempt == 0 || value.WorkerID == "" ||
			value.ProgressID == "" || value.ReportedAt.IsZero() {
			return errors.New("room.media progress envelope is incomplete")
		}
		details := value.Details
		if details.Packets < 0 || details.Bytes < 0 || details.Dropped < 0 || details.Tracks == nil || len(details.Tracks) > 128 {
			return nodeIssue("details", "invalid room.media progress counters")
		}
		if details.Load != nil {
			if err := ValidateMediaNodeLoad(*details.Load); err != nil {
				return nodeIssue("details.load", err.Error())
			}
		}
		if details.NodeRuntime != nil && details.NodeRuntime.WorkerID != value.WorkerID {
			return nodeIssue("details.runtime.worker_id", "a node runtime names the reporting Worker")
		}
		return nil
	case mediacontracts.TopologyUpdateType:
		_, _, err := DecodeMediaTopologyUpdate(encoded)
		return err
	default:
		return fmt.Errorf("unsupported room.media message %q", messageType)
	}
}

func decodeIssue(err error) error {
	var issue *MediaNodeIssue
	if errors.As(err, &issue) {
		return err
	}
	const unknown = `json: unknown field "`
	if message := err.Error(); strings.HasPrefix(message, unknown) {
		return nodeIssue(strings.TrimSuffix(strings.TrimPrefix(message, unknown), `"`), "unknown field")
	}
	return err
}

func rawDetails(encoded []byte) (json.RawMessage, error) {
	var envelope struct {
		Details json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil || len(envelope.Details) == 0 {
		return nil, nodeIssue("details", "details are required")
	}
	return envelope.Details, nil
}

func validateLegacyMediaRaw(raw json.RawMessage, required []string) error {
	if err := ValidateMediaLegacyDetailsJSON(raw); err != nil {
		return err
	}
	if err := ValidateMediaLegacyProtection(raw); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nodeIssue("details", "details must be an object")
	}
	for _, key := range required {
		if value, ok := fields[key]; !ok || isJSONNull(value) {
			return nodeIssue("details."+key, "required")
		}
	}
	return nil
}

var MediaTerminalReasons = []string{"ended", "source_closed", "worker_failed", "expired", MediaPackagerStateLost, MediaRecordingStateLost}

const (
	MediaPackagerStateLost  = "packager_state_lost"
	MediaRecordingStateLost = "recording_state_lost"
)

func MediaStateLostReason(message string) string {
	for _, reason := range []string{MediaPackagerStateLost, MediaRecordingStateLost} {
		if message == reason || strings.HasPrefix(message, reason+":") {
			return reason
		}
	}
	return ""
}

func validateMediaNodeOfferRequest(encoded []byte) error {
	var request mediacontracts.MediaNodeOfferRequest
	if err := decodeStrictMedia(encoded, &request); err != nil {
		return decodeIssue(err)
	}
	switch {
	case request.Type != mediacontracts.NodeOfferRequestType:
		return nodeIssue("type", "not a media_node_offer_request")
	case request.SchemaVersion != RoomWorkloadSchema:
		return nodeIssue("schema_version", "unsupported schema version")
	case request.Kind != string(domain.KindRoomMedia):
		return nodeIssue("kind", "a fresh offer is a room.media node offer")
	case !slices.Contains(mediacontracts.MediaNodeOfferRequestReasons, request.Reason):
		return nodeIssue("reason", "not a listed offer request reason")
	}
	if err := mediacontracts.ValidateMediaNodeOfferRequest(request); err != nil {
		return nodeIssue("", err.Error())
	}
	return nil
}

func validateMediaNodeOfferAck(encoded []byte, now time.Time) error {
	var ack mediacontracts.MediaAck
	if err := decodeStrictMedia(encoded, &ack); err != nil {
		return decodeIssue(err)
	}
	if ack.Type != mediacontracts.NodeOfferRequestAckType {
		return nodeIssue("type", "not a media_node_offer_request_ack")
	}
	if ack.Accepted && len(ack.Offer) == 0 {
		return nodeIssue("offer", "an accepted offer request carries the fresh offer")
	}
	if err := mediacontracts.ValidateMediaAck(ack); err != nil {
		return nodeIssue("", err.Error())
	}
	if !ack.Accepted {
		if len(ack.Offer) != 0 {
			return nodeIssue("offer", "a refused offer request carries no offer")
		}
		return nil
	}
	if !IsRoomMediaNodeProfile(MediaDetailsProfile(offerDetails(ack.Offer))) {
		return nodeIssue("offer.details.profile", "a fresh offer is a node workload offer")
	}
	if err := ValidateRoomMediaMessage(RoomWorkloadOffer, ack.Offer, now); err != nil {
		var issue *MediaNodeIssue
		if errors.As(err, &issue) {
			path := issue.Path
			if strings.HasPrefix(path, "details.") {
				path = "details"
			}
			return nodeIssue(strings.TrimSuffix("offer."+path, "."), issue.Message)
		}
		return nodeIssue("offer", err.Error())
	}
	return nil
}

func offerDetails(encoded []byte) json.RawMessage {
	raw, err := rawDetails(encoded)
	if err != nil {
		return nil
	}
	return raw
}

func validateMediaResultMessage(encoded []byte) error {
	var value RoomWorkloadResultWire[MediaResultDetails]
	if err := decodeStrictMedia(encoded, &value); err != nil {
		return decodeIssue(err)
	}
	if value.Type != RoomWorkloadResultType || value.SchemaVersion != RoomWorkloadSchema || value.Kind != domain.KindRoomMedia ||
		value.WorkloadID == "" || value.UnitID == "" || value.Epoch == 0 || value.Attempt == 0 || value.WorkerID == "" ||
		value.ResultID == "" || value.ReportedAt.IsZero() || value.Failures == nil {
		return errors.New("room.media result envelope is incomplete")
	}
	details := value.Details
	if details.Packets < 0 || details.Bytes < 0 || details.Dropped < 0 || details.Tracks == nil || len(details.Tracks) > 128 {
		return nodeIssue("details", "invalid room.media result counters")
	}
	if !slices.Contains(MediaTerminalReasons, details.TerminalReason) {
		return nodeIssue("details.terminal_reason", "not a listed room.media terminal reason")
	}
	for index, failure := range value.Failures {
		if failure.Origin == "" || failure.Code == "" {
			return nodeIssue(fmt.Sprintf("failures.%d", index), "a failure names its origin and code")
		}
	}
	return nil
}
