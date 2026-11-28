package broadcast

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

var broadcastProfiles = map[string]bool{
	mc.ProfileIngestV1: true, mc.ProfileTranscodeV1: true, mc.ProfilePackageLLHLSV1: true, mc.ProfileEdgeHTTPV1: true,
}

func IsBroadcastProfile(profile string) bool { return broadcastProfiles[profile] }

type Recording = mc.NodeRecording

type Details struct {
	SessionID          string          `json:"session_id"`
	Profile            string          `json:"profile"`
	NodeID             string          `json:"node_id"`
	Role               string          `json:"role"`
	Topology           json.RawMessage `json:"topology"`
	ServiceToken       *string         `json:"service_token"`
	ContentKeys        []mc.ContentKey `json:"content_keys"`
	Recording          *Recording      `json:"recording"`
	PlaybackSecret     *string         `json:"playback_secret"`
	ResumeFromSequence uint64          `json:"resume_from_sequence"`
	Replay             bool            `json:"replay"`
	HeartbeatTimeoutMS int64           `json:"heartbeat_timeout_ms"`
	BucketAccess       *BucketAccess   `json:"bucket_access"`
	MediaSequenceFloor *int64          `json:"media_sequence_floor"`
	ExcludedWorkers    []string        `json:"excluded_workers,omitempty"`

	Service    string          `json:"service"`
	Tracks     json.RawMessage `json:"tracks"`
	Layers     json.RawMessage `json:"layers"`
	Protection json.RawMessage `json:"protection"`
}

func (d Details) emptyLegacy() bool {
	empty := func(raw json.RawMessage) bool {
		trimmed := string(bytes.TrimSpace(raw))
		return trimmed == "" || trimmed == "null" || trimmed == "[]"
	}
	if d.Service != "" || !empty(d.Tracks) || !empty(d.Layers) {
		return false
	}
	if trimmed := bytes.TrimSpace(d.Protection); len(trimmed) > 0 && string(trimmed) != "null" {
		var protection contracts.MediaProtection
		if err := json.Unmarshal(trimmed, &protection); err != nil || protection != (contracts.MediaProtection{}) {
			return false
		}
	}
	return true
}

func (d Details) VODPackage(topology *mc.Topology) bool {
	return d.Role == mc.RolePackage && topology != nil && topology.DeliveryProfile == mc.DeliveryVOD
}

func (d Details) RecordingPrefix(vod bool) string {
	if d.Recording == nil {
		return ""
	}
	prefix, _ := mc.BucketGrantFor(*d.Recording, vod)
	return prefix
}

type Spec struct {
	Identity contracts.RoomWorkloadIdentity
	Details  Details
	Topology *mc.Topology
}

