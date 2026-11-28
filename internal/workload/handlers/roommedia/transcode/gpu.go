package transcode

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Beam-Network/beam/internal/worker/gpulock"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
)

var gpuProbe struct {
	sync.Once
	available bool
	sessions  int
}

func GPUAvailable(config broadcast.TranscodeConfig) bool {
	if config.GPU == "off" {
		return false
	}
	if config.FleetGPUs {
		return (config.GPUAllDevices || len(config.GPUDevices) > 0) && !leftoversPending.Load()
	}
	if config.GPU == "on" {
		return true
	}
	gpuProbe.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "nvidia-smi", "-L").Output()
		if err != nil {
			return
		}
		count := strings.Count(string(output), "GPU ")
		gpuProbe.available = count > 0
		gpuProbe.sessions = 8 * count
	})
	return gpuProbe.available
}

var encoderSessions atomic.Int64

func EncoderSessions(config broadcast.TranscodeConfig) (inUse, offered int64) {
	offered = int64(config.EncoderSessions)
	if offered == 0 && !config.FleetGPUs && GPUAvailable(config) {
		offered = int64(gpuProbe.sessions)
	}
	return encoderSessions.Load(), offered
}

var (
	errNoSessions = errors.New("no NVENC session left on this Worker")
	errNoGPU      = errors.New("no transcoder GPU is free: GPU seats hold them, or their NVENC sessions are taken")
)

func reserveSessions(config broadcast.TranscodeConfig, count int) error {
	if count == 0 {
		return nil
	}
	_, offered := EncoderSessions(config)
	if offered > 0 && encoderSessions.Add(int64(count)) > offered {
		encoderSessions.Add(-int64(count))
		return errNoSessions
	}
	if offered == 0 {
		encoderSessions.Add(int64(count))
	}
	return nil
}

func releaseSessions(count int) {
	if count > 0 {
		encoderSessions.Add(-int64(count))
	}
}

type encoderLease struct {
	device   string
	index    int
	sessions int
	lock     *gpulock.Lock
	release  func()
}

func (l *encoderLease) lockFile() *os.File {
	if l == nil {
		return nil
	}
	return l.lock.File()
}

var deviceSessions = struct {
	sync.Mutex
	inUse map[int]int64
}{inUse: map[int]int64{}}

func acquireEncoder(config broadcast.TranscodeConfig, count, prefer int) (*encoderLease, error) {
	if count == 0 {
		return &encoderLease{index: -1, release: func() {}}, nil
	}
	if err := reserveSessions(config, count); err != nil {
		return nil, err
	}
	if !config.FleetGPUs || config.GPUAllDevices {
		return &encoderLease{device: "all", index: -1, sessions: count, release: func() { releaseSessions(count) }}, nil
	}
	device, lock, unlock, err := lockDevice(config, int64(count), prefer)
	if err != nil {
		releaseSessions(count)
		return nil, err
	}
	var once sync.Once
	return &encoderLease{device: strconv.Itoa(device), index: device, sessions: count, lock: lock, release: func() {
		once.Do(func() {
			unlock()
			releaseSessions(count)
		})
	}}, nil
}

const (
	acquireRetry = 250 * time.Millisecond
	acquireWait  = 10 * time.Second
)

func acquireEncoderWaiting(ctx context.Context, config broadcast.TranscodeConfig, count, prefer int) (*encoderLease, error) {
	deadline := time.Now().Add(acquireWait)
	for {
		lease, err := acquireEncoder(config, count, prefer)
		if err == nil || !(errors.Is(err, errNoGPU) || errors.Is(err, errNoSessions)) || time.Now().After(deadline) {
			return lease, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(acquireRetry):
		}
	}
}

func lockDevice(config broadcast.TranscodeConfig, count int64, prefer int) (int, *gpulock.Lock, func(), error) {
	deviceSessions.Lock()
	defer deviceSessions.Unlock()
	inUse := deviceSessions.inUse
	rank := func(device broadcast.GPUDevice) int {
		switch {
		case device.Index == prefer:
			return 0
		case int64(device.Index) >= config.SeatGPUs:
			return 1
		case inUse[device.Index] > 0:
			return 2
		}
		return 3
	}
	candidates := slices.Clone(config.GPUDevices)
	slices.SortStableFunc(candidates, func(a, b broadcast.GPUDevice) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		switch rank(a) {
		case 1:
			return cmp.Compare(inUse[a.Index], inUse[b.Index])
		case 2:
			return cmp.Compare(inUse[b.Index], inUse[a.Index])
		}
		return cmp.Compare(b.Index, a.Index)
	})
	var refused error
	for _, candidate := range candidates {
		if candidate.Sessions > 0 && inUse[candidate.Index]+count > candidate.Sessions {
			continue
		}
		var lock *gpulock.Lock
		if config.GPULockDirectory != "" {
			held, err := gpulock.Shared(config.GPULockDirectory, candidate.Index)
			if err != nil {
				refused = err
				continue
			}
			lock = held
		}
		index := candidate.Index
		inUse[index] += count
		return index, lock, func() {
			deviceSessions.Lock()
			deviceSessions.inUse[index] -= count
			deviceSessions.Unlock()
			lock.Release()
		}, nil
	}
	if refused != nil {
		return 0, nil, nil, fmt.Errorf("%w (%v)", errNoGPU, refused)
	}
	return 0, nil, nil, errNoGPU
}

