package contracts

type HTTPEndpoint struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type TransferPart struct {
	TaskID         string       `json:"task_id,omitempty"`
	OfferID        string       `json:"offer_id,omitempty"`
	ETagRequired   bool         `json:"etag_required,omitempty"`
	Index          int          `json:"index"`
	Source         HTTPEndpoint `json:"source"`
	Destination    HTTPEndpoint `json:"destination"`
	SourceRange    bool         `json:"source_range,omitempty"`
	Offset         int64        `json:"offset,omitempty"`
	Length         int64        `json:"length,omitempty"`
	ExpectedSHA256 string       `json:"expected_sha256,omitempty"`
}

type MultipartTransfer struct {
	TransferID             string         `json:"transfer_id"`
	Parts                  []TransferPart `json:"parts"`
	SourceGroupID          string         `json:"source_group_id,omitempty"`
	DestinationConcurrency int            `json:"destination_concurrency,omitempty"`
}

const SourceGroupCheckpointSchema = "beam.transfer.source-group/1"

// Delivery evidence only. Source buffers, endpoints and credentials are never checkpointed.
type SourceGroupCheckpoint struct {
	TransferID    string            `json:"transfer_id"`
	SourceGroupID string            `json:"source_group_id"`
	Bytes         int64             `json:"bytes"`
	Outputs       map[string]string `json:"outputs"`
}

const DistributionProtocol = "beam.transfer.range/1"

type DistributionChild struct {
	WorkerID       string   `json:"worker_id"`
	NodeID         string   `json:"node_id"`
	StandbyNodeIDs []string `json:"standby_node_ids,omitempty"`
}

type DistributionDestination struct {
	DestinationID string       `json:"destination_id"`
	Endpoint      HTTPEndpoint `json:"endpoint"`
}

// DistributionTransfer assigns one node in an Orchestrator-computed dissemination
// tree. Exactly one root reads Source; every other node reads its parent
// Circuit and streams concurrently to its children and external destinations.
type DistributionTransfer struct {
	TransferID     string                    `json:"transfer_id"`
	CircuitID      string                    `json:"circuit_id,omitempty"`
	Root           bool                      `json:"root"`
	Source         *HTTPEndpoint             `json:"source,omitempty"`
	ParentNodeID   string                    `json:"parent_node_id,omitempty"`
	Children       []DistributionChild       `json:"children,omitempty"`
	Destinations   []DistributionDestination `json:"destinations,omitempty"`
	Offset         int64                     `json:"offset,omitempty"`
	Length         int64                     `json:"length"`
	ExpectedSHA256 string                    `json:"expected_sha256,omitempty"`
}

type ActionExecution struct {
	TaskID            string                   `json:"task_id"`
	ActionPackageName string                   `json:"action_package_name"`
	ActionVersion     string                   `json:"action_version,omitempty"`
	Entrypoint        string                   `json:"entrypoint"`
	ArtifactSHA256    string                   `json:"artifact_sha256"`
	TimeoutSeconds    int64                    `json:"timeout_seconds,omitempty"`
	Input             map[string]any           `json:"input,omitempty"`
	Config            map[string]any           `json:"config,omitempty"`
	RegistryArtifact  *ActionRegistryArtifact  `json:"registry_artifact,omitempty"`
	SecretBroker      *ActionSecretBroker      `json:"secret_broker,omitempty"`
	ArtifactPublisher *ActionArtifactPublisher `json:"artifact_publisher,omitempty"`
	Sandbox           ActionSandbox            `json:"sandbox"`
	HostRPCMethods    []string                 `json:"host_rpc_methods,omitempty"`
}

// ActionRegistryArtifact is a short-lived, Orchestrator-issued download capability.
// The Worker caches bytes by ArtifactSHA256; the URL and its headers are never
// exposed to the sandbox.
type ActionRegistryArtifact struct {
	URL                string            `json:"url"`
	Headers            map[string]string `json:"headers,omitempty"`
	MediaType          string            `json:"media_type,omitempty"`
	SizeBytes          int64             `json:"size_bytes,omitempty"`
	PublisherPublicKey string            `json:"publisher_public_key,omitempty"`
	PublisherSignature string            `json:"publisher_signature,omitempty"`
}

// ActionSecretBroker is consumed only by the Worker host RPC server. Tokens
// and returned secret values are never added to the sandbox environment.
type ActionSecretBroker struct {
	Endpoint     string   `json:"endpoint"`
	BearerToken  string   `json:"bearer_token"`
	AllowedNames []string `json:"allowed_names,omitempty"`
}

// ActionArtifactPublisher is a bounded upload capability. The host streams a
// sandbox scratch file to Endpoint and returns the durable artifact reference.
type ActionArtifactPublisher struct {
	Endpoint         string            `json:"endpoint"`
	BearerToken      string            `json:"bearer_token"`
	Headers          map[string]string `json:"headers,omitempty"`
	MaxArtifactBytes int64             `json:"max_artifact_bytes,omitempty"`
}

// ActionSandbox selects a strong execution backend. OCI images must be pinned
// by digest and are run without network or Linux capabilities. WASI modules
// receive only a preopened workspace and deterministic execution fuel.
type ActionSandbox struct {
	Runtime    string   `json:"runtime"`
	OCIImage   string   `json:"oci_image,omitempty"`
	Command    []string `json:"command,omitempty"`
	Fuel       uint64   `json:"fuel,omitempty"`
	LegacyNode bool     `json:"legacy_node,omitempty"`
}

type TunnelAssignment struct {
	TunnelID      string            `json:"tunnel_id"`
	Mode          string            `json:"mode"`
	Protocol      string            `json:"protocol"`
	ListenAddress string            `json:"listen_address"`
	Target        string            `json:"target"`
	IngressToken  string            `json:"ingress_token,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

type RoomAssignment struct {
	RoomID             string            `json:"room_id"`
	Role               string            `json:"role"`
	Protocol           string            `json:"protocol"`
	ListenAddress      string            `json:"listen_address"`
	Members            map[string]string `json:"members"`
	MaxDatagramBytes   int               `json:"max_datagram_bytes,omitempty"`
	IdleTimeoutSeconds int64             `json:"idle_timeout_seconds,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

type ArtifactPublication struct {
	ArtifactID string `json:"artifact_id"`
	Path       string `json:"path"`
	MediaType  string `json:"media_type,omitempty"`
	TTLSeconds int64  `json:"ttl_seconds"`
}
