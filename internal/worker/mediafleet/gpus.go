package mediafleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/worker/gpulock"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type GPUSelection struct {
	Auto    bool
	None    bool
	Devices []int
}

func ParseGPUs(value string) (GPUSelection, error) {
	switch value = strings.TrimSpace(value); value {
	case "", "auto":
		return GPUSelection{Auto: true}, nil
	case "none":
		return GPUSelection{None: true}, nil
	}
	devices, err := gpulock.ParseDevices(value)
	if err != nil {
		return GPUSelection{}, fmt.Errorf("--media-gpus: %v", err)
	}
	if len(devices) == 0 {
		return GPUSelection{}, errors.New("--media-gpus must be auto, none or a list of GPU indexes")
	}
	return GPUSelection{Devices: devices}, nil
}

func (s GPUSelection) explicit() bool { return !s.Auto && !s.None }

type EncoderGPU struct {
	Index    int
	Model    string
	Sessions int64
}

type GPUInfo struct {
	Vendor             string
	Model              string
	MaxEncoderSessions int64
	Devices            []EncoderGPU
}

const consumerNVENCSessions = 8

const professionalNVENCSessions = 16

type DetectedGPU struct {
	Index int
	Name  string
}

func DetectGPUs(ctx context.Context) ([]DetectedGPU, bool) {
	output, err := nvidiaSMI(ctx, "--query-gpu=index,name", "--format=csv,noheader")
	if err != nil {
		return nil, false
	}
	var gpus []DetectedGPU
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		index, name, found := strings.Cut(line, ",")
		value, err := strconv.Atoi(strings.TrimSpace(index))
		if !found || err != nil || value < 0 {
			continue
		}
		gpus = append(gpus, DetectedGPU{Index: value, Name: strings.TrimSpace(name)})
	}
	return gpus, true
}

func nvencSessions(name string) (int64, bool) {
	tokens := modelTokens(name)
	switch {
	case withoutNVENC(tokens):
		return 0, false
	case slices.Contains(tokens, "GEFORCE") || slices.Contains(tokens, "TITAN"):
		return consumerNVENCSessions, true
	default:
		return professionalNVENCSessions, false
	}
}

func withoutNVENC(tokens []string) bool {
	for _, model := range []string{"A100", "A30", "H100", "H200", "H800", "A800", "B100", "B200", "GB200", "GH200"} {
		if slices.Contains(tokens, model) {
			return true
		}
	}
	return false
}

func modelTokens(name string) []string {
	return strings.FieldsFunc(strings.ToUpper(name), func(character rune) bool {
		return !(character >= 'A' && character <= 'Z' || character >= '0' && character <= '9')
	})
}

func (c Config) validateGPUs() []string {
	var problems []string
	if gpulock.Supported && c.GPULockDirectory != "" && !filepath.IsAbs(c.GPULockDirectory) {
		problems = append(problems, "--gpu-lock-dir must be an absolute path, the same for every GPU Worker of the host")
	}
	if !c.GPUs.explicit() || c.GPUShare {
		return problems
	}
	if both := gpulock.Overlap(c.GPUs.Devices, gpulock.Range(c.SeatGPUs)); len(both) > 0 {
		problems = append(problems, fmt.Sprintf("--media-gpus %s overlaps the seat GPUs 0..%d (--gpu-count %d): give each GPU to one plane (--media-gpus on GPUs from %d on, or a lower --gpu-count), or set --media-gpu-share to share them through the GPU locks",
			gpulock.FormatDevices(both), c.SeatGPUs-1, c.SeatGPUs, c.SeatGPUs))
	}
	return problems
}

func ResolveGPU(ctx context.Context, config Config) (*GPUInfo, error) {
	gpu, _, err := CheckGPU(ctx, config)
	return gpu, err
}

func CheckGPU(ctx context.Context, config Config) (gpu *GPUInfo, warning string, err error) {
	detected, probed := DetectGPUs(ctx)
	gpu, warning, err = resolveGPU(config, detected, probed)
	if err != nil || gpu == nil || gpu.Devices == nil || !gpulock.Supported || config.GPULockDirectory == "" ||
		(config.SeatGPUs == 0 && !config.GPUShare) {
		return gpu, warning, err
	}
	if err := gpulock.Prepare(config.GPULockDirectory); err != nil {
		return nil, warning, fmt.Errorf("GPU lock directory %s (--gpu-lock-dir): %w", config.GPULockDirectory, err)
	}
	return gpu, warning, nil
}

