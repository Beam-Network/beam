package contracts

type HTTPEndpoint struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Destination upload protocols. An empty protocol is UploadProtocolPresignedPut.
const (
	UploadProtocolPresignedPut      = "presigned_put"
	UploadProtocolAuthenticatedPart = "authenticated_part_v1"
	UploadProtocolAuthenticatedForm = "authenticated_form_v1"
	// UploadProfileBox is the Box upload-session part and small-file form profile.
	UploadProfileBox = "box"
)

// TransferBufferOverheadBytes is the transport reservation added to every buffered range.
const TransferBufferOverheadBytes = 4 << 20

// AuthenticatedPartProbeBytes is the largest provider part BeamCore plans for
// authenticated destinations (Box: 8, 16 or 32 MiB); capability probes reserve it.
const AuthenticatedPartProbeBytes = 32 << 20

type TransferPart struct {
	ETagRequired   bool         `json:"etag_required,omitempty"`
	Index          int          `json:"index"`
	Source         HTTPEndpoint `json:"source"`
	Destination    HTTPEndpoint `json:"destination"`
	SourceRange    bool         `json:"source_range,omitempty"`
	Offset         int64        `json:"offset,omitempty"`
	Length         int64        `json:"length,omitempty"`
	ExpectedSHA256 string       `json:"expected_sha256,omitempty"`
	// Protocol selects how Destination is written; Destination.Method is the declared method.
	// The authenticated protocols buffer the range, send its digest, retry with the same
	// buffer up to MaxAttempts and verify the provider receipt.
	Protocol    string         `json:"protocol,omitempty"`
	Profile     string         `json:"profile,omitempty"`
	Digest      *UploadDigest  `json:"digest,omitempty"`
	Receipt     *UploadReceipt `json:"receipt,omitempty"`
	MaxAttempts int            `json:"max_attempts,omitempty"`
	Form        *UploadForm    `json:"form,omitempty"`
}

// Authenticated reports whether the destination uses an authenticated upload protocol.
func (part TransferPart) Authenticated() bool {
	return part.Protocol == UploadProtocolAuthenticatedPart || part.Protocol == UploadProtocolAuthenticatedForm
}

// UploadDigest names the digest of the sent bytes and the header that carries it.
// Format is "sha=base64" (Digest: sha=<base64>) or "hex".
type UploadDigest struct {
	Algorithm string `json:"algorithm"`
	Header    string `json:"header"`
	Format    string `json:"format"`
}

// UploadReceipt locates the provider receipt in the JSON response. Path is a
// dot-separated path whose numeric segments index arrays; Fields must be present.
type UploadReceipt struct {
	Path   string   `json:"path"`
	Fields []string `json:"fields"`
}

// UploadForm describes a multipart form upload: the Attributes JSON part comes
// first, then the range bytes as FileField named Filename.
type UploadForm struct {
	Attributes string `json:"attributes"`
	FileField  string `json:"file_field"`
	Filename   string `json:"filename"`
}

type MultipartTransfer struct {
	Parts  []TransferPart `json:"parts"`
	Fanout bool           `json:"fanout,omitempty"`
}

const FanoutDestinationConcurrency = 8

const FanoutCheckpointSchema = "beam.transfer.fanout/1"

// Delivery evidence only. Source buffers, endpoints and credentials are never checkpointed.
type FanoutCheckpoint struct {
	Bytes   int64             `json:"bytes"`
	Outputs map[string]string `json:"outputs"`
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
