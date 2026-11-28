package mediacontracts

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const TopologySchemaVersion = "media-topology/v1"

const (
	DeliveryInteractive = "interactive"
	DeliveryStage       = "stage"
	DeliveryBroadcast   = "broadcast"
	DeliveryVOD         = "vod"
)

const (
	TopologyMaxDenyNonces     = 2000
	TopologyMaxDenyIdentities = 500
	TopologyMaxChildren       = 256
)

const (
	RoleSFU       = "sfu"
	RoleRelay     = "relay"
	RoleIngest    = "ingest"
	RoleTranscode = "transcode"
	RolePackage   = "package"
	RoleShield    = "shield"
	RoleEdge      = "edge"
)

const (
	ProfileLegacy           = "legacy"
	ProfileWebRTCWorkerV1   = "webrtc-worker-v1"
	ProfileInteractiveSFUV2 = "interactive-sfu-v2"
	ProfileRelayV1          = "relay-v1"
	ProfileIngestV1         = "ingest-v1"
	ProfileTranscodeV1      = "transcode-v1"
	ProfilePackageLLHLSV1   = "package-llhls-v1"
	ProfileEdgeHTTPV1       = "edge-http-v1"
)

var RoleProfiles = map[string]string{
	RoleSFU: ProfileInteractiveSFUV2, RoleRelay: ProfileRelayV1, RoleIngest: ProfileIngestV1,
	RoleTranscode: ProfileTranscodeV1, RolePackage: ProfilePackageLLHLSV1, RoleShield: ProfileEdgeHTTPV1,
	RoleEdge: ProfileEdgeHTTPV1,
}

var RoleCapabilities = map[string]string{
	RoleSFU: "room.media.sfu.v2", RoleRelay: "room.media.relay.v1", RoleIngest: "room.media.ingest.v1",
	RoleTranscode: "room.media.transcode.v1", RolePackage: "room.media.package.v1",
	RoleShield: "room.media.shield.v1", RoleEdge: "room.media.edge.v1",
}

var WorkerRoles = map[string]string{
	RoleSFU: "media.sfu", RoleRelay: "media.relay", RoleIngest: "media.ingest", RoleTranscode: "media.transcode",
	RolePackage: "media.package", RoleShield: "media.shield", RoleEdge: "media.edge",
}

var RoleEndpointKinds = map[string][]string{
	RoleSFU: {"whip", "whep"}, RoleRelay: {"whep"}, RoleIngest: {"whip", "rtmp", "rtmps", "srt", "pull"},
	RoleTranscode: {"pull"}, RolePackage: {"hls"}, RoleShield: {"hls"}, RoleEdge: {"hls"},
}

var LinkRoles = map[string][]string{
	RoleSFU: {RoleRelay, RoleTranscode}, RoleRelay: {RoleRelay}, RoleIngest: {RoleTranscode},
	RoleTranscode: {RolePackage}, RolePackage: {RoleShield, RoleEdge}, RoleShield: {RoleEdge},
}

var endpointSchemes = map[string]string{"whip": "https", "whep": "https", "hls": "https", "pull": "https", "rtmp": "rtmp", "rtmps": "rtmps", "srt": "srt"}

var (
	topologyIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	printablePattern   = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
	nodeIDPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	renditionIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	streamKeyIDPattern = regexp.MustCompile(`^msk_[0-9a-z]{26}$`)
	sha256HexPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	publicKeyPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	endpointURLPattern = regexp.MustCompile(`^(https|rtmps?|srt)://(\[[0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,7}\]|[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*)(:([1-9][0-9]{0,4}))?(/[A-Za-z0-9._~%/-]*)?$`)
)

type Topology struct {
	SchemaVersion   string             `json:"schema_version"`
	SessionID       string             `json:"session_id"`
	ProjectID       *string            `json:"project_id"`
	Room            *TopologyRoom      `json:"room"`
	DeliveryProfile string             `json:"delivery_profile"`
	Epoch           int64              `json:"epoch"`
	Revision        int64              `json:"revision"`
	IssuedAtMS      int64              `json:"issued_at_ms"`
	ExpiresAtMS     int64              `json:"expires_at_ms"`
	Protection      TopologyProtection `json:"protection"`
	Tokens          TopologyTokens     `json:"tokens"`
	Nodes           []TopologyNode     `json:"nodes"`
	Links           []TopologyLink     `json:"links"`
	Ladder          *TranscodeLadder   `json:"ladder"`
	Packaging       *TopologyPackaging `json:"packaging"`
	Ingest          *TopologyIngest    `json:"ingest"`
	View            *TopologyView      `json:"view"`
	Usage           *UsageSettings     `json:"usage,omitempty"`
}

type UsageSettings struct {
	SealKeys  []UsageSealKey    `json:"seal_keys"`
	GrantKeys []ReceiptGrantKey `json:"grant_keys"`
	Receipts  UsageReceipts     `json:"receipts"`
}

type UsageSealKey struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

type ReceiptGrantKey struct {
	KeyID     string `json:"kid"`
	PublicKey string `json:"public_key"`
}

type UsageReceipts struct {
	Enforce []string `json:"enforce"`
}

const (
	UsageSealKeysMax  = 4
	UsageGrantKeysMax = 4
)

var ReceiptTransports = []string{"whip", "whep", "hls"}

var (
	SealKeyIDPattern         = regexp.MustCompile(`^usk-[a-z0-9-]{1,48}$`)
	ReceiptGrantKeyIDPattern = regexp.MustCompile(`^rgk-[a-z0-9-]{1,48}$`)
)

