package subsystem

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
	"github.com/Beam-Network/beam/internal/workload/runtime"
)

const ConfigEnvironment = "BEAM_WORKER_SUBSYSTEM_CONFIG"

var ErrProcessExited = errors.New("Worker subsystem process exited abnormally")

type commandFactory func(context.Context, domain.Kind, string) (*exec.Cmd, error)

// ProcessHandler validates in the control process and executes in a fresh,
// specialized subprocess. A crash is contained to the workload and recovery
// starts another subprocess from the last durable checkpoint.
type ProcessHandler struct {
	delegate runtime.Handler
	config   Config
	command  commandFactory
}

func NewProcessHandler(delegate runtime.Handler, config Config) (*ProcessHandler, error) {
	if delegate == nil {
		return nil, errors.New("subsystem delegate is required")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &ProcessHandler{delegate: delegate, config: config, command: func(ctx context.Context, kind domain.Kind, encoded string) (*exec.Cmd, error) {
		command := exec.CommandContext(ctx, executable, "subsystem", "--kind", string(kind))
		command.Env = append(sanitizedEnvironment(), ConfigEnvironment+"="+encoded)
		command.Cancel = func() error {
			if command.Process == nil {
				return os.ErrProcessDone
			}
			return command.Process.Signal(syscall.SIGTERM)
		}
		command.WaitDelay = 5 * time.Second
		return command, nil
	}}, nil
}

func (h *ProcessHandler) Kind() domain.Kind               { return h.delegate.Kind() }
func (h *ProcessHandler) Validate(spec domain.Spec) error { return h.delegate.Validate(spec) }

func (h *ProcessHandler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	const maximumAttempts = 2
	for attempt := 1; attempt <= maximumAttempts; attempt++ {
		result, err := h.executeOnce(ctx, spec)
		if err == nil || !errors.Is(err, ErrProcessExited) || ctx.Err() != nil || attempt == maximumAttempts {
			return result, err
		}
		workloadprogress.Report(ctx, map[string]string{
			"phase": "subsystem_restarting", "kind": string(h.Kind()), "attempt": fmt.Sprint(attempt + 1),
		})
	}
	return domain.Result{}, ErrProcessExited
}

func (h *ProcessHandler) executeOnce(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	config, err := json.Marshal(h.config.ForKind(h.Kind()))
	if err != nil {
		return domain.Result{}, err
	}
	encodedConfig := base64.RawURLEncoding.EncodeToString(config)
	command, err := h.command(ctx, h.Kind(), encodedConfig)
	if err != nil {
		return domain.Result{}, err
	}
	request, err := json.Marshal(Request{Spec: spec, Checkpoint: workloadcheckpoint.Snapshot(ctx)})
	if err != nil {
		return domain.Result{}, err
	}
	command.Stdin = bytes.NewReader(request)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return domain.Result{}, err
	}
	stderr := &boundedBuffer{maximum: 64 << 10}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return domain.Result{}, fmt.Errorf("start %s subsystem: %w", h.Kind(), err)
	}
	var result *domain.Result
	var executionError error
	streamErr := decodeEvents(stdout, func(event Event) error {
		switch event.Type {
		case "progress":
			workloadprogress.Report(ctx, event.Progress)
		case "checkpoint":
			if event.Checkpoint == nil {
				return errors.New("subsystem emitted an empty checkpoint")
			}
			if err := workloadcheckpoint.Apply(ctx, *event.Checkpoint); err != nil {
				return err
			}
		case "result":
			if event.Result == nil {
				return errors.New("subsystem emitted an empty result")
			}
			copy := *event.Result
			result = &copy
			if event.Error != "" {
				executionError = errors.New(event.Error)
			}
		case "error":
			if event.Error == "" {
				return errors.New("subsystem emitted an unspecified error")
			}
			executionError = errors.New(event.Error)
		default:
			return fmt.Errorf("unknown subsystem event %q", event.Type)
		}
		return nil
	})
	waitErr := command.Wait()
	if streamErr != nil {
		return domain.Result{}, streamErr
	}
	if ctx.Err() != nil {
		return domain.Result{}, ctx.Err()
	}
	if waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return domain.Result{}, fmt.Errorf("%w: %s: %v: %s", ErrProcessExited, h.Kind(), waitErr, message)
		}
		return domain.Result{}, fmt.Errorf("%w: %s: %v", ErrProcessExited, h.Kind(), waitErr)
	}
	if result == nil {
		if executionError != nil {
			return domain.Result{}, executionError
		}
		return domain.Result{}, errors.New("subsystem exited without a result")
	}
	return *result, executionError
}

type boundedBuffer struct {
	buffer  bytes.Buffer
	maximum int
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.maximum - b.buffer.Len()
	if remaining > 0 {
		_, _ = b.buffer.Write(value[:min(len(value), remaining)])
	}
	return original, nil
}

func (b *boundedBuffer) String() string { return b.buffer.String() }

func sanitizedEnvironment() []string {
	allowed := []string{
		"PATH", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "DOCKER_HOST", "CONTAINER_HOST",
	}
	result := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok {
			result = append(result, name+"="+value)
		}
	}
	return result
}
