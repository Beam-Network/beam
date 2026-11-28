package packager

import (
	"fmt"
	"math"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"

	mc "github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/broadcast"
)

func independentSegments(ladder *mc.TranscodeLadder) bool { return !mc.IsSourceLadder(ladder) }

func fallbackCodec(codec string) string {
	switch codec {
	case "h264":
		return "avc1.64002a"
	case "h265":
		return "hvc1.1.6.L123.B0"
	case "opus":
		return "opus"
	default:
		return "mp4a.40.2"
	}
}

func (r *rendition) codecString(fallback string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.codec != "" {
		return r.codec
	}
	return fallbackCodec(fallback)
}

func (r *rendition) sourceFormat() (width, height int, fps float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.width, r.height, r.fps
}

func buildMaster(ladder *mc.TranscodeLadder, renditions map[string]*rendition) ([]byte, error) {
	master := &playlist.Multivariant{Version: 9, IndependentSegments: independentSegments(ladder)}
	var audioCodec string
	var audioBitrate int64
	for i, audio := range ladder.Audio {
		uri := audio.RenditionID + "/" + broadcast.PlaylistName
		channels := fmt.Sprintf("%d", audio.Channels)
		master.Renditions = append(master.Renditions, &playlist.MultivariantRendition{Type: playlist.MultivariantRenditionTypeAudio,
			GroupID: "audio", Name: audio.RenditionID, Default: i == 0, Autoselect: true, Channels: &channels, URI: &uri})
		if i == 0 {
			audioBitrate = audio.BitrateBPS
			if rend := renditions[audio.RenditionID]; rend != nil {
				audioCodec = rend.codecString(audio.Codec)
			}
		}
	}
	for _, video := range ladder.Video {
		rend := renditions[video.RenditionID]
		if rend == nil {
			continue
		}
		codecs := []string{rend.codecString(video.Codec)}
		if audioCodec != "" {
			codecs = append(codecs, audioCodec)
		}
		bandwidth := int(float64(video.BitrateBPS+audioBitrate) * 1.1)
		average := int(video.BitrateBPS + audioBitrate)
		fps := float64(video.FPSMax)
		variant := &playlist.MultivariantVariant{Bandwidth: bandwidth, AverageBandwidth: &average, Codecs: codecs,
			Resolution: fmt.Sprintf("%dx%d", video.Width, video.Height), FrameRate: &fps,
			URI: video.RenditionID + "/" + broadcast.PlaylistName}
		if video.Passthrough {
			variant.Bandwidth, variant.AverageBandwidth = int(video.BitrateBPS), nil
			variant.Resolution, variant.FrameRate = "", nil
			if width, height, sourceFPS := rend.sourceFormat(); width > 0 && height > 0 {
				variant.Resolution = fmt.Sprintf("%dx%d", width, height)
				if sourceFPS > 0 && sourceFPS <= 240 {
					rounded := math.Round(sourceFPS*1000) / 1000
					variant.FrameRate = &rounded
				}
			}
		}
		if len(ladder.Audio) > 0 {
			variant.Audio = "audio"
		}
		master.Variants = append(master.Variants, variant)
	}
	if len(ladder.Video) == 0 {
		master.Renditions = nil
		for _, audio := range ladder.Audio {
			rend := renditions[audio.RenditionID]
			if rend == nil {
				continue
			}
			master.Variants = append(master.Variants, &playlist.MultivariantVariant{Bandwidth: int(float64(audio.BitrateBPS) * 1.1),
				Codecs: []string{rend.codecString(audio.Codec)}, URI: audio.RenditionID + "/" + broadcast.PlaylistName})
		}
	}
	return master.Marshal()
}