type TopologyView struct {
	NodeID string `json:"node_id"`
}

type TopologyRoom struct {
	RoomID    string `json:"room_id"`
	ChannelID string `json:"channel_id"`
}

type TopologyProtection struct {
	Mode              string  `json:"mode"`
	FrameScheme       *string `json:"frame_scheme"`
	KeyProvider       *string `json:"key_provider"`
	SegmentEncryption *string `json:"segment_encryption"`
}

type TopologyTokens struct {
	Audience string        `json:"audience"`
	Keys     []TokenKey    `json:"keys"`
	Deny     TokenDenyList `json:"deny"`
}

type TopologyEndpoint struct {
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

type TopologyNode struct {
	NodeID               string             `json:"node_id"`
	Role                 string             `json:"role"`
	Profile              string             `json:"profile"`
	WorkloadID           *string            `json:"workload_id"`
	OrchestratorHotkey   *string            `json:"orchestrator_hotkey"`
	WorkerID             *string            `json:"worker_id"`
	Region               string             `json:"region"`
	State                string             `json:"state"`
	Endpoints            []TopologyEndpoint `json:"endpoints"`
	NodeKey              *string            `json:"node_key"`
	Hostname             *string            `json:"hostname"`
	TLSCertificateSHA256 *string            `json:"tls_certificate_sha256"`
}

type TopologyLink struct {
	Parent string `json:"parent"`
	Child  string `json:"child"`
}

type VideoRendition struct {
	RenditionID string `json:"rendition_id"`
	Codec       string `json:"codec"`
	Width       int64  `json:"width"`
	Height      int64  `json:"height"`
	FPSMax      int64  `json:"fps_max"`
	BitrateBPS  int64  `json:"bitrate_bps"`
	Passthrough bool   `json:"passthrough,omitempty"`
}

type AudioRendition struct {
	RenditionID string `json:"rendition_id"`
	Codec       string `json:"codec"`
	BitrateBPS  int64  `json:"bitrate_bps"`
	Channels    int64  `json:"channels"`
	SampleRate  int64  `json:"sample_rate"`
}

type TranscodeLadder struct {
	Video []VideoRendition `json:"video"`
	Audio []AudioRendition `json:"audio"`
	GOPMS int64            `json:"gop_ms"`
}

type TopologyPackaging struct {
	SegmentMS  int64 `json:"segment_ms"`
	PartMS     int64 `json:"part_ms"`
	DVRWindowS int64 `json:"dvr_window_s"`
	Recording  bool  `json:"recording"`
}

type TopologyStreamKey struct {
	KeyID   string `json:"key_id"`
	SHA256  string `json:"sha256"`
	Channel bool   `json:"channel,omitempty"`
}

type TopologyIngest struct {
	Protocols  []string            `json:"protocols"`
	StreamKeys []TopologyStreamKey `json:"stream_keys"`
	ChannelID  *string             `json:"channel_id,omitempty"`
}

type TopologyIssue struct {
	Path    string
	Message string
}

func (issue *TopologyIssue) Error() string {
	return "media-topology/v1: " + issue.Path + ": " + issue.Message
}

type tv struct{ issue *TopologyIssue }

func (v *tv) fail(path, message string) bool {
	if v.issue == nil {
		v.issue = &TopologyIssue{Path: path, Message: message}
	}
	return false
}

func join(path string, key any) string {
	part := fmt.Sprint(key)
	if path == "" {
		return part
	}
	return path + "." + part
}

func (v *tv) object(path string, value any, keys ...string) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, v.fail(path, "expected object")
	}
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
		if _, present := object[key]; !present {
			return nil, v.fail(join(path, key), "required")
		}
	}
	for key := range object {
		if !allowed[key] {
			return nil, v.fail(path, "unrecognized key "+key)
		}
	}
	return object, true
}

func (v *tv) objectOptional(path string, value any, required, optional []string) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, v.fail(path, "expected object")
	}
	allowed := map[string]bool{}
	for _, key := range optional {
		allowed[key] = true
	}
	for _, key := range required {
		allowed[key] = true
		if _, present := object[key]; !present {
			return nil, v.fail(join(path, key), "required")
		}
	}
	for key := range object {
		if !allowed[key] {
			return nil, v.fail(path, "unrecognized key "+key)
		}
	}
	return object, true
}

func (v *tv) str(path string, value any, pattern *regexp.Regexp) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", v.fail(path, "expected string")
	}
	if pattern != nil && !pattern.MatchString(text) {
		return "", v.fail(path, "invalid format")
	}
	return text, true
}

func (v *tv) nullableStr(path string, value any, pattern *regexp.Regexp) (*string, bool) {
	if value == nil {
		return nil, true
	}
	text, ok := v.str(path, value, pattern)
	return &text, ok
}

func (v *tv) enum(path string, value any, options ...string) (string, bool) {
	text, ok := value.(string)
	if !ok || !contains(options, text) {
		return "", v.fail(path, "invalid enum value")
	}
	return text, true
}

func (v *tv) integer(path string, value any, min, max int64) (int64, bool) {
	number, ok := jsonInteger(value)
	if !ok || number < min || number > max {
		return 0, v.fail(path, "expected integer in range")
	}
	return number, true
}

