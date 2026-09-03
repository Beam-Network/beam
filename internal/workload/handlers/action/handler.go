package action

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
)

const defaultOutputLimit = 1 << 20
const actionCheckpointSchema = "beam.studio.action/1"

type actionCheckpoint struct {
	TaskID string         `json:"task_id"`
	Phase  string         `json:"phase"`
	Result *domain.Result `json:"result,omitempty"`
}

//go:embed runner.mjs
var runnerSource []byte

type Config struct {
	ActionRoot                string
	CacheRoot                 string
	ScratchRoot               string
	NodeBinary                string
	OCIBinary                 string
	WasmtimeBinary            string
	AllowedPermissions        []string
	AllowedHostRPCMethods     []string
	RegistryHosts             []string
	ControlHosts              []string
	TrustedPublisherKeys      []string
	MaximumTimeout            time.Duration
	MemoryLimitMB             int64
	OutputLimitBytes          int64
	MaximumActionBytes        int64
	RequirePublisherSignature bool
	AllowLegacyNode           bool
	HTTPClient                *http.Client
	SandboxRunner             SandboxRunner
}

type Handler struct {
	config Config
	cache  *ArtifactCache
	runner SandboxRunner
}

func NewHandler(config Config) (*Handler, error) {
	if config.ScratchRoot == "" {
		return nil, errors.New("Studio action scratch root is required")
	}
	if config.CacheRoot == "" {
		config.CacheRoot = filepath.Join(filepath.Dir(config.ScratchRoot), "action-cache")
	}
	if config.MaximumTimeout <= 0 {
		config.MaximumTimeout = 5 * time.Minute
	}
	if config.MemoryLimitMB <= 0 {
		config.MemoryLimitMB = 128
	}
	if config.OutputLimitBytes <= 0 {
		config.OutputLimitBytes = defaultOutputLimit
	}
	if len(config.AllowedHostRPCMethods) == 0 {
		config.AllowedHostRPCMethods = []string{
			"logger.debug", "logger.info", "logger.warn", "logger.error",
			"state.get", "state.set", "state.patch", "secrets.get", "artifacts.publish",
		}
	}
	if config.AllowLegacyNode {
		if config.NodeBinary == "" {
			path, err := exec.LookPath("node")
			if err != nil {
				return nil, errors.New("Node.js is required when the legacy Studio sandbox is enabled")
			}
			config.NodeBinary = path
		}
		if config.ActionRoot != "" {
			if info, err := os.Stat(config.ActionRoot); err != nil || !info.IsDir() {
				return nil, errors.New("Studio action root must be an existing directory")
			}
		}
	}
	if err := os.MkdirAll(config.ScratchRoot, 0o700); err != nil {
		return nil, err
	}
	cache, err := NewArtifactCache(config.CacheRoot, config.HTTPClient, config.MaximumActionBytes,
		config.RequirePublisherSignature, config.RegistryHosts, config.TrustedPublisherKeys)
	if err != nil {
		return nil, err
	}
	runner := config.SandboxRunner
	if runner == nil {
		runner = newStrongSandboxRunner(config.OCIBinary, config.WasmtimeBinary)
	}
	return &Handler{config: config, cache: cache, runner: runner}, nil
}

func (h *Handler) Kind() domain.Kind { return domain.KindActionExecute }

func (h *Handler) Validate(spec domain.Spec) error {
	action, err := decodeAction(spec)
	if err != nil {
		return err
	}
	return h.validate(spec, action)
}

