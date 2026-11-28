package transcode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/substructs"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
)

type audioEncoder struct {
	config    broadcast.TranscodeConfig
	rendition mc.AudioRendition
	input     tscodecs.Codec
	audio     *sourceAudio
	node      string

	queue   chan audioChunk
	dropped atomic.Int64
	fed     atomic.Int64
	done    chan struct{}
}

type audioChunk struct {
	at     int64
	frames [][]byte
}

const audioQueueChunks = 256

var errEncoderStopped = errors.New("encoder stopped")

func newAudioEncoder(ctx context.Context, config broadcast.TranscodeConfig, rendition mc.AudioRendition, source *mpegts.Track,
	audio *sourceAudio, node string) *audioEncoder {
	var input tscodecs.Codec
	switch codec := source.Codec.(type) {
	case *tscodecs.MPEG4Audio:
		input = &tscodecs.MPEG4Audio{Config: codec.Config}
	case *tscodecs.Opus:
		desc := codec.Desc
		if desc == nil {
			desc = &substructs.OpusAudioDescriptor{ChannelConfigCode: 2}
		}
		input = &tscodecs.Opus{Desc: desc}
	default:
		input = &tscodecs.MPEG1Audio{}
	}
	encoder := &audioEncoder{config: config, rendition: rendition, input: input, audio: audio, node: node,
		queue: make(chan audioChunk, audioQueueChunks), done: make(chan struct{})}
	go encoder.run(ctx)
	return encoder
}

func (e *audioEncoder) feed(at int64, frames [][]byte) {
	if len(frames) == 0 {
		return
	}
	select {
	case e.queue <- audioChunk{at: at, frames: copyFrames(frames)}:
		e.audio.handed(at)
	default:
		if dropped := e.dropped.Add(1); dropped == 1 || dropped%500 == 0 {
			log.Printf("media transcode %s: AAC encoder behind: %d source audio packets dropped", e.node, dropped)
		}
	}
}

func (e *audioEncoder) run(ctx context.Context) {
	defer close(e.done)
	backoff := time.Second
	for {
		started := time.Now()
		err := e.session(ctx)
		e.audio.setSource(false)
		if errors.Is(err, errEncoderStopped) || ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		log.Printf("media transcode %s: AAC encoder: %v; restarting in %s (the audio rendition carries silence meanwhile)", e.node, err, backoff)
		timer := time.NewTimer(backoff)
	wait:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case _, open := <-e.queue:
				if !open {
					timer.Stop()
					return
				}
				e.dropped.Add(1)
			case <-timer.C:
				break wait
			}
		}
		backoff = min(backoff*2, 15*time.Second)
	}
}

func (e *audioEncoder) session(ctx context.Context) error {
	rendition := e.rendition
	args := []string{"-hide_banner", "-nostats", "-loglevel", "error",
		"-fflags", "+discardcorrupt", "-probesize", "32768", "-analyzeduration", "500000",
		"-copyts", "-f", "mpegts", "-i", "pipe:0", "-map", "0:a:0",
		"-af", "aresample=" + strconv.FormatInt(rendition.SampleRate, 10) + ":async=1:min_hard_comp=0.100000",
		"-c:a", "aac", "-profile:a", "aac_low",
		"-b:a", strconv.FormatInt(rendition.BitrateBPS, 10),
		"-ac", strconv.FormatInt(rendition.Channels, 10),
		"-ar", strconv.FormatInt(rendition.SampleRate, 10),
		"-muxdelay", "0", "-muxpreload", "0", "-pes_payload_size", "0", "-flush_packets", "1",
		"-f", "mpegts", "pipe:1"}
	process, err := startFFmpeg(ctx, e.config, args, nil)
	if err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	track := &mpegts.Track{PID: 0x100, Codec: e.input}
	writer := &mpegts.Writer{W: process.stdin, Tracks: []*mpegts.Track{track}}
	readDone := make(chan error, 1)
	fail := func(err error) error {
		if stopErr := process.stop(); stopErr != nil {
			return fmt.Errorf("%w (%s)", err, lastLines(stopErr.Error(), 20))
		}
		return err
	}
	if err := writer.Initialize(); err != nil {
		return fail(err)
	}
	if _, err := writer.WriteTables(); err != nil {
		return fail(err)
	}
	go func() { readDone <- e.read(process.stdout) }()
	e.audio.setSource(true)
	for {
		select {
		case chunk, open := <-e.queue:
			if !open {
				_ = process.stdin.Close()
				select {
				case <-readDone:
				case <-time.After(5 * time.Second):
				}
				if err := process.stop(); err != nil {
					log.Printf("media transcode %s: AAC encoder: %v", e.node, err)
				}
				return errEncoderStopped
			}
			var err error
			switch e.input.(type) {
			case *tscodecs.Opus:
				err = writer.WriteOpus(track, chunk.at+audioFeedBias, chunk.frames)
			case *tscodecs.MPEG4Audio:
				err = writer.WriteMPEG4Audio(track, chunk.at+audioFeedBias, chunk.frames)
			default:
				err = writer.WriteMPEG1Audio(track, chunk.at+audioFeedBias, chunk.frames)
			}
			e.fed.Store(chunk.at)
			if err != nil {
				return fail(fmt.Errorf("feed: %w", err))
			}
		case err := <-readDone:
			if err == nil || errors.Is(err, io.EOF) {
				err = errors.New("exited")
			}
			return fail(err)
		case <-ctx.Done():
			return fail(ctx.Err())
		}
	}
}

func (e *audioEncoder) read(stdout io.Reader) error {
	rendition := e.rendition
	reader := &mpegts.Reader{R: bufio.NewReaderSize(stdout, 64<<10)}
	if err := reader.Initialize(); err != nil {
		_, _ = io.Copy(io.Discard, stdout)
		return fmt.Errorf("encoder output: %w", err)
	}
	reader.OnDecodeError(func(error) {})
	matched := false
	for _, track := range reader.Tracks() {
		codec, ok := track.Codec.(*tscodecs.MPEG4Audio)
		if !ok {
			continue
		}
		if codec.Config.Type != mpeg4audio.ObjectTypeAACLC || int64(codec.Config.SampleRate) != rendition.SampleRate ||
			int64(codec.Config.ChannelConfig) != rendition.Channels {
			_, _ = io.Copy(io.Discard, stdout)
			return fmt.Errorf("encoder output %v does not match the audio rendition", codec.Config)
		}
		matched = true
		reader.OnDataMPEG4Audio(track, func(pts int64, aus [][]byte) error {
			at := unwrapNear(pts, e.fed.Load()+audioFeedBias) - audioFeedBias
			for i, au := range aus {
				e.audio.write(at+int64(i)*aacFrameSamples*90000/rendition.SampleRate, au)
			}
			return nil
		})
		break
	}
	if !matched {
		_, _ = io.Copy(io.Discard, stdout)
		return errors.New("encoder output has no AAC track")
	}
	for {
		if err := reader.Read(); err != nil {
			return err
		}
	}
}

func (e *audioEncoder) stop() {
	close(e.queue)
	<-e.done
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
