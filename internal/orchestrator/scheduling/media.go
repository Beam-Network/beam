package scheduling

import (
	"fmt"
	"slices"
	"sort"
	"time"

	orchestratordomain "github.com/Beam-Network/beam/internal/orchestrator/domain"
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const DefaultMediaHeadroomRatio = 0.8

const DefaultMediaMaxCPURatio = 0.9

type MediaLoad struct {
	Nodes           map[string]int
	EgressMbps      int64
	IngressMbps     int64
	EncoderSessions int64
}

type MediaRequest struct {
	Role            string
	Region          string
	EgressMbps      int64
	IngressMbps     int64
	EncoderSessions int64
	MaxNodes        int
	Placed          map[string]MediaLoad
	HeadroomRatio   float64
	MaxCPURatio     float64
}

type MediaHeadroom struct {
	EgressMbps      int64
	IngressMbps     int64
	EncoderSessions int64
	SpareEncoders   int64
}

func (s *Scheduler) SelectMedia(request Request, media MediaRequest, now time.Time) (Placement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.MaxObservationAge <= 0 {
		request.MaxObservationAge = 30 * time.Second
	}
	if media.HeadroomRatio <= 0 || media.HeadroomRatio > 1 {
		media.HeadroomRatio = DefaultMediaHeadroomRatio
	}
	if media.MaxCPURatio <= 0 || media.MaxCPURatio > 1 {
		media.MaxCPURatio = DefaultMediaMaxCPURatio
	}
	type candidate struct {
		observation orchestratordomain.WorkerObservation
		headroom    MediaHeadroom
		inRegion    bool
		score       int64
	}
	var candidates []candidate
	placement := Placement{}
	for _, observation := range s.registry.Observations() {
		observation.Available = observation.Available.SubFloor(s.unreported(observation, now))
		reason := rejectionReason(observation, request, now)
		var headroom MediaHeadroom
		if reason == "" {
			headroom, reason = mediaRejection(observation, media)
		}
		if reason != "" {
			placement.Rejected = append(placement.Rejected, Rejection{WorkerID: observation.WorkerID, Reason: reason})
			continue
		}
		inRegion := media.Region != "" && observation.Media.RegionClaim != nil && *observation.Media.RegionClaim == media.Region
		candidates = append(candidates, candidate{observation: observation, headroom: headroom, inRegion: inRegion,
			score: MediaScore(media.Role, headroom)})
	}
	if len(candidates) == 0 {
		return placement, ErrNoCandidate
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.inRegion != right.inRegion {
			return left.inRegion
		}
		if left.score != right.score {
			return left.score > right.score
		}
		return left.observation.WorkerID < right.observation.WorkerID
	})
	selected := candidates[0]
	placement.WorkerID = selected.observation.WorkerID
	placement.NodeID = selected.observation.NodeID
	placement.Score = selected.score
	return placement, nil
}

func MediaScore(role string, headroom MediaHeadroom) int64 {
	switch role {
	case mediacontracts.RoleTranscode:
		return (headroom.EncoderSessions-headroom.SpareEncoders)*1_000_000 + headroom.EgressMbps
	case mediacontracts.RoleIngest:
		return headroom.IngressMbps*1_000 + headroom.EgressMbps
	default:
		return headroom.EgressMbps
	}
}

func mediaRejection(observation orchestratordomain.WorkerObservation, media MediaRequest) (MediaHeadroom, string) {
	capacity := observation.Media
	if capacity == nil {
		return MediaHeadroom{}, "no media capacity report"
	}
	workerRole := mediacontracts.WorkerRoles[media.Role]
	if workerRole == "" {
		return MediaHeadroom{}, fmt.Sprintf("unknown media role %s", media.Role)
	}
	if !slices.Contains(capacity.Roles, workerRole) {
		return MediaHeadroom{}, fmt.Sprintf("media role %s not enabled", workerRole)
	}
	placed := media.Placed[observation.WorkerID]
	if media.MaxNodes > 0 && placed.Nodes[media.Role] >= media.MaxNodes {
		return MediaHeadroom{}, fmt.Sprintf("media role %s at its limit of %d nodes", media.Role, media.MaxNodes)
	}
	if capacity.Load.CPURatio > media.MaxCPURatio {
		return MediaHeadroom{}, "media CPU saturated"
	}
	headroom := MediaHeadroom{
		EgressMbps:  int64(float64(EffectiveMbps(capacity.Egress))*media.HeadroomRatio) - max(capacity.Load.EgressMbps, placed.EgressMbps),
		IngressMbps: int64(float64(EffectiveMbps(capacity.Ingress))*media.HeadroomRatio) - max(capacity.Load.IngressMbps, placed.IngressMbps),
	}
	if headroom.EgressMbps < media.EgressMbps {
		return MediaHeadroom{}, "insufficient media egress headroom"
	}
	if headroom.IngressMbps < media.IngressMbps {
		return MediaHeadroom{}, "insufficient media ingress headroom"
	}
	if media.EncoderSessions > 0 {
		if capacity.GPU == nil {
			return MediaHeadroom{}, "no NVENC encoder"
		}
		headroom.EncoderSessions = capacity.GPU.MaxEncoderSessions - max(capacity.Load.EncoderSessions, placed.EncoderSessions)
		if headroom.EncoderSessions < media.EncoderSessions {
			return MediaHeadroom{}, "insufficient NVENC sessions"
		}
	} else if media.Role == mediacontracts.RoleTranscode && capacity.GPU != nil {
		headroom.SpareEncoders = max(capacity.GPU.MaxEncoderSessions-max(capacity.Load.EncoderSessions, placed.EncoderSessions), 0)
	}
	if (media.Role == mediacontracts.RoleShield || media.Role == mediacontracts.RoleEdge) &&
		(capacity.Cache == nil || capacity.Cache.DiskBytes <= 0) {
		return MediaHeadroom{}, "no media cache"
	}
	if (media.Role == mediacontracts.RoleSFU || media.Role == mediacontracts.RoleRelay) && capacity.UDPPortRange == nil {
		return MediaHeadroom{}, "no media UDP port range"
	}
	if media.Role != mediacontracts.RoleTranscode && media.Role != mediacontracts.RolePackage &&
		capacity.PublicIPv4 == nil && capacity.PublicIPv6 == nil {
		return MediaHeadroom{}, "no public IP"
	}
	return headroom, ""
}

func EffectiveMbps(bandwidth mediacontracts.Bandwidth) int64 {
	if bandwidth.MeasuredMbps == nil || *bandwidth.MeasuredMbps <= 0 {
		return bandwidth.DeclaredMbps
	}
	if bandwidth.DeclaredMbps > 0 {
		return min(*bandwidth.MeasuredMbps, bandwidth.DeclaredMbps)
	}
	return *bandwidth.MeasuredMbps
}
