package contracts

import (
	"errors"
	"net"
	"regexp"
	"slices"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

var MediaBroadRegions = []string{"wnam", "enam", "sam", "weur", "eeur", "apac", "oc", "afr", "me"}

var MediaWorkerRoles = []string{"media.sfu", "media.relay", "media.ingest", "media.transcode", "media.package", "media.shield", "media.edge"}

const MediaWorkerFeaturesMax = 16

var mediaWorkerFeaturePattern = regexp.MustCompile(`^[a-z0-9_.]{1,64}$`)

const MediaCapacityReportIntervalMS = 30_000

const MediaCapacityStaleMS = 90_000

func NodeRoleOfWorkerRole(workerRole string) string {
	for role, candidate := range mediacontracts.WorkerRoles {
		if candidate == workerRole {
			return role
		}
	}
	return ""
}

func ValidateWorkerMediaCapacity(value mediacontracts.WorkerCapacity) error {
	if !printableText(value.WorkerID, 256) {
		return errors.New("media capacity worker_id is invalid")
	}
	if len(value.Roles) > len(MediaWorkerRoles) {
		return errors.New("media capacity lists too many roles")
	}
	seen := map[string]bool{}
	for _, role := range value.Roles {
		if !slices.Contains(MediaWorkerRoles, role) || seen[role] {
			return errors.New("media capacity role is unknown or repeated")
		}
		seen[role] = true
	}
	if value.RegionClaim != nil && !slices.Contains(MediaBroadRegions, *value.RegionClaim) {
		return errors.New("media capacity region claim is not a broad region")
	}
	if value.PublicIPv4 != nil {
		if ip := net.ParseIP(*value.PublicIPv4); ip == nil || ip.To4() == nil || *value.PublicIPv4 != ip.String() {
			return errors.New("media capacity public_ipv4 is invalid")
		}
	}
	if value.PublicIPv6 != nil {
		if ip := net.ParseIP(*value.PublicIPv6); ip == nil || ip.To4() != nil {
			return errors.New("media capacity public_ipv6 is invalid")
		}
	}
	port := func(value int64) bool { return value >= 1 && value <= 65_535 }
	optionalPort := func(value *int64) bool { return value == nil || port(*value) }
	if value.UDPPortRange != nil && (!port(value.UDPPortRange.Min) || !port(value.UDPPortRange.Max) || value.UDPPortRange.Min > value.UDPPortRange.Max) {
		return errors.New("media capacity UDP port range is invalid")
	}
	if !port(value.Ports.HTTPS) || !optionalPort(value.Ports.RTMP) || !optionalPort(value.Ports.RTMPS) || !optionalPort(value.Ports.SRT) {
		return errors.New("media capacity ports are invalid")
	}
	for _, bandwidth := range []mediacontracts.Bandwidth{value.Egress, value.Ingress} {
		if bandwidth.DeclaredMbps < 0 || (bandwidth.MeasuredMbps != nil && *bandwidth.MeasuredMbps < 0) ||
			(bandwidth.MeasuredAtMS != nil && *bandwidth.MeasuredAtMS < 0) || (bandwidth.MeasuredMbps == nil) != (bandwidth.MeasuredAtMS == nil) {
			return errors.New("media capacity bandwidth is invalid")
		}
	}
	if value.GPU != nil && (len(value.GPU.Vendor) > 64 || len(value.GPU.Model) > 128 || value.GPU.Encoder != "nvenc" || value.GPU.MaxEncoderSessions < 0) {
		return errors.New("media capacity GPU is invalid")
	}
	if value.Cache != nil && (value.Cache.DiskBytes < 0 || value.Cache.MemoryBytes < 0) {
		return errors.New("media capacity cache is invalid")
	}
	load := value.Load
	if load.CPURatio < 0 || load.CPURatio > 1 || load.EgressMbps < 0 || load.IngressMbps < 0 || load.Sessions < 0 ||
		load.Subscribers < 0 || load.EncoderSessions < 0 || load.CacheUsedBytes < 0 {
		return errors.New("media capacity load is invalid")
	}
	if value.EdgeHostname != nil && !mediacontracts.ValidHostname(*value.EdgeHostname) {
		return errors.New("media capacity edge hostname is invalid")
	}
	if len(value.Features) > MediaWorkerFeaturesMax {
		return errors.New("media capacity lists too many features")
	}
	features := map[string]bool{}
	for _, feature := range value.Features {
		if !mediaWorkerFeaturePattern.MatchString(feature) || features[feature] {
			return errors.New("media capacity feature is invalid or repeated")
		}
		features[feature] = true
	}
	return nil
}