func (v *tv) array(path string, value any, min, max int) ([]any, bool) {
	list, ok := value.([]any)
	if !ok {
		return nil, v.fail(path, "expected array")
	}
	if len(list) < min || len(list) > max {
		return nil, v.fail(path, "array length out of range")
	}
	return list, true
}

func validEndpointURL(kind, value string) bool {
	match := endpointURLPattern.FindStringSubmatch(value)
	if match == nil || !strings.HasPrefix(value, endpointSchemes[kind]+"://") {
		return false
	}
	if port := match[7]; port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number > 65535 {
			return false
		}
	}
	return true
}

const maxSafe = 9007199254740991

func ValidateTopologyJSON(data []byte) (*Topology, error) {
	object, err := decodeObject(data)
	if err != nil {
		return nil, &TopologyIssue{Path: "", Message: "not a JSON object"}
	}
	return ValidateTopology(object)
}

func ValidateTopology(value any) (*Topology, error) {
	v := &tv{}
	if !v.validateTopology(value) {
		return nil, v.issue
	}
	data, err := json.Marshal(integerize(value))
	if err != nil {
		return nil, err
	}
	var topology Topology
	if err := json.Unmarshal(data, &topology); err != nil {
		return nil, err
	}
	return &topology, nil
}

func (v *tv) validateTopology(value any) bool {
	root, ok := v.objectOptional("", value, []string{"schema_version", "session_id", "project_id", "room", "delivery_profile", "epoch",
		"revision", "issued_at_ms", "expires_at_ms", "protection", "tokens", "nodes", "links", "ladder", "packaging", "ingest", "view"}, []string{"usage"})
	if !ok {
		return false
	}
	if _, ok := v.enum("schema_version", root["schema_version"], TopologySchemaVersion); !ok {
		return false
	}
	if _, ok := v.str("session_id", root["session_id"], topologyIDPattern); !ok {
		return false
	}
	projectID, ok := v.nullableStr("project_id", root["project_id"], topologyIDPattern)
	if !ok {
		return false
	}
	if root["room"] != nil {
		room, ok := v.object("room", root["room"], "room_id", "channel_id")
		if !ok {
			return false
		}
		if _, ok := v.str("room.room_id", room["room_id"], printablePattern); !ok {
			return false
		}
		if _, ok := v.str("room.channel_id", room["channel_id"], printablePattern); !ok {
			return false
		}
	}
	delivery, ok := v.enum("delivery_profile", root["delivery_profile"], DeliveryInteractive, DeliveryStage, DeliveryBroadcast, DeliveryVOD)
	if !ok {
		return false
	}
	if _, ok := v.integer("epoch", root["epoch"], 1, maxSafe); !ok {
		return false
	}
	if _, ok := v.integer("revision", root["revision"], 1, maxSafe); !ok {
		return false
	}
	issuedAt, ok := v.integer("issued_at_ms", root["issued_at_ms"], 0, maxSafe)
	if !ok {
		return false
	}
	expiresAt, ok := v.integer("expires_at_ms", root["expires_at_ms"], 0, maxSafe)
	if !ok {
		return false
	}
	protection, ok := v.validateProtection(root["protection"])
	if !ok {
		return false
	}
	if !v.validateTokens(root["tokens"]) {
		return false
	}
	nodes, ok := v.validateNodes(root["nodes"])
	if !ok {
		return false
	}
	links, ok := v.validateLinks(root["links"])
	if !ok {
		return false
	}
	ladder, ok := v.validateLadder(root["ladder"])
	if !ok {
		return false
	}
	packaging, ok := v.validatePackaging(root["packaging"])
	if !ok {
		return false
	}
	if root["ingest"] != nil && !v.validateIngest(root["ingest"]) {
		return false
	}
	viewNode := ""
	if root["view"] != nil {
		view, ok := v.object("view", root["view"], "node_id")
		if !ok {
			return false
		}
		if viewNode, ok = v.str("view.node_id", view["node_id"], nodeIDPattern); !ok {
			return false
		}
	}

	usage, hasUsage := root["usage"]
	if hasUsage && !v.validateUsage(usage) {
		return false
	}

	if projectID == nil && root["room"] == nil {
		return v.fail("project_id", "a topology names a project, a room or both")
	}
	if expiresAt <= issuedAt {
		return v.fail("expires_at_ms", "expires_at_ms must be after issued_at_ms")
	}
	byID := map[string]string{}
	counts := map[string]int{}
	for index, node := range nodes {
		if _, duplicate := byID[node.id]; duplicate {
			return v.fail(join(join("nodes", index), "node_id"), "duplicate node_id")
		}
		byID[node.id] = node.role
		counts[node.role]++
	}
	parents := map[string]string{}
	children := map[string]int{}
	for index, link := range links {
		path := join("links", index)
		parentRole, parentOK := byID[link.parent]
		if !parentOK {
			return v.fail(join(path, "parent"), "unknown node")
		}
		childRole, childOK := byID[link.child]
		if !childOK {
			return v.fail(join(path, "child"), "unknown node")
		}
		if link.parent == link.child || !contains(LinkRoles[parentRole], childRole) {
			return v.fail(join(path, "child"), "link not allowed between these roles")
		}
		if _, has := parents[link.child]; has {
			return v.fail(join(path, "child"), "node has two parents")
		}
		parents[link.child] = link.parent
		children[link.parent]++
		if children[link.parent] > TopologyMaxChildren {
			return v.fail(join(path, "parent"), "a node has at most 256 children")
		}
	}
	for index, link := range links {
		seen := map[string]bool{link.child: true}
		for current := link.parent; current != ""; current = parents[current] {
			if seen[current] {
				return v.fail(join(join("links", index), "child"), "links form a cycle")
			}
			seen[current] = true
		}
	}
	if viewNode != "" {
		if _, present := byID[viewNode]; !present {
			return v.fail("view.node_id", "the viewed node is in the view")
		}
		for index, node := range nodes {
			if node.id != viewNode && parents[viewNode] != node.id && parents[node.id] != viewNode {
				return v.fail(join(join("nodes", index), "node_id"), "a view holds the node, its parent and its children only")
			}
		}
		for index, link := range links {
			if link.parent != viewNode && link.child != viewNode {
				return v.fail(join("links", index), "a view holds the links of the viewed node only")
			}
		}
	}
	if hasUsage && viewNode == "" {
		return v.fail("usage", "usage settings are for a node's view")
	}
	switch delivery {
	case DeliveryInteractive:
		if viewNode == "" && counts[RoleSFU] != 1 || counts[RoleSFU] > 1 || counts[RoleRelay] != 0 || counts[RoleIngest] != 0 {
			return v.fail("nodes", "interactive: one sfu, no relay, no ingest")
		}
	case DeliveryStage:
		if viewNode == "" && counts[RoleSFU] != 1 || counts[RoleSFU] > 1 || counts[RoleIngest] != 0 {
			return v.fail("nodes", "stage: one sfu, no ingest")
		}
	case DeliveryBroadcast:
		if viewNode == "" && (counts[RoleIngest] != 1 || counts[RoleTranscode] < 1 || counts[RolePackage] != 1) ||
			counts[RoleSFU] != 0 || counts[RoleRelay] != 0 {
			return v.fail("nodes", "broadcast: one ingest, at least one transcode, one package, no sfu or relay")
		}
	case DeliveryVOD:
		if counts[RoleSFU]+counts[RoleRelay]+counts[RoleIngest]+counts[RoleTranscode] > 0 || viewNode == "" && counts[RolePackage] != 1 {
			return v.fail("nodes", "vod: one package, shields and edges only")
		}
	}
	if counts[RolePackage] > 1 {
		return v.fail("nodes", "at most one package node")
	}
	if (counts[RoleTranscode] > 0 || counts[RolePackage] > 0) && ladder == nil {
		return v.fail("ladder", "transcode and package nodes need a ladder")
	}
	if ladder != nil && ladder.source && delivery != DeliveryBroadcast && delivery != DeliveryVOD {
		return v.fail("ladder", "a source ladder is for broadcasts and their recordings")
	}
	if counts[RolePackage] > 0 && packaging == nil {
		return v.fail("packaging", "a package node needs packaging")
	}
	if counts[RoleIngest] > 0 && root["ingest"] == nil {
		return v.fail("ingest", "an ingest node needs ingest settings")
	}
	if ladder != nil && packaging != nil && packaging.segmentMS%ladder.gopMS != 0 {
		return v.fail("packaging.segment_ms", "segment_ms must be a multiple of gop_ms")
	}
	switch protection {
	case "e2ee":
		if counts[RoleIngest]+counts[RoleTranscode]+counts[RolePackage]+counts[RoleShield]+counts[RoleEdge] > 0 {
			return v.fail("protection.mode", "e2ee sessions have no ingest, transcode, package, shield or edge node")
		}
	case "protected":
		if viewNode == "" && counts[RolePackage] == 0 {
			return v.fail("protection.mode", "protected needs a package node")
		}
	}
	return true
}

