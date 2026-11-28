package transcode

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
)

type inputPlan struct {
	format      string
	hasVideo    bool
	audioInputs int
}

type encoderPlan struct {
	gpu      []bool
	sessions int
}

func planEncoders(ladder *mc.TranscodeLadder, config broadcast.TranscodeConfig, gpu bool) encoderPlan {
	plan := encoderPlan{gpu: make([]bool, len(ladder.Video))}
	for i, rendition := range ladder.Video {
		useGPU := gpu && rendition.Height > config.CPUMaxHeight
		if rendition.Codec != "h264" && gpu {
			useGPU = true
		}
		plan.gpu[i] = useGPU
		if useGPU {
			plan.sessions++
		}
	}
	return plan
}

func kvazaar(rendition mc.VideoRendition, gpu bool) bool { return !gpu && rendition.Codec == "h265" }

func pidOf(index int) uint16 { return uint16(0x100 + index) }

func ffmpegArgs(ladder *mc.TranscodeLadder, input inputPlan, plan encoderPlan) []string {
	args := []string{"-hide_banner", "-nostats", "-loglevel", "error",
		"-fflags", "+genpts+discardcorrupt", "-analyzeduration", "3000000", "-probesize", "5000000",
		"-f", input.format, "-i", "pipe:0"}
	nextInput := 1
	videoSource := "[0:v:0]"
	maxWidth, maxHeight := int64(1280), int64(720)
	for _, rendition := range ladder.Video {
		maxWidth, maxHeight = max(maxWidth, rendition.Width), max(maxHeight, rendition.Height)
	}
	if !input.hasVideo && len(ladder.Video) > 0 {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%dx%d:r=30", maxWidth, maxHeight))
		videoSource = fmt.Sprintf("[%d:v:0]", nextInput)
		nextInput++
	}
	audioSource := "[0:a:0]"
	if input.audioInputs == 0 && len(ladder.Audio) > 0 {
		args = append(args, "-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000")
		audioSource = fmt.Sprintf("[%d:a:0]", nextInput)
	}
	var graph []string
	if len(ladder.Video) > 0 {
		labels := make([]string, len(ladder.Video))
		for i := range labels {
			labels[i] = fmt.Sprintf("[vs%d]", i)
		}
		graph = append(graph, fmt.Sprintf("%ssplit=%d%s", videoSource, len(labels), strings.Join(labels, "")))
		for i, rendition := range ladder.Video {
			constantRate := ""
			if kvazaar(rendition, plan.gpu[i]) {
				constantRate = fmt.Sprintf("fps=%d,", rendition.FPSMax)
			}
			graph = append(graph, fmt.Sprintf("[vs%d]%sscale=w=%d:h=%d:force_original_aspect_ratio=decrease:force_divisible_by=2,"+
				"pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,format=yuv420p[v%d]",
				i, constantRate, rendition.Width, rendition.Height, rendition.Width, rendition.Height, i))
		}
	}
	if len(ladder.Audio) > 0 {
		source := audioSource
		if input.audioInputs > 1 {
			var inputs strings.Builder
			for i := range input.audioInputs {
				fmt.Fprintf(&inputs, "[0:a:%d]", i)
			}
			graph = append(graph, fmt.Sprintf("%samix=inputs=%d:normalize=0:dropout_transition=0[amixed]", inputs.String(), input.audioInputs))
			source = "[amixed]"
		}
		labels := make([]string, len(ladder.Audio))
		for i := range labels {
			labels[i] = fmt.Sprintf("[a%d]", i)
		}
		graph = append(graph, fmt.Sprintf("%saresample=async=1000:first_pts=0,asplit=%d%s", source, len(labels), strings.Join(labels, "")))
	}
	args = append(args, "-filter_complex", strings.Join(graph, ";"))
	gopSeconds := strconv.FormatFloat(float64(ladder.GOPMS)/1000, 'f', -1, 64)
	index := 0
	for i, rendition := range ladder.Video {
		stream := strconv.Itoa(i)
		gopFrames := strconv.FormatInt(max(1, rendition.FPSMax*ladder.GOPMS/1000), 10)
		bitrate := strconv.FormatInt(rendition.BitrateBPS, 10)
		args = append(args, "-map", fmt.Sprintf("[v%d]", i))
		switch {
		case plan.gpu[i] && rendition.Codec == "h265":
			args = append(args, "-c:v:"+stream, "hevc_nvenc", "-profile:v:"+stream, "main")
		case plan.gpu[i] && rendition.Codec == "av1":
			args = append(args, "-c:v:"+stream, "av1_nvenc")
		case plan.gpu[i]:
			args = append(args, "-c:v:"+stream, "h264_nvenc", "-profile:v:"+stream, "high")
		case kvazaar(rendition, plan.gpu[i]):
			args = append(args, "-c:v:"+stream, "libkvazaar",
				"-kvazaar-params:v:"+stream, "period="+gopFrames+",gop=0,open-gop=0")
		default:
			args = append(args, "-c:v:"+stream, "libopenh264", "-profile:v:"+stream, "constrained_baseline",
				"-allow_skip_frames:v:"+stream, "0", "-rc_mode:v:"+stream, "bitrate")
		}
		if plan.gpu[i] {
			args = append(args, "-preset:v:"+stream, "p4", "-tune:v:"+stream, "ll", "-rc:v:"+stream, "cbr",
				"-zerolatency:v:"+stream, "1", "-forced-idr:v:"+stream, "1", "-no-scenecut:v:"+stream, "1")
		}
		args = append(args, "-b:v:"+stream, bitrate, "-maxrate:v:"+stream, bitrate,
			"-bufsize:v:"+stream, strconv.FormatInt(rendition.BitrateBPS*2, 10),
			"-bf:v:"+stream, "0", "-g:v:"+stream, gopFrames, "-keyint_min:v:"+stream, gopFrames,
			"-fpsmax:v:"+stream, strconv.FormatInt(rendition.FPSMax, 10),
			"-force_key_frames:v:"+stream, "expr:gte(t,n_forced*"+gopSeconds+")",
			"-streamid", fmt.Sprintf("%d:%d", index, pidOf(index)))
		index++
	}
	for i, rendition := range ladder.Audio {
		stream := strconv.Itoa(i)
		codec := "aac"
		if rendition.Codec == "opus" {
			codec = "libopus"
		}
		args = append(args, "-map", fmt.Sprintf("[a%d]", i), "-c:a:"+stream, codec,
			"-b:a:"+stream, strconv.FormatInt(rendition.BitrateBPS, 10),
			"-ac:a:"+stream, strconv.FormatInt(rendition.Channels, 10),
			"-ar:a:"+stream, strconv.FormatInt(rendition.SampleRate, 10),
			"-streamid", fmt.Sprintf("%d:%d", index, pidOf(index)))
		index++
	}
	args = append(args, "-muxdelay", "0", "-muxpreload", "0", "-flush_packets", "1", "-f", "mpegts", "pipe:1")
	return args
}

