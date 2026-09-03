package action

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

const (
	maximumRPCMessageBytes = 1 << 20
	defaultArtifactBytes   = 64 << 20
)

var rpcIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type HostRPC struct {
	workloadID   string
	taskID       string
	workspace    string
	allowed      map[string]struct{}
	secretBroker *httpSecretBroker
	publisher    *httpArtifactPublisher

	mu        sync.Mutex
	state     map[string]any
	artifacts []PublishedArtifact
}

type RPCRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args,omitempty"`
}

type RPCResponse struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Value any    `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

type ArtifactRequest struct {
	Name      string         `json:"name"`
	Path      string         `json:"path"`
	MediaType string         `json:"media_type,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type PublishedArtifact struct {
	ArtifactID string         `json:"artifact_id"`
	Name       string         `json:"name"`
	URI        string         `json:"uri"`
	MediaType  string         `json:"media_type,omitempty"`
	SizeBytes  int64          `json:"size_bytes"`
	SHA256     string         `json:"sha256"`
	ETag       string         `json:"etag,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type hostRPCConfig struct {
	HTTPClient   *http.Client
	ControlHosts []string
}

func newHostRPC(spec domain.Spec, action contracts.ActionExecution, workspace string, config hostRPCConfig) (*HostRPC, error) {
	allowed := make(map[string]struct{}, len(action.HostRPCMethods))
	for _, method := range action.HostRPCMethods {
		if !hostMethodPermitted(method, spec.Security.Permissions) {
			return nil, fmt.Errorf("host RPC method %q is not permitted by the workload policy", method)
		}
		allowed[method] = struct{}{}
	}
	client := restrictedHTTPClient(config.HTTPClient, config.ControlHosts)
	host := &HostRPC{workloadID: spec.WorkloadID, taskID: action.TaskID, workspace: workspace,
		allowed: allowed, state: make(map[string]any)}
	if action.SecretBroker != nil {
		if err := validateCapabilityEndpoint(action.SecretBroker.Endpoint, config.ControlHosts); err != nil {
			return nil, fmt.Errorf("secret broker: %w", err)
		}
		host.secretBroker = &httpSecretBroker{config: *action.SecretBroker, client: client, allowedHosts: config.ControlHosts}
	}
	if action.ArtifactPublisher != nil {
		if err := validateCapabilityEndpoint(action.ArtifactPublisher.Endpoint, config.ControlHosts); err != nil {
			return nil, fmt.Errorf("artifact publisher: %w", err)
		}
		if !boundedHeaders(action.ArtifactPublisher.Headers) {
			return nil, errors.New("artifact publisher headers are invalid or too large")
		}
		host.publisher = &httpArtifactPublisher{config: *action.ArtifactPublisher, client: client,
			workloadID: spec.WorkloadID, taskID: action.TaskID, allowedHosts: config.ControlHosts}
	}
	return host, nil
}

func (h *HostRPC) Call(ctx context.Context, request RPCRequest) RPCResponse {
	response := RPCResponse{ID: request.ID}
	if !rpcIDPattern.MatchString(request.ID) || request.Method == "" {
		response.Error = "invalid RPC request identity or method"
		return response
	}
	if _, allowed := h.allowed[request.Method]; !allowed {
		response.Error = fmt.Sprintf("host RPC method %q is not granted", request.Method)
		return response
	}
	value, err := h.call(ctx, request.Method, request.Args)
	if err != nil {
		response.Error = err.Error()
		return response
	}
	response.OK = true
	response.Value = value
	return response
}

func (h *HostRPC) call(ctx context.Context, method string, args json.RawMessage) (any, error) {
	switch method {
	case "logger.debug", "logger.info", "logger.warn", "logger.error":
		return nil, nil
	case "state.get":
		h.mu.Lock()
		defer h.mu.Unlock()
		return cloneMap(h.state), nil
	case "state.set":
		var state map[string]any
		if err := json.Unmarshal(args, &state); err != nil {
			return nil, errors.New("state.set requires a JSON object")
		}
		h.mu.Lock()
		h.state = cloneMap(state)
		h.mu.Unlock()
		return nil, nil
	case "state.patch":
		var patch map[string]any
		if err := json.Unmarshal(args, &patch); err != nil {
			return nil, errors.New("state.patch requires a JSON object")
		}
		h.mu.Lock()
		for key, value := range patch {
			h.state[key] = value
		}
		h.mu.Unlock()
		return nil, nil
	case "secrets.get":
		if h.secretBroker == nil {
			return nil, errors.New("secret broker capability is unavailable")
		}
		var input struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(args, &input); err != nil || input.Name == "" {
			return nil, errors.New("secrets.get requires a secret name")
		}
		return h.secretBroker.Get(ctx, h.workloadID, h.taskID, input.Name)
	case "artifacts.publish":
		if h.publisher == nil {
			return nil, errors.New("artifact publisher capability is unavailable")
		}
		var input ArtifactRequest
		if err := json.Unmarshal(args, &input); err != nil {
			return nil, errors.New("artifacts.publish requires an artifact descriptor")
		}
		published, err := h.publisher.Publish(ctx, h.workspace, input)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.artifacts = append(h.artifacts, published)
		h.mu.Unlock()
		return published, nil
	default:
		return nil, fmt.Errorf("unsupported host RPC method %q", method)
	}
}

func (h *HostRPC) ServeFilesystem(ctx context.Context, directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			entries, err := os.ReadDir(directory)
			if err != nil {
				return err
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".request.json") {
					continue
				}
				if err := h.handleRequestFile(ctx, directory, entry.Name()); err != nil {
					return err
				}
			}
		}
	}
}

func (h *HostRPC) handleRequestFile(ctx context.Context, directory, name string) error {
	requestPath := filepath.Join(directory, name)
	file, err := os.Open(requestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	encoded, readErr := io.ReadAll(io.LimitReader(file, maximumRPCMessageBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(encoded) > maximumRPCMessageBytes {
		return errors.New("host RPC request exceeds its size limit")
	}
	var request RPCRequest
	if err := json.Unmarshal(encoded, &request); err != nil {
		return err
	}
	if !rpcIDPattern.MatchString(request.ID) || name != request.ID+".request.json" {
		return errors.New("host RPC request filename does not match a valid request id")
	}
	response := h.Call(ctx, request)
	responsePath := filepath.Join(directory, request.ID+".response.json")
	if err := writeAtomicJSON(responsePath, response); err != nil {
		return err
	}
	return os.Remove(requestPath)
}

func (h *HostRPC) Artifacts() []PublishedArtifact {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]PublishedArtifact(nil), h.artifacts...)
}

type httpSecretBroker struct {
	config       contracts.ActionSecretBroker
	client       *http.Client
	allowedHosts []string
}

func (b *httpSecretBroker) Get(ctx context.Context, workloadID, taskID, name string) (string, error) {
	if len(name) > 256 || !secretAllowed(b.config.AllowedNames, name) {
		return "", errors.New("secret name is not granted by the workload capability")
	}
	body, _ := json.Marshal(map[string]string{"workload_id": workloadID, "task_id": taskID, "name": name})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	if b.config.BearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+b.config.BearerToken)
	}
	response, err := b.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if err := validateCapabilityEndpoint(response.Request.URL.String(), b.allowedHosts); err != nil {
		return "", fmt.Errorf("secret broker redirect: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("secret broker returned HTTP %d", response.StatusCode)
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil {
		return "", err
	}
	if len(encoded) > 64<<10 {
		return "", errors.New("secret broker response exceeds its size limit")
	}
	var result struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return "", errors.New("secret broker returned an invalid response")
	}
	return result.Value, nil
}

type httpArtifactPublisher struct {
	config       contracts.ActionArtifactPublisher
	client       *http.Client
	workloadID   string
	taskID       string
	allowedHosts []string
}

func (p *httpArtifactPublisher) Publish(ctx context.Context, workspace string, input ArtifactRequest) (PublishedArtifact, error) {
	if input.Name == "" || input.Path == "" || len(input.Name) > 512 {
		return PublishedArtifact{}, errors.New("artifact name and path are required")
	}
	path, err := safeJoin(workspace, input.Path)
	if err != nil {
		return PublishedArtifact{}, err
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return PublishedArtifact{}, err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return PublishedArtifact{}, errors.New("artifact scratch file is unavailable")
	}
	if relative, relErr := filepath.Rel(resolvedWorkspace, resolvedPath); relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return PublishedArtifact{}, errors.New("artifact path escapes the sandbox workspace")
	}
	file, err := os.Open(resolvedPath)
	if err != nil {
		return PublishedArtifact{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return PublishedArtifact{}, errors.New("artifact must be a regular file")
	}
	maximum := p.config.MaxArtifactBytes
	if maximum <= 0 {
		maximum = defaultArtifactBytes
	}
	if info.Size() > maximum {
		return PublishedArtifact{}, errors.New("artifact exceeds its publication capability limit")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return PublishedArtifact{}, err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return PublishedArtifact{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.config.Endpoint, file)
	if err != nil {
		return PublishedArtifact{}, err
	}
	request.ContentLength = info.Size()
	for name, value := range p.config.Headers {
		request.Header.Set(name, value)
	}
	if p.config.BearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+p.config.BearerToken)
	}
	request.Header.Set("Content-Type", fallback(input.MediaType, "application/octet-stream"))
	request.Header.Set("X-Beam-Artifact-Name", input.Name)
	request.Header.Set("X-Beam-Workload-ID", p.workloadID)
	request.Header.Set("X-Beam-Task-ID", p.taskID)
	request.Header.Set("X-Beam-Artifact-SHA256", digest)
	response, err := p.client.Do(request)
	if err != nil {
		return PublishedArtifact{}, err
	}
	defer response.Body.Close()
	if err := validateCapabilityEndpoint(response.Request.URL.String(), p.allowedHosts); err != nil {
		return PublishedArtifact{}, fmt.Errorf("artifact publisher redirect: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return PublishedArtifact{}, fmt.Errorf("artifact publisher returned HTTP %d", response.StatusCode)
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil {
		return PublishedArtifact{}, err
	}
	if len(encoded) > 64<<10 {
		return PublishedArtifact{}, errors.New("artifact publisher response exceeds its size limit")
	}
	var result struct {
		ArtifactID string `json:"artifact_id"`
		URI        string `json:"uri"`
		ETag       string `json:"etag"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil || result.ArtifactID == "" || result.URI == "" {
		return PublishedArtifact{}, errors.New("artifact publisher returned an invalid durable reference")
	}
	return PublishedArtifact{ArtifactID: result.ArtifactID, Name: input.Name, URI: result.URI,
		MediaType: input.MediaType, SizeBytes: info.Size(), SHA256: digest,
		ETag: result.ETag, Metadata: input.Metadata}, nil
}

func hostMethodPermitted(method string, permissions []string) bool {
	switch method {
	case "logger.debug", "logger.info", "logger.warn", "logger.error", "state.get", "state.set", "state.patch":
		return true
	case "secrets.get":
		return containsPermission(permissions, "secrets:read")
	case "artifacts.publish":
		return containsPermission(permissions, "storage:write")
	default:
		return false
	}
}

func secretAllowed(allowed []string, name string) bool {
	for _, candidate := range allowed {
		if candidate == name || candidate == "*" || (strings.HasSuffix(candidate, "*") && strings.HasPrefix(name, strings.TrimSuffix(candidate, "*"))) {
			return true
		}
	}
	return false
}

func cloneMap(input map[string]any) map[string]any {
	encoded, _ := json.Marshal(input)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	return result
}

func writeAtomicJSON(path string, value any) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".rpc-response-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if os.Getuid() == 0 {
		if err := temporary.Chown(sandboxNobodyID, sandboxNobodyID); err != nil {
			temporary.Close()
			return err
		}
	}
	if err := json.NewEncoder(temporary).Encode(value); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func fallback(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}