func (v *tv) validateProtection(value any) (string, bool) {
	protection, ok := v.object("protection", value, "mode", "frame_scheme", "key_provider", "segment_encryption")
	if !ok {
		return "", false
	}
	mode, ok := v.enum("protection.mode", protection["mode"], "e2ee", "public", "protected")
	if !ok {
		return "", false
	}
	frame, ok := v.nullableStr("protection.frame_scheme", protection["frame_scheme"], nil)
	if !ok {
		return "", false
	}
	if frame != nil && *frame != SFrameScheme {
		return "", v.fail("protection.frame_scheme", "invalid enum value")
	}
	provider, ok := v.nullableStr("protection.key_provider", protection["key_provider"], nil)
	if !ok {
		return "", false
	}
	if provider != nil && *provider != "mls" && *provider != "customer" {
		return "", v.fail("protection.key_provider", "invalid enum value")
	}
	segment, ok := v.nullableStr("protection.segment_encryption", protection["segment_encryption"], nil)
	if !ok {
		return "", false
	}
	if segment != nil && *segment != "cbcs" {
		return "", v.fail("protection.segment_encryption", "invalid enum value")
	}
	switch mode {
	case "e2ee":
		if frame == nil {
			return "", v.fail("protection.frame_scheme", "e2ee needs beam-sframe-v1")
		}
		if provider == nil {
			return "", v.fail("protection.key_provider", "e2ee needs a key provider")
		}
		if segment != nil {
			return "", v.fail("protection.segment_encryption", "e2ee has no segment encryption")
		}
	case "public":
		if frame != nil {
			return "", v.fail("protection.frame_scheme", "public has no frame scheme")
		}
		if provider != nil {
			return "", v.fail("protection.key_provider", "public has no key provider")
		}
		if segment != nil {
			return "", v.fail("protection.segment_encryption", "public has no segment encryption")
		}
	case "protected":
		if frame != nil {
			return "", v.fail("protection.frame_scheme", "protected has no frame scheme")
		}
		if provider != nil {
			return "", v.fail("protection.key_provider", "protected has no key provider")
		}
		if segment == nil {
			return "", v.fail("protection.segment_encryption", "protected needs cbcs")
		}
	}
	return mode, true
}