func DecodeSpec(spec domain.Spec, now time.Time) (*Spec, error) {
	var worker contracts.RoomWorkerSpec[json.RawMessage]
	decoder := json.NewDecoder(bytes.NewReader(spec.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&worker); err != nil {
		return nil, fmt.Errorf("invalid room media node spec: %w", err)
	}
	if worker.Schema != contracts.RoomWorkloadSchema || worker.Identity.Kind != domain.KindRoomMedia {
		return nil, errors.New("invalid room media node spec")
	}
	if err := worker.Identity.Validate(domain.KindRoomMedia, now); err != nil {
		return nil, err
	}
	if len(worker.Targets) != 0 {
		return nil, errors.New("room media node workloads have no targets")
	}
	var details Details
	detailsDecoder := json.NewDecoder(bytes.NewReader(worker.Details))
	detailsDecoder.DisallowUnknownFields()
	if err := detailsDecoder.Decode(&details); err != nil {
		return nil, fmt.Errorf("invalid room media node details: %w", err)
	}
	topology, err := mc.ValidateTopologyJSON(details.Topology)
	if err != nil {
		return nil, fmt.Errorf("invalid node topology view: %w", err)
	}
	if err := checkDetails(details, topology); err != nil {
		return nil, err
	}
	return &Spec{Identity: worker.Identity, Details: details, Topology: topology}, nil
}

func checkDetails(details Details, topology *mc.Topology) error {
	if !IsBroadcastProfile(details.Profile) || mc.RoleProfiles[details.Role] != details.Profile {
		return errors.New("node profile does not match its role")
	}
	if !details.emptyLegacy() {
		return errors.New("a node workload carries no legacy media tracks, layers or protection")
	}
	if details.Replay {
		return errors.New("room media node workloads never replay")
	}
	if topology.View == nil || topology.View.NodeID != details.NodeID {
		return errors.New("node details must carry the node's own topology view")
	}
	if topology.SessionID != details.SessionID {
		return errors.New("node session does not match its topology")
	}
	self, ok := findNode(topology, details.NodeID)
	if !ok || self.Role != details.Role {
		return errors.New("node is not in its topology view with its role")
	}
	vod := topology.DeliveryProfile == mc.DeliveryVOD
	needsParent := details.Role != mc.RoleIngest && !(details.Role == mc.RolePackage && vod)
	if needsParent != (details.ServiceToken != nil) {
		return errors.New("service_token is required exactly for nodes with a parent role")
	}
	if details.ServiceToken != nil {
		claims, _, _, err := mc.ParseTokenUnverified(*details.ServiceToken)
		if err != nil || !strings.HasPrefix(claims.KeyID, mc.ServiceKeyIDPrefix) || claims.Role != mc.RoleSubscribe ||
			claims.Identity != mc.NodeIdentityPrefix+details.NodeID || claims.Session != details.SessionID {
			return errors.New("service_token is not this node's service token")
		}
	}
	protected := topology.Protection.Mode == "protected"
	wantKeys := details.Role == mc.RolePackage && protected
	if wantKeys != (details.ContentKeys != nil) {
		return errors.New("content_keys are required exactly for the package node of a protected topology")
	}
	if wantKeys {
		if len(details.ContentKeys) < 1 || len(details.ContentKeys) > 4 {
			return errors.New("content_keys must hold 1 to 4 keys")
		}
		for _, key := range details.ContentKeys {
			for _, value := range []string{key.KeyID, key.Key, key.IV} {
				if decoded, err := mc.DecodeHex(value); err != nil || len(decoded) != 16 {
					return errors.New("content keys are 16-byte hex values")
				}
			}
		}
	}
	vodPackage := details.Role == mc.RolePackage && vod
	wantRecording := details.Role == mc.RolePackage && !vod && topology.Packaging != nil && topology.Packaging.Recording
	if (wantRecording || vodPackage) != (details.Recording != nil) {
		return errors.New("recording is set exactly for a recording package node and a vod package node")
	}
	if details.Recording != nil {
		if err := mc.ValidateNodeRecording(*details.Recording, vodPackage); err != nil {
			return err
		}
		if vodPackage && details.Recording.RecordingID != details.SessionID {
			return errors.New("a vod session's id is its recording id")
		}
		if !vodPackage && !strings.HasSuffix(details.Recording.BucketPrefix, "/"+details.SessionID+"/"+details.Recording.RecordingID+"/") {
			return errors.New("the recording prefix ends with <session_id>/<recording_id>/")
		}
	}
	if details.BucketAccess != nil {
		if details.Recording == nil {
			return errors.New("bucket_access is granted only to a node carrying a recording")
		}
		if err := mc.CheckNodeBucketAccess(*details.BucketAccess, *details.Recording, vodPackage); err != nil {
			return errors.New("bucket_access is not this node's least-privilege grant")
		}
	}
	if details.MediaSequenceFloor != nil && (details.Role != mc.RolePackage || vod || *details.MediaSequenceFloor < 0) {
		return errors.New("media_sequence_floor is set only for a live package node")
	}
	wantSecret := details.Role == mc.RolePackage || details.Role == mc.RoleShield || details.Role == mc.RoleEdge
	if wantSecret != (details.PlaybackSecret != nil) {
		return errors.New("playback_secret is required exactly for package, shield and edge nodes")
	}
	if details.PlaybackSecret != nil {
		if decoded, err := mc.DecodeHex(*details.PlaybackSecret); err != nil || len(decoded) != 32 {
			return errors.New("playback_secret must be 32 bytes of hex")
		}
	}
	switch details.Role {
	case mc.RoleTranscode, mc.RolePackage:
		if topology.Ladder == nil {
			return errors.New("transcode and package nodes require a ladder")
		}
	}
	if details.Role == mc.RolePackage && topology.Packaging == nil {
		return errors.New("a package node requires packaging")
	}
	if details.Role == mc.RoleIngest && topology.Ingest == nil {
		return errors.New("an ingest node requires the ingest settings")
	}
	return nil
}

func findNode(topology *mc.Topology, nodeID string) (mc.TopologyNode, bool) {
	for _, node := range topology.Nodes {
		if node.NodeID == nodeID {
			return node, true
		}
	}
	return mc.TopologyNode{}, false
}

type Node struct {
	Spec       *Spec
	WorkloadID string
	NodeID     string
	Role       string
	SessionID  string
	WorkerID   string
	StartedAt  time.Time

	signer Signer
	secret []byte

	mu         sync.RWMutex
	topology   *mc.Topology
	verifier   *mc.TokenVerifier
	changed    chan struct{}
	waitLogged [2]int64
}

type Signer interface {
	Sign(schema string, payload []byte) mc.SignedEnvelope
	NodeKey() string
}

func NewNode(spec *Spec, workerID string, signer Signer, now time.Time) (*Node, error) {
	if signer == nil {
		return nil, errors.New("a broadcast node needs its node key")
	}
	node := &Node{Spec: spec, WorkloadID: spec.Identity.WorkloadID, NodeID: spec.Details.NodeID, Role: spec.Details.Role,
		SessionID: spec.Details.SessionID, WorkerID: workerID, StartedAt: now, signer: signer, changed: make(chan struct{})}
	if spec.Details.PlaybackSecret != nil {
		node.secret, _ = hex.DecodeString(*spec.Details.PlaybackSecret)
	}
	node.install(spec.Topology)
	return node, nil
}

func (n *Node) install(topology *mc.Topology) {
	project := ""
	if topology.ProjectID != nil {
		project = *topology.ProjectID
	}
	n.topology = topology
	n.verifier = mc.NewTokenVerifier(mc.TokenVerifyContext{Audience: topology.Tokens.Audience, Project: project,
		Session: topology.SessionID, Keys: topology.Tokens.Keys, Deny: topology.Tokens.Deny})
}

func (n *Node) PublicKey() string { return n.signer.NodeKey() }

func (n *Node) Sign(schema string, payload []byte) mc.SignedEnvelope {
	return n.signer.Sign(schema, payload)
}

func (n *Node) PlaybackSecret() []byte { return n.secret }

func (n *Node) ServiceToken() string {
	if n.Spec.Details.ServiceToken == nil {
		return ""
	}
	return *n.Spec.Details.ServiceToken
}

func (n *Node) Topology() *mc.Topology {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.topology
}

func (n *Node) Changed() <-chan struct{} {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.changed
}

func (n *Node) Apply(topology *mc.Topology) bool {
	if topology == nil || topology.View == nil || topology.View.NodeID != n.NodeID || topology.SessionID != n.SessionID {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	current := n.topology
	if topology.Epoch < current.Epoch || (topology.Epoch == current.Epoch && topology.Revision <= current.Revision) {
		return false
	}
	n.install(topology)
	close(n.changed)
	n.changed = make(chan struct{})
	return true
}

func (n *Node) Self() (mc.TopologyNode, bool) { return findNode(n.Topology(), n.NodeID) }

func (n *Node) Parent() (mc.TopologyNode, bool) {
	topology := n.Topology()
	for _, link := range topology.Links {
		if link.Child == n.NodeID {
			return findNode(topology, link.Parent)
		}
	}
	return mc.TopologyNode{}, false
}

func (n *Node) NoteParentWait(kind string) {
	n.mu.Lock()
	version := [2]int64{n.topology.Epoch, n.topology.Revision}
	first := n.waitLogged != version
	n.waitLogged = version
	n.mu.Unlock()
	if first {
		log.Printf("media %s %s: waiting for its parent's %s endpoint (topology epoch %d revision %d)",
			n.Role, n.NodeID, kind, version[0], version[1])
	}
}

func (n *Node) Children() []mc.TopologyNode {
	topology := n.Topology()
	var children []mc.TopologyNode
	for _, link := range topology.Links {
		if link.Parent == n.NodeID {
			if child, ok := findNode(topology, link.Child); ok {
				children = append(children, child)
			}
		}
	}
	return children
}

func (n *Node) IsChild(nodeID string) bool {
	for _, link := range n.Topology().Links {
		if link.Parent == n.NodeID && link.Child == nodeID {
			return true
		}
	}
	return false
}

func (n *Node) Expired(now time.Time) bool { return now.UnixMilli() >= n.Topology().ExpiresAtMS }

func (n *Node) Draining() bool {
	self, ok := n.Self()
	return !ok || self.State == "draining"
}

func (n *Node) VerifyToken(token string, now time.Time) (mc.TokenClaims, error) {
	n.mu.RLock()
	verifier := n.verifier
	n.mu.RUnlock()
	return verifier.Verify(token, now.Unix())
}

func (n *Node) VerifyIngestGrant(token string, now time.Time, nonces *mc.IngestGrantNonces) (mc.TopologyStreamKey, error) {
	n.mu.RLock()
	verifier, topology := n.verifier, n.topology
	n.mu.RUnlock()
	var keys []mc.TopologyStreamKey
	if topology.Ingest != nil {
		keys = topology.Ingest.StreamKeys
	}
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		ids = append(ids, key.KeyID)
	}
	_, keyID, err := verifier.VerifyIngestGrant(token, now.Unix(), mc.IngestGrantNode{StreamKeyIDs: ids, NodeID: n.NodeID, Nonces: nonces})
	if err != nil {
		return mc.TopologyStreamKey{}, err
	}
	for _, key := range keys {
		if key.KeyID == keyID {
			return key, nil
		}
	}
	return mc.TopologyStreamKey{}, errors.New("ingest grant key left the view")
}

func (n *Node) Revoked(nonce, identity string, now time.Time) bool {
	n.mu.RLock()
	verifier := n.verifier
	n.mu.RUnlock()
	return verifier.Revoked(nonce, identity, now.Unix())
}

var ErrNotChild = errors.New("service token of a node that is not a child")

func (n *Node) VerifyChild(token string, now time.Time) (string, error) {
	claims, err := n.VerifyToken(token, now)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(claims.KeyID, mc.ServiceKeyIDPrefix) || !strings.HasPrefix(claims.Identity, mc.NodeIdentityPrefix) {
		return "", ErrNotChild
	}
	child := strings.TrimPrefix(claims.Identity, mc.NodeIdentityPrefix)
	if !n.IsChild(child) {
		return "", ErrNotChild
	}
	return child, nil
}