func (h *Handler) validate(spec domain.Spec, action contracts.ActionExecution) error {
	if action.TaskID == "" || action.ActionPackageName == "" || action.Entrypoint == "" || action.ArtifactSHA256 == "" {
		return errors.New("Studio task, action package, entrypoint, and artifact SHA-256 are required")
	}
	if _, err := normalizedSHA256(action.ArtifactSHA256); err != nil {
		return err
	}
	for _, permission := range spec.Security.Permissions {
		if !permissionAllowed(h.config.AllowedPermissions, permission) {
			return fmt.Errorf("Studio permission %q is not allowed by this Worker", permission)
		}
	}
	runtime := action.Sandbox.Runtime
	if runtime == "" && action.Sandbox.LegacyNode {
		runtime = SandboxNode
	}
	if runtime == "" && h.config.AllowLegacyNode {
		runtime = SandboxNode
	}
	switch runtime {
	case SandboxOCI:
		if action.RegistryArtifact == nil {
			return errors.New("OCI actions require a registry artifact capability")
		}
		if !pinnedOCIImage.MatchString(action.Sandbox.OCIImage) {
			return errors.New("OCI action image must be pinned by sha256 digest")
		}
	case SandboxWASI:
		if action.RegistryArtifact == nil {
			return errors.New("WASI actions require a registry artifact capability")
		}
	case SandboxNode:
		if !h.config.AllowLegacyNode {
			return errors.New("legacy Node action sandbox is disabled; use OCI or WASI")
		}
		if len(action.HostRPCMethods) > 0 {
			return errors.New("host RPC is available only to OCI or WASI sandboxes")
		}
	default:
		return fmt.Errorf("unsupported Studio sandbox runtime %q", runtime)
	}
	for _, method := range action.HostRPCMethods {
		if !slices.Contains(h.config.AllowedHostRPCMethods, method) {
			return fmt.Errorf("host RPC method %q is disabled by Worker policy", method)
		}
		if !hostMethodPermitted(method, spec.Security.Permissions) {
			return fmt.Errorf("host RPC method %q is not permitted by the workload", method)
		}
	}
	if slices.Contains(action.HostRPCMethods, "secrets.get") {
		if action.SecretBroker == nil || len(action.SecretBroker.AllowedNames) == 0 {
			return errors.New("secrets.get requires a scoped secret broker capability")
		}
	}
	if slices.Contains(action.HostRPCMethods, "artifacts.publish") && action.ArtifactPublisher == nil {
		return errors.New("artifacts.publish requires an artifact publisher capability")
	}
	if action.RegistryArtifact != nil {
		if err := validateCapabilityEndpoint(action.RegistryArtifact.URL, h.config.RegistryHosts); err != nil {
			return fmt.Errorf("registry artifact: %w", err)
		}
	}
	if action.SecretBroker != nil {
		if err := validateCapabilityEndpoint(action.SecretBroker.Endpoint, h.config.ControlHosts); err != nil {
			return fmt.Errorf("secret broker: %w", err)
		}
	}
	if action.ArtifactPublisher != nil {
		if err := validateCapabilityEndpoint(action.ArtifactPublisher.Endpoint, h.config.ControlHosts); err != nil {
			return fmt.Errorf("artifact publisher: %w", err)
		}
	}
	return nil
}

func (h *Handler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	action, err := decodeAction(spec)
	if err != nil {
		return domain.Result{}, err
	}
	if err := h.validate(spec, action); err != nil {
		return domain.Result{}, err
	}
	var resume actionCheckpoint
	if _, ok, checkpointErr := workloadcheckpoint.Current(ctx, actionCheckpointSchema, &resume); checkpointErr != nil {
		return domain.Result{}, fmt.Errorf("restore Studio action checkpoint: %w", checkpointErr)
	} else if ok {
		if resume.TaskID != action.TaskID {
			return domain.Result{}, errors.New("Studio action checkpoint belongs to another task")
		}
		if resume.Phase == "completed" && resume.Result != nil {
			return *resume.Result, nil
		}
		if resume.Phase == "sandbox_started" {
			return domain.Result{}, errors.New("Studio action was interrupted after sandbox start; refusing an unsafe automatic replay")
		}
	}
	executionContext, cancel := context.WithTimeout(ctx, h.executionTimeout(action))
	defer cancel()
	entrypoint, err := h.resolve(executionContext, action)
	if err != nil {
		return domain.Result{}, err
	}
	if err := saveActionCheckpoint(ctx, actionCheckpoint{TaskID: action.TaskID, Phase: "resolved"}); err != nil {
		return domain.Result{}, err
	}
	runtime := action.Sandbox.Runtime
	if runtime == "" && (action.Sandbox.LegacyNode || h.config.AllowLegacyNode) {
		runtime = SandboxNode
	}
	if err := saveActionCheckpoint(ctx, actionCheckpoint{TaskID: action.TaskID, Phase: "sandbox_started"}); err != nil {
		return domain.Result{}, err
	}
	var result domain.Result
	if runtime == SandboxNode {
		result, err = h.executeLegacy(executionContext, action, entrypoint)
	} else {
		result, err = h.executeStrong(executionContext, spec, action, entrypoint)
	}
	if err != nil {
		return result, err
	}
	if err := saveActionCheckpoint(ctx, actionCheckpoint{TaskID: action.TaskID, Phase: "completed", Result: &result}); err != nil {
		return domain.Result{}, err
	}
	return result, nil
}