func (v *tv) validateTokens(value any) bool {
	tokens, ok := v.object("tokens", value, "audience", "keys", "deny")
	if !ok {
		return false
	}
	if _, ok := v.str("tokens.audience", tokens["audience"], tokenAudiencePattern); !ok {
		return false
	}
	keys, ok := v.array("tokens.keys", tokens["keys"], 1, 16)
	if !ok {
		return false
	}
	kids := map[string]bool{}
	for index, raw := range keys {
		path := join("tokens.keys", index)
		key, ok := v.objectOptional(path, raw, []string{"kid", "public_key"}, []string{"max_role"})
		if !ok {
			return false
		}
		if raw, present := key["max_role"]; present {
			if _, ok := v.enum(join(path, "max_role"), raw, RoleSubscribe, RolePublish, RoleAdmin); !ok {
				return false
			}
		}
		kid, ok := v.str(join(path, "kid"), key["kid"], tokenKeyIDPattern)
		if !ok {
			return false
		}
		publicKey, ok := v.str(join(path, "public_key"), key["public_key"], publicKeyPattern)
		if !ok {
			return false
		}
		if decoded, err := DecodeB64URL(publicKey); err != nil || len(decoded) != 32 {
			return v.fail(join(path, "public_key"), "invalid format")
		}
		if kids[kid] {
			return v.fail(join(path, "kid"), "duplicate kid")
		}
		kids[kid] = true
	}
	deny, ok := v.object("tokens.deny", tokens["deny"], "nonces", "identities")
	if !ok {
		return false
	}
	nonces, ok := v.array("tokens.deny.nonces", deny["nonces"], 0, TopologyMaxDenyNonces)
	if !ok {
		return false
	}
	for index, raw := range nonces {
		path := join("tokens.deny.nonces", index)
		entry, ok := v.object(path, raw, "nonce", "exp")
		if !ok {
			return false
		}
		if _, ok := v.str(join(path, "nonce"), entry["nonce"], tokenNoncePattern); !ok {
			return false
		}
		if _, ok := v.integer(join(path, "exp"), entry["exp"], 0, maxSafe); !ok {
			return false
		}
	}
	identities, ok := v.array("tokens.deny.identities", deny["identities"], 0, TopologyMaxDenyIdentities)
	if !ok {
		return false
	}
	for index, raw := range identities {
		path := join("tokens.deny.identities", index)
		entry, ok := v.object(path, raw, "identity", "until")
		if !ok {
			return false
		}
		if _, ok := v.str(join(path, "identity"), entry["identity"], tokenIdentityPattern); !ok {
			return false
		}
		if _, ok := v.integer(join(path, "until"), entry["until"], 0, maxSafe); !ok {
			return false
		}
	}
	return true
}

type nodeSummary struct{ id, role string }

func (v *tv) validateNodes(value any) ([]nodeSummary, bool) {
	list, ok := v.array("nodes", value, 1, 1024)
	if !ok {
		return nil, false
	}
	roles := []string{RoleSFU, RoleRelay, RoleIngest, RoleTranscode, RolePackage, RoleShield, RoleEdge}
	profiles := []string{ProfileInteractiveSFUV2, ProfileRelayV1, ProfileIngestV1, ProfileTranscodeV1,
		ProfilePackageLLHLSV1, ProfileEdgeHTTPV1}
	summaries := make([]nodeSummary, 0, len(list))
	for index, raw := range list {
		path := join("nodes", index)
		node, ok := v.object(path, raw, "node_id", "role", "profile", "workload_id", "orchestrator_hotkey", "worker_id",
			"region", "state", "endpoints", "node_key", "hostname", "tls_certificate_sha256")
		if !ok {
			return nil, false
		}
		id, ok := v.str(join(path, "node_id"), node["node_id"], nodeIDPattern)
		if !ok {
			return nil, false
		}
		role, ok := v.enum(join(path, "role"), node["role"], roles...)
		if !ok {
			return nil, false
		}
		profile, ok := v.enum(join(path, "profile"), node["profile"], profiles...)
		if !ok {
			return nil, false
		}
		for _, field := range []string{"workload_id", "orchestrator_hotkey", "worker_id"} {
			if _, ok := v.nullableStr(join(path, field), node[field], printablePattern); !ok {
				return nil, false
			}
		}
		if _, ok := v.enum(join(path, "region"), node["region"], BroadRegions...); !ok {
			return nil, false
		}
		state, ok := v.enum(join(path, "state"), node["state"], "planned", "starting", "active", "draining")
		if !ok {
			return nil, false
		}
		endpoints, ok := v.array(join(path, "endpoints"), node["endpoints"], 0, 8)
		if !ok {
			return nil, false
		}
		kinds := map[string]bool{}
		for endpointIndex, rawEndpoint := range endpoints {
			endpointPath := join(join(path, "endpoints"), endpointIndex)
			endpoint, ok := v.object(endpointPath, rawEndpoint, "kind", "url")
			if !ok {
				return nil, false
			}
			kind, ok := v.enum(join(endpointPath, "kind"), endpoint["kind"], "whip", "whep", "hls", "pull", "rtmp", "rtmps", "srt")
			if !ok {
				return nil, false
			}
			url, ok := v.str(join(endpointPath, "url"), endpoint["url"], nil)
			if !ok {
				return nil, false
			}
			if !validEndpointURL(kind, url) {
				return nil, v.fail(join(endpointPath, "url"), "invalid endpoint URL")
			}
			if !contains(RoleEndpointKinds[role], kind) || kinds[kind] {
				return nil, v.fail(join(endpointPath, "kind"), "endpoint kind not allowed for this role")
			}
			kinds[kind] = true
		}
		nodeKey, ok := v.nullableStr(join(path, "node_key"), node["node_key"], publicKeyPattern)
		if !ok {
			return nil, false
		}
		if nodeKey != nil {
			if decoded, err := DecodeB64URL(*nodeKey); err != nil || len(decoded) != 32 {
				return nil, v.fail(join(path, "node_key"), "invalid format")
			}
		}
		hostname, ok := v.nullableStr(join(path, "hostname"), node["hostname"], nil)
		if !ok {
			return nil, false
		}
		if hostname != nil && !ValidHostname(*hostname) {
			return nil, v.fail(join(path, "hostname"), "invalid format")
		}
		if _, ok := v.nullableStr(join(path, "tls_certificate_sha256"), node["tls_certificate_sha256"], sha256HexPattern); !ok {
			return nil, false
		}
		if RoleProfiles[role] != profile {
			return nil, v.fail(join(path, "profile"), "profile does not match role")
		}
		if (state == "active" || state == "draining") && node["worker_id"] == nil {
			return nil, v.fail(join(path, "worker_id"), "an active or draining node names its worker")
		}
		summaries = append(summaries, nodeSummary{id: id, role: role})
	}
	return summaries, true
}