var (
	releaseRetry    = 15 * time.Second
	releaseAttempts = 8
)

func (l *encoderLease) releaseAfterRemoval(removeErr error, remove func() error, logf func(string, ...any)) {
	if removeErr == nil || remove == nil || l.device == "" {
		l.release()
		return
	}
	logf("media.transcode: the encoder container on GPU %s was not removed (%v); its GPU stays locked until it is", l.device, removeErr)
	retry, attempts := releaseRetry, releaseAttempts
	go func() {
		for attempt := 0; attempt < attempts; attempt++ {
			time.Sleep(retry)
			if err := remove(); err == nil {
				l.release()
				return
			}
		}
		logf("media.transcode: the encoder container on GPU %s is still not removed; the GPU stays locked until the media plane restarts", l.device)
	}()
}

const (
	ContainerWorkerLabel   = "beam.media.transcode.worker"
	ContainerGPULabel      = "beam.media.transcode.gpu.devices"
	ContainerGPUOwnerLabel = "beam.media.transcode.gpu.owner"
)

func containerArgs(config broadcast.TranscodeConfig, device string) []string {
	var args []string
	if config.WorkerID != "" {
		args = append(args, "--label", ContainerWorkerLabel+"="+config.WorkerID)
	}
	if device == "" {
		return args
	}
	gpus := "all"
	if device != "all" {
		gpus = "device=" + device
		args = append(args, "--label", ContainerGPULabel+"="+device)
		if config.GPULockDirectory != "" {
			args = append(args, "--label", ContainerGPUOwnerLabel+"="+gpulock.Owner(config.GPULockDirectory))
		}
	}
	return append(args, "--gpus", gpus, "-e", "NVIDIA_DRIVER_CAPABILITIES=compute,video,utility")
}

const leftoverRemoveTimeout = 2 * time.Minute

func RemoveLeftoverContainers(ctx context.Context, config broadcast.TranscodeConfig) error {
	if config.Runtime == "local" || config.WorkerID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, leftoverRemoveTimeout)
	defer cancel()
	binary := config.OCIBinary
	if binary == "" {
		binary = "docker"
	}
	output, err := exec.CommandContext(ctx, binary, "ps", "-aq", "--filter", "label="+ContainerWorkerLabel+"="+config.WorkerID).Output()
	if err != nil {
		return fmt.Errorf("list the transcoder containers: %w", withStderr(err))
	}
	ids := strings.Fields(string(output))
	if len(ids) == 0 {
		return nil
	}
	removed, err := exec.CommandContext(ctx, binary, append([]string{"rm", "-f"}, ids...)...).CombinedOutput()
	if err == nil {
		return nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(removed)), "\n") {
		if !strings.Contains(strings.ToLower(line), "no such container") {
			return fmt.Errorf("remove the transcoder containers %s: %w: %s", strings.Join(ids, " "), err, strings.TrimSpace(string(removed)))
		}
	}
	return nil
}

var leftoversPending atomic.Bool

const leftoverRetry = 15 * time.Second

func StartLeftoverRemoval(ctx context.Context, config broadcast.TranscodeConfig, logf func(string, ...any)) <-chan struct{} {
	return startLeftoverRemoval(ctx, config, leftoverRetry, logf)
}

func startLeftoverRemoval(ctx context.Context, config broadcast.TranscodeConfig, retry time.Duration, logf func(string, ...any)) <-chan struct{} {
	settled := make(chan struct{})
	leftoversPending.Store(true)
	go func() {
		var once sync.Once
		settle := func() { once.Do(func() { close(settled) }) }
		defer settle()
		logged := ""
		for {
			err := RemoveLeftoverContainers(ctx, config)
			if err == nil {
				leftoversPending.Store(false)
				if logged != "" {
					logf("media.transcode: GPU encoding accepted: the transcoder containers a previous media plane left are removed")
				}
				return
			}
			if err.Error() != logged {
				logf("media.transcode: GPU encoding refused until the transcoder containers a previous media plane left are removed: %v; retried every %s", err, retry)
				logged = err.Error()
			}
			settle()
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
			}
		}
	}()
	return settled
}

func withStderr(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if stderr := strings.TrimSpace(string(exit.Stderr)); stderr != "" {
			return fmt.Errorf("%w: %s", err, stderr)
		}
	}
	return err
}
