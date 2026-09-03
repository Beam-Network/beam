package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

const (
	SandboxOCI  = "oci"
	SandboxWASI = "wasi"
	SandboxNode = "node-legacy"

	sandboxNobodyID = 65532
)

var pinnedOCIImage = regexp.MustCompile(`^[^\s]+@sha256:[a-f0-9]{64}$`)

type SandboxExecution struct {
	Sandbox       contracts.ActionSandbox
	Entrypoint    string
	Workspace     string
	Timeout       time.Duration
	MemoryLimitMB int64
	CPUMillis     int64
	OutputLimit   int64
}

type SandboxResult struct {
	Result json.RawMessage
}

type SandboxRunner interface {
	Execute(context.Context, SandboxExecution) (SandboxResult, error)
}

type strongSandboxRunner struct {
	ociBinary      string
	wasmtimeBinary string
}

func newStrongSandboxRunner(ociBinary, wasmtimeBinary string) SandboxRunner {
	return &strongSandboxRunner{ociBinary: ociBinary, wasmtimeBinary: wasmtimeBinary}
}

func (r *strongSandboxRunner) Execute(ctx context.Context, execution SandboxExecution) (SandboxResult, error) {
	var command *exec.Cmd
	var err error
	switch execution.Sandbox.Runtime {
	case SandboxOCI:
		command, err = r.ociCommand(ctx, execution)
	case SandboxWASI:
		command, err = r.wasiCommand(ctx, execution)
	default:
		return SandboxResult{}, fmt.Errorf("unsupported strong sandbox runtime %q", execution.Sandbox.Runtime)
	}
	if err != nil {
		return SandboxResult{}, err
	}
	stdout := newBoundedBuffer(execution.OutputLimit)
	stderr := newBoundedBuffer(64 << 10)
	command.Stdout = stdout
	command.Stderr = stderr
	command.Stdin = nil
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return SandboxResult{}, fmt.Errorf("action sandbox terminated: %w", ctx.Err())
		}
		return SandboxResult{}, fmt.Errorf("action sandbox failed: %w: %s", err, stderr.String())
	}
	if stdout.Overflowed() || stderr.Overflowed() {
		return SandboxResult{}, errors.New("action sandbox output exceeded its limit")
	}
	return readSandboxResult(filepath.Join(execution.Workspace, "result.json"), execution.OutputLimit)
}

func (r *strongSandboxRunner) ociCommand(ctx context.Context, execution SandboxExecution) (*exec.Cmd, error) {
	if !pinnedOCIImage.MatchString(execution.Sandbox.OCIImage) {
		return nil, errors.New("OCI action image must be pinned by sha256 digest")
	}
	binary := r.ociBinary
	if binary == "" {
		var err error
		binary, err = exec.LookPath("docker")
		if err != nil {
			binary, err = exec.LookPath("podman")
		}
		if err != nil {
			return nil, errors.New("Docker or Podman is required for OCI action sandboxes")
		}
	}
	actionDirectory := filepath.Dir(execution.Entrypoint)
	if strings.ContainsAny(actionDirectory+execution.Workspace, ",\n\r") {
		return nil, errors.New("sandbox paths contain unsupported characters")
	}
	memoryBytes := execution.MemoryLimitMB << 20
	if memoryBytes <= 0 {
		memoryBytes = 128 << 20
	}
	cpus := float64(execution.CPUMillis) / 1000
	if cpus <= 0 {
		cpus = 1
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		// Root-owned Worker processes must not turn into root-owned action
		// containers. 65532 is the conventional non-root/nobody identity used
		// by distroless images and does not require an /etc/passwd entry.
		uid, gid = sandboxNobodyID, sandboxNobodyID
	}
	args := []string{"run", "--rm", "--pull=never", "--network=none", "--read-only",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=64",
		"--memory=" + strconv.FormatInt(memoryBytes, 10), "--cpus=" + strconv.FormatFloat(cpus, 'f', 3, 64),
		"--user=" + strconv.Itoa(uid) + ":" + strconv.Itoa(gid), "--tmpfs=/tmp:rw,noexec,nosuid,nodev,size=67108864",
		"--mount=type=bind,src=" + actionDirectory + ",dst=/action,readonly",
		"--mount=type=bind,src=" + execution.Workspace + ",dst=/workspace",
		"--env=BEAM_ACTION_ENTRYPOINT=/action/" + filepath.Base(execution.Entrypoint),
		"--env=BEAM_ACTION_REQUEST=/workspace/request.json",
		"--env=BEAM_ACTION_RESULT=/workspace/result.json",
		"--env=BEAM_HOST_RPC_DIR=/workspace/rpc",
		execution.Sandbox.OCIImage}
	args = append(args, execution.Sandbox.Command...)
	return exec.CommandContext(ctx, binary, args...), nil
}

func (r *strongSandboxRunner) wasiCommand(ctx context.Context, execution SandboxExecution) (*exec.Cmd, error) {
	binary := r.wasmtimeBinary
	if binary == "" {
		var err error
		binary, err = exec.LookPath("wasmtime")
		if err != nil {
			return nil, errors.New("Wasmtime is required for WASI action sandboxes")
		}
	}
	fuel := execution.Sandbox.Fuel
	if fuel == 0 {
		fuel = 100_000_000
	}
	memoryBytes := execution.MemoryLimitMB << 20
	if memoryBytes <= 0 {
		memoryBytes = 128 << 20
	}
	args := []string{"run",
		"-W", "fuel=" + strconv.FormatUint(fuel, 10),
		"-W", "max-memory-size=" + strconv.FormatInt(memoryBytes, 10),
		"-W", "max-instances=1", "-W", "max-memories=1", "-W", "max-tables=4",
		"--dir", execution.Workspace + "::/workspace",
		"--env", "BEAM_ACTION_REQUEST=/workspace/request.json",
		"--env", "BEAM_ACTION_RESULT=/workspace/result.json",
		"--env", "BEAM_HOST_RPC_DIR=/workspace/rpc",
		execution.Entrypoint}
	args = append(args, execution.Sandbox.Command...)
	return exec.CommandContext(ctx, binary, args...), nil
}

func readSandboxResult(path string, limit int64) (SandboxResult, error) {
	if limit <= 0 {
		limit = defaultOutputLimit
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return SandboxResult{}, errors.New("action sandbox did not produce a regular result.json")
	}
	file, err := os.Open(path)
	if err != nil {
		return SandboxResult{}, errors.New("action sandbox did not produce result.json")
	}
	defer file.Close()
	buffer := newBoundedBuffer(limit)
	if _, err := io.Copy(buffer, file); err != nil {
		return SandboxResult{}, err
	}
	if buffer.Overflowed() {
		return SandboxResult{}, errors.New("action sandbox result exceeded its limit")
	}
	var response struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(buffer.Bytes(), &response); err != nil {
		return SandboxResult{}, errors.New("action sandbox returned invalid result JSON")
	}
	if !response.OK {
		return SandboxResult{}, fmt.Errorf("action sandbox: %s", fallback(response.Error, "execution failed"))
	}
	if len(bytes.TrimSpace(response.Result)) == 0 || !json.Valid(response.Result) {
		return SandboxResult{}, errors.New("action sandbox result payload is invalid")
	}
	return SandboxResult{Result: response.Result}, nil
}