type linkSummary struct{ parent, child string }

func (v *tv) validateLinks(value any) ([]linkSummary, bool) {
	list, ok := v.array("links", value, 0, 2048)
	if !ok {
		return nil, false
	}
	links := make([]linkSummary, 0, len(list))
	for index, raw := range list {
		path := join("links", index)
		link, ok := v.object(path, raw, "parent", "child")
		if !ok {
			return nil, false
		}
		parent, ok := v.str(join(path, "parent"), link["parent"], nodeIDPattern)
		if !ok {
			return nil, false
		}
		child, ok := v.str(join(path, "child"), link["child"], nodeIDPattern)
		if !ok {
			return nil, false
		}
		links = append(links, linkSummary{parent: parent, child: child})
	}
	return links, true
}

type ladderSummary struct {
	gopMS  int64
	source bool
}

func (v *tv) validateLadder(value any) (*ladderSummary, bool) {
	if value == nil {
		return nil, true
	}
	ladder, ok := v.object("ladder", value, "video", "audio", "gop_ms")
	if !ok {
		return nil, false
	}
	ids := map[string]bool{}
	passthroughs := 0
	video, ok := v.array("ladder.video", ladder["video"], 0, 8)
	if !ok {
		return nil, false
	}
	for index, raw := range video {
		path := join("ladder.video", index)
		rendition, ok := v.objectOptional(path, raw, []string{"rendition_id", "codec", "width", "height", "fps_max", "bitrate_bps"},
			[]string{"passthrough"})
		if !ok {
			return nil, false
		}
		id, ok := v.str(join(path, "rendition_id"), rendition["rendition_id"], renditionIDPattern)
		if !ok {
			return nil, false
		}
		if _, ok := v.enum(join(path, "codec"), rendition["codec"], "h264", "h265", "av1"); !ok {
			return nil, false
		}
		width, ok := v.integer(join(path, "width"), rendition["width"], 16, 7680)
		if !ok {
			return nil, false
		}
		height, ok := v.integer(join(path, "height"), rendition["height"], 16, 4320)
		if !ok {
			return nil, false
		}
		if _, ok := v.integer(join(path, "fps_max"), rendition["fps_max"], 1, 120); !ok {
			return nil, false
		}
		if _, ok := v.integer(join(path, "bitrate_bps"), rendition["bitrate_bps"], 50_000, 50_000_000); !ok {
			return nil, false
		}
		if width%2 != 0 {
			return nil, v.fail(join(path, "width"), "width must be even")
		}
		if height%2 != 0 {
			return nil, v.fail(join(path, "height"), "height must be even")
		}
		passthrough := false
		if raw, present := rendition["passthrough"]; present {
			if flag, isBool := raw.(bool); !isBool || !flag {
				return nil, v.fail(join(path, "passthrough"), "passthrough is true when present")
			}
			passthrough = true
			passthroughs++
		}
		if ids[id] {
			return nil, v.fail(join(path, "rendition_id"), "duplicate rendition_id")
		}
		ids[id] = true
		if id == SourceRenditionID && !passthrough {
			return nil, v.fail(join(path, "rendition_id"), "source is the id of the passthrough rendition only")
		}
		if passthrough && id != SourceRenditionID {
			return nil, v.fail(join(path, "rendition_id"), "the passthrough rendition's id is source")
		}
	}
	audio, ok := v.array("ladder.audio", ladder["audio"], 0, 4)
	if !ok {
		return nil, false
	}
	for index, raw := range audio {
		path := join("ladder.audio", index)
		rendition, ok := v.object(path, raw, "rendition_id", "codec", "bitrate_bps", "channels", "sample_rate")
		if !ok {
			return nil, false
		}
		id, ok := v.str(join(path, "rendition_id"), rendition["rendition_id"], renditionIDPattern)
		if !ok {
			return nil, false
		}
		if _, ok := v.enum(join(path, "codec"), rendition["codec"], "aac", "opus"); !ok {
			return nil, false
		}
		if _, ok := v.integer(join(path, "bitrate_bps"), rendition["bitrate_bps"], 16_000, 512_000); !ok {
			return nil, false
		}
		if _, ok := v.integer(join(path, "channels"), rendition["channels"], 1, 2); !ok {
			return nil, false
		}
		rate, ok := v.integer(join(path, "sample_rate"), rendition["sample_rate"], 44_100, 48_000)
		if !ok {
			return nil, false
		}
		if rate != 44_100 && rate != 48_000 {
			return nil, v.fail(join(path, "sample_rate"), "44100 or 48000")
		}
		if ids[id] {
			return nil, v.fail(join(path, "rendition_id"), "duplicate rendition_id")
		}
		ids[id] = true
		if id == SourceRenditionID {
			return nil, v.fail(join(path, "rendition_id"), "source is the id of the passthrough rendition only")
		}
	}
	gop, ok := v.integer("ladder.gop_ms", ladder["gop_ms"], 500, 10_000)
	if !ok {
		return nil, false
	}
	if len(video)+len(audio) == 0 {
		return nil, v.fail("ladder.video", "a ladder has at least one rendition")
	}
	if passthroughs > 0 {
		if len(video) != 1 {
			return nil, v.fail("ladder.video", "a source ladder has exactly one video rendition")
		}
		if video[0].(map[string]any)["codec"] != "h264" {
			return nil, v.fail("ladder.video.0.codec", "the passthrough rendition is h264")
		}
		if len(audio) > 1 {
			return nil, v.fail("ladder.audio", "a source ladder has at most one audio rendition")
		}
		if len(audio) == 1 && audio[0].(map[string]any)["codec"] != "aac" {
			return nil, v.fail("ladder.audio.0.codec", "the audio rendition of a source ladder is aac")
		}
	}
	return &ladderSummary{gopMS: gop, source: passthroughs > 0}, true
}