func resolveGPU(config Config, detected []DetectedGPU, probed bool) (gpu *GPUInfo, note string, err error) {
	selection := config.GPUs
	if !selection.Auto && !selection.None && !selection.explicit() {
		selection = GPUSelection{Auto: true}
	}
	override := int64(config.GPUEncoderSessions)
	if selection.None || override == 0 {
		return nil, note, nil
	}
	seats := gpulock.Range(config.SeatGPUs)
	if probed && len(detected) == 0 {
		probed = false
	}
	if !probed {
		switch {
		case selection.explicit():
			return nil, "", fmt.Errorf("--media-gpus %s: nvidia-smi is unavailable, so the GPUs cannot be checked", gpulock.FormatDevices(selection.Devices))
		case override < 0:
			return nil, note, nil
		case len(seats) > 0:
			return nil, "", fmt.Errorf("--media-gpu-encoder-sessions %d on a Worker with seat GPUs (--gpu-count %d) needs nvidia-smi to keep the transcoder on its own GPUs", override, config.SeatGPUs)
		}
		return &GPUInfo{Vendor: "nvidia", Model: "declared", MaxEncoderSessions: override}, "", nil
	}
	byIndex := map[int]DetectedGPU{}
	for _, gpu := range detected {
		byIndex[gpu.Index] = gpu
	}
	var devices []EncoderGPU
	consumer := false
	add := func(gpu DetectedGPU, sessions int64, isConsumer bool) {
		devices = append(devices, EncoderGPU{Index: gpu.Index, Model: gpu.Name, Sessions: sessions})
		consumer = consumer || isConsumer
	}
	if selection.explicit() {
		for _, index := range selection.Devices {
			gpu, listed := byIndex[index]
			if !listed {
				return nil, "", fmt.Errorf("--media-gpus names GPU %d, which nvidia-smi does not list", index)
			}
			sessions, isConsumer := nvencSessions(gpu.Name)
			if sessions == 0 && override < 0 {
				return nil, "", fmt.Errorf("--media-gpus names GPU %d (%s), which has no NVENC encoder", index, gpu.Name)
			}
			add(gpu, sessions, isConsumer)
		}
	} else {
		skipped := 0
		for _, gpu := range detected {
			sessions, isConsumer := nvencSessions(gpu.Name)
			if sessions == 0 && override < 0 {
				continue
			}
			if slices.Contains(seats, gpu.Index) && !config.GPUShare {
				skipped++
				continue
			}
			add(gpu, sessions, isConsumer)
		}
		if len(devices) == 0 && skipped > 0 && config.Has(mediacontracts.RoleTranscode) {
			note = fmt.Sprintf("media.transcode gets no GPU: every NVENC GPU is a seat GPU (--gpu-count %d), and the transcoder keeps off seat GPUs by default, so it serves source and audio-only ladders only. To encode on a GPU, give it its own (--media-gpus with the last GPUs, which --gpu-count auto then leaves to it, or a lower --gpu-count) or share the seat GPUs through the GPU locks (--media-gpu-share); docs/worker.md, \"GPUs shared by media and compute\"", config.SeatGPUs)
		}
	}
	if len(devices) == 0 {
		return nil, note, nil
	}
	info := &GPUInfo{Vendor: "nvidia", Model: devices[0].Model, Devices: devices}
	if override > 0 {
		for i := range info.Devices {
			info.Devices[i].Sessions = override
		}
		info.MaxEncoderSessions = override
		return info, "", nil
	}
	for _, device := range info.Devices {
		if sessions, isConsumer := nvencSessions(device.Model); !isConsumer {
			info.MaxEncoderSessions += sessions
		}
	}
	if consumer {
		info.MaxEncoderSessions += consumerNVENCSessions
	}
	return info, "", nil
}

func EncoderSessionsInUse(ctx context.Context, devices []EncoderGPU) int64 {
	output, err := nvidiaSMI(ctx, "--query-gpu=index,encoder.stats.sessionCount", "--format=csv,noheader,nounits")
	if err != nil {
		return 0
	}
	var total int64
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		index, count, found := strings.Cut(line, ",")
		device, indexErr := strconv.Atoi(strings.TrimSpace(index))
		value, countErr := strconv.ParseInt(strings.TrimSpace(count), 10, 64)
		if !found || indexErr != nil || countErr != nil || value <= 0 {
			continue
		}
		if devices == nil || slices.ContainsFunc(devices, func(gpu EncoderGPU) bool { return gpu.Index == device }) {
			total += value
		}
	}
	return total
}

func nvidiaSMI(parent context.Context, arguments ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	command := exec.CommandContext(ctx, path, arguments...)
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (f *Fleet) prepareGPULocks() error {
	directory := f.config.GPULockDirectory
	if !gpulock.Supported {
		f.config.GPULockDirectory = ""
		return nil
	}
	if directory == "" || f.gpu.Devices == nil {
		return nil
	}
	err := gpulock.Prepare(directory)
	switch {
	case err == nil:
		return nil
	case f.config.SeatGPUs > 0 || f.config.GPUShare:
		return fmt.Errorf("GPU lock directory %s (--gpu-lock-dir): %w", directory, err)
	}
	log.Printf("warning: media.transcode: GPU lock directory %s (--gpu-lock-dir): %v; this Worker has no seat GPUs, so its transcoder encodes without the GPU locks", directory, err)
	f.config.GPULockDirectory = ""
	return nil
}

func (f *Fleet) EncoderGPUs() (devices []EncoderGPU, all bool) {
	if f == nil || f.gpu == nil {
		return nil, false
	}
	return append([]EncoderGPU(nil), f.gpu.Devices...), f.gpu.Devices == nil
}

func (f *Fleet) GPULockDirectory() string {
	if f == nil {
		return ""
	}
	return f.config.GPULockDirectory
}