func saveActionCheckpoint(ctx context.Context, value actionCheckpoint) error {
	err := workloadcheckpoint.Save(ctx, actionCheckpointSchema, map[string]string{"task_id": value.TaskID, "phase": value.Phase}, value)
	if errors.Is(err, workloadcheckpoint.ErrUnavailable) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("save Studio action checkpoint: %w", err)
	}
	return nil
}

func (h *Handler) executeStrong(ctx context.Context, spec domain.Spec, action contracts.ActionExecution, entrypoint string) (domain.Result, error) {
	timeout := h.executionTimeout(action)
	executionContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	workspace, err := os.MkdirTemp(h.config.ScratchRoot, "beam-action-*")
	if err != nil {
		return domain.Result{}, err
	}
	defer os.RemoveAll(workspace)
	request := map[string]any{
		"abi_version": "beam.action/1", "task_id": action.TaskID,
		"input": action.Input, "config": action.Config,
		"host_rpc_directory": "/workspace/rpc",
	}
	if err := writeAtomicJSON(filepath.Join(workspace, "request.json"), request); err != nil {
		return domain.Result{}, err
	}
	rpcDirectory := filepath.Join(workspace, "rpc")
	if err := os.MkdirAll(rpcDirectory, 0o700); err != nil {
		return domain.Result{}, err
	}
	if os.Getuid() == 0 {
		// OCI actions always run as an unprivileged identity, even when the
		// Worker host process is root.
		if err := os.Chown(workspace, sandboxNobodyID, sandboxNobodyID); err != nil {
			return domain.Result{}, err
		}
		if err := os.Chown(rpcDirectory, sandboxNobodyID, sandboxNobodyID); err != nil {
			return domain.Result{}, err
		}
	}
	host, err := newHostRPC(spec, action, workspace, hostRPCConfig{HTTPClient: h.config.HTTPClient, ControlHosts: h.config.ControlHosts})
	if err != nil {
		return domain.Result{}, err
	}
	hostContext, stopHost := context.WithCancel(executionContext)
	hostErrors := make(chan error, 1)
	go func() { hostErrors <- host.ServeFilesystem(hostContext, rpcDirectory) }()
	execution := SandboxExecution{Sandbox: action.Sandbox, Entrypoint: entrypoint, Workspace: workspace,
		Timeout: timeout, MemoryLimitMB: h.config.MemoryLimitMB, CPUMillis: max(spec.Resources.CPUMillis, 100),
		OutputLimit: h.config.OutputLimitBytes}
	sandboxResult, runErr := h.runner.Execute(executionContext, execution)
	stopHost()
	select {
	case hostErr := <-hostErrors:
		if hostErr != nil && runErr == nil {
			return domain.Result{}, hostErr
		}
	case <-time.After(time.Second):
		if runErr == nil {
			return domain.Result{}, errors.New("host RPC server did not stop")
		}
	}
	if runErr != nil {
		return domain.Result{}, runErr
	}
	artifacts, err := json.Marshal(host.Artifacts())
	if err != nil {
		return domain.Result{}, err
	}
	return domain.Result{Outputs: map[string]string{"result": string(sandboxResult.Result), "artifacts": string(artifacts)}}, nil
}