type process struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    *tailBuffer
	remove    func() error
	removeErr error
}

var runs atomic.Uint64

const containerRemoveTimeout = 30 * time.Second

func startFFmpeg(ctx context.Context, config broadcast.TranscodeConfig, args []string, lease *encoderLease) (*process, error) {
	device := ""
	if lease != nil {
		device = lease.device
	}
	var cmd *exec.Cmd
	var remove func() error
	switch config.Runtime {
	case "local":
		cmd = exec.CommandContext(ctx, config.FFmpeg, args...)
	case "oci", "":
		name := fmt.Sprintf("media-transcode-%d-%d", time.Now().UnixNano(), runs.Add(1))
		ociArgs := []string{"run", "--rm", "-i", "--name", name, "--network", "none", "--read-only",
			"--tmpfs", "/tmp:rw,size=64m", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
			"--pids-limit", "256", "--memory", config.MemoryLimit, "--user", "65534:65534"}
		if config.CPUs != "" {
			ociArgs = append(ociArgs, "--cpus", config.CPUs)
		}
		ociArgs = append(ociArgs, containerArgs(config, device)...)
		ociArgs = append(ociArgs, config.Image)
		ociArgs = append(ociArgs, args...)
		cmd = exec.CommandContext(ctx, config.OCIBinary, ociArgs...)
		binary := config.OCIBinary
		remove = func() error { return removeContainer(binary, name) }
	default:
		return nil, fmt.Errorf("unknown transcoder runtime %q", config.Runtime)
	}
	if file := lease.lockFile(); file != nil {
		cmd.ExtraFiles = []*os.File{file}
	}
	if device != "" {
		tieEncoderCLI(cmd)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &tailBuffer{limit: 8 << 10}
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &process{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, remove: remove}, nil
}

func removeContainer(binary, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), containerRemoveTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "rm", "-f", name).CombinedOutput()
	if err == nil || strings.Contains(strings.ToLower(string(output)), "no such container") {
		return nil
	}
	return fmt.Errorf("remove %s: %w: %s", name, err, strings.TrimSpace(string(output)))
}

func (p *process) stop() error {
	_ = p.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		err = <-done
	}
	if p.remove != nil {
		p.removeErr = p.remove()
	}
	if err != nil && p.stderr.String() != "" {
		return fmt.Errorf("%w: %s", err, p.stderr.String())
	}
	return err
}

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = b.data[len(b.data)-b.limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}