type packagingSummary struct{ segmentMS int64 }

func (v *tv) validatePackaging(value any) (*packagingSummary, bool) {
	if value == nil {
		return nil, true
	}
	packaging, ok := v.object("packaging", value, "segment_ms", "part_ms", "dvr_window_s", "recording")
	if !ok {
		return nil, false
	}
	segment, ok := v.integer("packaging.segment_ms", packaging["segment_ms"], 1_000, 10_000)
	if !ok {
		return nil, false
	}
	part, ok := v.integer("packaging.part_ms", packaging["part_ms"], 100, 2_000)
	if !ok {
		return nil, false
	}
	if _, ok := v.integer("packaging.dvr_window_s", packaging["dvr_window_s"], 0, 43_200); !ok {
		return nil, false
	}
	if _, isBool := packaging["recording"].(bool); !isBool {
		return nil, v.fail("packaging.recording", "expected boolean")
	}
	if part >= segment {
		return nil, v.fail("packaging.part_ms", "part_ms must be below segment_ms")
	}
	return &packagingSummary{segmentMS: segment}, true
}

func (v *tv) validateUsage(value any) bool {
	usage, ok := v.object("usage", value, "seal_keys", "grant_keys", "receipts")
	if !ok {
		return false
	}
	for _, list := range []struct {
		name, id string
		min, max int
		pattern  *regexp.Regexp
	}{{"seal_keys", "key_id", 1, UsageSealKeysMax, SealKeyIDPattern}, {"grant_keys", "kid", 0, UsageGrantKeysMax, ReceiptGrantKeyIDPattern}} {
		path := join("usage", list.name)
		keys, ok := v.array(path, usage[list.name], list.min, list.max)
		if !ok {
			return false
		}
		seen := map[string]bool{}
		for index, raw := range keys {
			at := join(path, index)
			key, ok := v.object(at, raw, list.id, "public_key")
			if !ok {
				return false
			}
			id, ok := v.str(join(at, list.id), key[list.id], list.pattern)
			if !ok {
				return false
			}
			publicKey, ok := v.str(join(at, "public_key"), key["public_key"], publicKeyPattern)
			if !ok {
				return false
			}
			if decoded, err := DecodeB64URL(publicKey); err != nil || len(decoded) != 32 {
				return v.fail(join(at, "public_key"), "invalid format")
			}
			if seen[id] {
				return v.fail(join(at, list.id), "duplicate")
			}
			seen[id] = true
		}
	}
	receipts, ok := v.object("usage.receipts", usage["receipts"], "enforce")
	if !ok {
		return false
	}
	enforce, ok := v.array("usage.receipts.enforce", receipts["enforce"], 0, len(ReceiptTransports))
	if !ok {
		return false
	}
	seen := map[string]bool{}
	for index, raw := range enforce {
		path := join("usage.receipts.enforce", index)
		transport, ok := v.enum(path, raw, ReceiptTransports...)
		if !ok {
			return false
		}
		if seen[transport] {
			return v.fail(path, "duplicate")
		}
		seen[transport] = true
	}
	return true
}