func (h *Handler) executeLegacy(ctx context.Context, action contracts.ActionExecution, entrypoint string) (domain.Result, error) {
	timeout := h.executionTimeout(action)
	executionContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	scratch, err := os.MkdirTemp(h.config.ScratchRoot, "beam-action-legacy-*")
	if err != nil {
		return domain.Result{}, err
	}
	defer os.RemoveAll(scratch)
	runnerPath := filepath.Join(scratch, "runner.mjs")
	if err := os.WriteFile(runnerPath, runnerSource, 0o500); err != nil {
		return domain.Result{}, err
	}
	request, err := json.Marshal(map[string]any{"input": action.Input, "config": action.Config,
		"cpuTimeoutMs": min(timeout.Milliseconds(), int64(30_000))})
	if err != nil {
		return domain.Result{}, err
	}
	command := exec.CommandContext(executionContext, h.config.NodeBinary,
		"--permission", "--allow-fs-read="+runnerPath, "--allow-fs-read="+entrypoint,
		"--allow-fs-write="+scratch, "--experimental-vm-modules", "--frozen-intrinsics",
		"--disable-proto=throw", "--no-addons", fmt.Sprintf("--max-old-space-size=%d", h.config.MemoryLimitMB),
		runnerPath, entrypoint)
	command.Dir = scratch
	command.Env = []string{"HOME=" + scratch, "TMPDIR=" + scratch}
	command.Stdin = bytes.NewReader(request)
	stdout := newBoundedBuffer(h.config.OutputLimitBytes)
	stderr := newBoundedBuffer(64 << 10)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil && executionContext.Err() != nil {
		return domain.Result{}, fmt.Errorf("Studio sandbox terminated: %w", executionContext.Err())
	} else if err != nil && stdout.Len() == 0 {
		return domain.Result{}, fmt.Errorf("Studio sandbox failed: %w: %s", err, stderr.String())
	}
	if stdout.Overflowed() {
		return domain.Result{}, errors.New("Studio sandbox output exceeded its limit")
	}
	var response struct {
		OK        bool            `json:"ok"`
		Result    json.RawMessage `json:"result"`
		Artifacts json.RawMessage `json:"artifacts"`
		Error     struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		return domain.Result{}, fmt.Errorf("decode Studio sandbox result: %w", err)
	}
	if !response.OK {
		return domain.Result{}, fmt.Errorf("Studio action %s: %s", response.Error.Name, response.Error.Message)
	}
	return domain.Result{Outputs: map[string]string{"result": string(response.Result), "artifacts": string(response.Artifacts)}}, nil
}

func (h *Handler) resolve(ctx context.Context, action contracts.ActionExecution) (string, error) {
	if action.RegistryArtifact != nil {
		return h.cache.Ensure(ctx, action)
	}
	return h.resolveLocal(action)
}

func (h *Handler) resolveLocal(action contracts.ActionExecution) (string, error) {
	if h.config.ActionRoot == "" {
		return "", errors.New("local action root is unavailable and no registry artifact was provided")
	}
	version := action.ActionVersion
	if version == "" {
		version = "latest"
	}
	relative := filepath.Join(action.ActionPackageName, version, action.Entrypoint)
	root, err := filepath.Abs(h.config.ActionRoot)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	entrypoint, err := safeJoin(root, relative)
	if err != nil {
		return "", err
	}
	entrypoint, err = filepath.EvalSymlinks(entrypoint)
	if err != nil {
		return "", errors.New("Studio action entrypoint is unavailable")
	}
	if relative, relErr := filepath.Rel(root, entrypoint); relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("Studio action entrypoint escapes the action root")
	}
	if err := verifySHA256(entrypoint, action.ArtifactSHA256); err != nil {
		return "", err
	}
	return entrypoint, nil
}

func (h *Handler) executionTimeout(action contracts.ActionExecution) time.Duration {
	timeout := time.Duration(action.TimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > h.config.MaximumTimeout {
		return h.config.MaximumTimeout
	}
	return timeout
}

func decodeAction(spec domain.Spec) (contracts.ActionExecution, error) {
	var action contracts.ActionExecution
	if err := json.Unmarshal(spec.Payload, &action); err != nil {
		return action, fmt.Errorf("decode Studio action: %w", err)
	}
	return action, nil
}

func verifySHA256(path, expected string) error {
	expected, err := normalizedSHA256(expected)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected) {
		return errors.New("Studio action artifact checksum mismatch")
	}
	return nil
}

func permissionAllowed(allowed []string, required string) bool {
	if len(allowed) == 0 {
		return false
	}
	if slices.Contains(allowed, required) || slices.Contains(allowed, "*") {
		return true
	}
	category, _, _ := strings.Cut(required, ":")
	return slices.Contains(allowed, category+":*")
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	overflow bool
}

func newBoundedBuffer(limit int64) *boundedBuffer { return &boundedBuffer{limit: limit} }

func (b *boundedBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := b.limit - int64(b.buffer.Len())
	if remaining <= 0 {
		b.overflow = true
		return originalLength, nil
	}
	if int64(len(value)) > remaining {
		value = value[:remaining]
		b.overflow = true
	}
	_, _ = b.buffer.Write(value)
	return originalLength, nil
}

func (b *boundedBuffer) Bytes() []byte    { return b.buffer.Bytes() }
func (b *boundedBuffer) Len() int         { return b.buffer.Len() }
func (b *boundedBuffer) String() string   { return b.buffer.String() }
func (b *boundedBuffer) Overflowed() bool { return b.overflow }