func (v *tv) validateIngest(value any) bool {
	ingest, ok := v.objectOptional("ingest", value, []string{"protocols", "stream_keys"}, []string{"channel_id"})
	if !ok {
		return false
	}
	if channel, present := ingest["channel_id"]; present {
		if _, ok := v.str("ingest.channel_id", channel, ChannelIDPattern); !ok {
			return false
		}
	}
	protocols, ok := v.array("ingest.protocols", ingest["protocols"], 1, 4)
	if !ok {
		return false
	}
	seen := map[string]bool{}
	for index, raw := range protocols {
		path := join("ingest.protocols", index)
		protocol, ok := v.enum(path, raw, "whip", "rtmp", "rtmps", "srt")
		if !ok {
			return false
		}
		if seen[protocol] {
			return v.fail(path, "duplicate protocol")
		}
		seen[protocol] = true
	}
	keys, ok := v.array("ingest.stream_keys", ingest["stream_keys"], 0, 16)
	if !ok {
		return false
	}
	ids := map[string]bool{}
	for index, raw := range keys {
		path := join("ingest.stream_keys", index)
		key, ok := v.objectOptional(path, raw, []string{"key_id", "sha256"}, []string{"channel"})
		if !ok {
			return false
		}
		if channel, present := key["channel"]; present && channel != true {
			return v.fail(join(path, "channel"), "channel is present only as true")
		}
		id, ok := v.str(join(path, "key_id"), key["key_id"], streamKeyIDPattern)
		if !ok {
			return false
		}
		if _, ok := v.str(join(path, "sha256"), key["sha256"], sha256HexPattern); !ok {
			return false
		}
		if ids[id] {
			return v.fail(join(path, "key_id"), "duplicate key_id")
		}
		ids[id] = true
	}
	return true
}

func integerize(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = integerize(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = integerize(item)
		}
		return out
	case json.Number:
		if integer, ok := jsonInteger(typed); ok {
			return json.Number(strconv.FormatInt(integer, 10))
		}
		return typed
	default:
		return value
	}
}

const (
	SourceRenditionID   = "source"
	SourceLadderKeyword = "source"
)

func SourceLadder() TranscodeLadder {
	return TranscodeLadder{
		Video: []VideoRendition{{RenditionID: SourceRenditionID, Codec: "h264", Width: 1920, Height: 1080, FPSMax: 60, BitrateBPS: 8_000_000, Passthrough: true}},
		Audio: []AudioRendition{{RenditionID: "audio", Codec: "aac", BitrateBPS: 128_000, Channels: 2, SampleRate: 48_000}},
		GOPMS: 2000,
	}
}

func IsSourceLadder(ladder *TranscodeLadder) bool {
	if ladder == nil {
		return false
	}
	for _, rendition := range ladder.Video {
		if rendition.Passthrough {
			return true
		}
	}
	return false
}

func LadderNeedsEncoder(ladder *TranscodeLadder) bool {
	if ladder == nil {
		return false
	}
	for _, rendition := range ladder.Video {
		if !rendition.Passthrough {
			return true
		}
	}
	return false
}

func EncodedRenditions(ladder *TranscodeLadder) int {
	if ladder == nil {
		return 0
	}
	count := len(ladder.Audio)
	for _, rendition := range ladder.Video {
		if !rendition.Passthrough {
			count++
		}
	}
	return count
}

func ValidateLadder(ladder TranscodeLadder) error {
	data, err := MarshalWire(ladder)
	if err != nil {
		return err
	}
	value, err := decodeObject(data)
	if err != nil {
		return err
	}
	v := &tv{}
	if _, ok := v.validateLadder(value); !ok {
		return v.issue
	}
	return nil
}

type LadderRequest struct {
	Source bool
	Ladder *TranscodeLadder
}

var ErrLadderRequest = errors.New("mediacontracts: a ladder request is the keyword source or a ladder object, exactly one")

func (request LadderRequest) Resolve() (TranscodeLadder, error) {
	switch {
	case request.Source && request.Ladder == nil:
		return SourceLadder(), nil
	case !request.Source && request.Ladder != nil:
		return *request.Ladder, nil
	}
	return TranscodeLadder{}, ErrLadderRequest
}

func (request LadderRequest) MarshalJSON() ([]byte, error) {
	if _, err := request.Resolve(); err != nil {
		return nil, err
	}
	if request.Source {
		return json.Marshal(SourceLadderKeyword)
	}
	return MarshalWire(*request.Ladder)
}

func (request *LadderRequest) UnmarshalJSON(data []byte) error {
	var keyword string
	if json.Unmarshal(data, &keyword) == nil {
		if keyword != SourceLadderKeyword {
			return &TopologyIssue{Path: "", Message: "a ladder request is \"source\" or a ladder object"}
		}
		*request = LadderRequest{Source: true}
		return nil
	}
	var ladder TranscodeLadder
	if err := UnmarshalStrict(data, &ladder); err != nil {
		return err
	}
	*request = LadderRequest{Ladder: &ladder}
	return nil
}

func ValidateLadderRequest(request LadderRequest) error {
	ladder, err := request.Resolve()
	if err != nil {
		return err
	}
	if request.Source {
		return nil
	}
	return ValidateLadder(ladder)
}

func HLSTargetDurationS(ladder *TranscodeLadder, packaging TopologyPackaging) int64 {
	if IsSourceLadder(ladder) {
		return (packaging.SegmentMS + 2*ladder.GOPMS + 999) / 1000
	}
	return (3*packaging.SegmentMS + 1999) / 2000
}

func HLSMaxPartIndex(ladder *TranscodeLadder, packaging TopologyPackaging) int64 {
	if packaging.PartMS <= 0 {
		return 0
	}
	return (HLSTargetDurationS(ladder, packaging)*1000 + packaging.PartMS - 1) / packaging.PartMS
}
