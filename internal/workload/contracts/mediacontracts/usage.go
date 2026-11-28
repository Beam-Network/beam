package mediacontracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	ClassAudio = "audio"
	ClassSD    = "sd"
	ClassHD    = "hd"
	ClassFHD   = "fhd"
	ClassUHD   = "uhd"
)

var MediaClasses = []string{ClassAudio, ClassSD, ClassHD, ClassFHD, ClassUHD}

const hourMS = 3_600_000

func VideoClass(width, height int64) string {
	side := width
	if height < side {
		side = height
	}
	switch {
	case side >= 1440:
		return ClassUHD
	case side >= 1080:
		return ClassFHD
	case side >= 720:
		return ClassHD
	default:
		return ClassSD
	}
}

func classRank(class string) int {
	for index, name := range MediaClasses {
		if name == class {
			return index
		}
	}
	return -1
}

func MostDemandingClass(classes []string) string {
	best := ""
	for _, class := range classes {
		if classRank(class) > classRank(best) {
			best = class
		}
	}
	return best
}

type VideoSize struct {
	Width  int64 `json:"width"`
	Height int64 `json:"height"`
}

func SecondClass(audio bool, video []VideoSize) string {
	classes := make([]string, 0, len(video)+1)
	if audio {
		classes = append(classes, ClassAudio)
	}
	for _, track := range video {
		classes = append(classes, VideoClass(track.Width, track.Height))
	}
	return MostDemandingClass(classes)
}

func RenditionClass(ladder *TranscodeLadder, renditionID string, resolution *VideoSize) string {
	if ladder == nil {
		return ""
	}
	for _, rendition := range ladder.Audio {
		if rendition.RenditionID == renditionID {
			return ClassAudio
		}
	}
	for _, rendition := range ladder.Video {
		if rendition.RenditionID == renditionID {
			if resolution != nil {
				return VideoClass(resolution.Width, resolution.Height)
			}
			return VideoClass(rendition.Width, rendition.Height)
		}
	}
	return ""
}

func RTPPacketBytes(packet []byte, stream string, repeat bool) (mediaBytes, overheadBytes int64, ok bool) {
	if len(packet) < 12 || packet[0]>>6 != 2 {
		return 0, 0, false
	}
	header := 12 + 4*int(packet[0]&0x0f)
	if packet[0]&0x10 != 0 {
		if len(packet) < header+4 {
			return 0, 0, false
		}
		header += 4 + 4*(int(packet[header+2])<<8|int(packet[header+3]))
	}
	if len(packet) < header {
		return 0, 0, false
	}
	padding := 0
	if packet[0]&0x20 != 0 {
		padding = int(packet[len(packet)-1])
		if padding == 0 || padding > len(packet)-header {
			return 0, 0, false
		}
	}
	original := 0
	if stream == "rtx_recovered" {
		original = 2
	}
	if (stream != "media" && stream != "rtx_recovered") || repeat || len(packet)-header-padding <= original {
		return 0, int64(len(packet)), true
	}
	return int64(len(packet) - padding - original), int64(padding + original), true
}

type UsageInterval [2]int64

func SplitAtHours(interval UsageInterval) []UsageInterval {
	parts := []UsageInterval{}
	for start := interval[0]; start < interval[1]; {
		end := (start/hourMS + 1) * hourMS
		if end > interval[1] {
			end = interval[1]
		}
		parts = append(parts, UsageInterval{start, end})
		start = end
	}
	return parts
}

func UnionMS(intervals []UsageInterval) int64 {
	sorted := append([]UsageInterval{}, intervals...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i][0] < sorted[j][0] })
	var total, reached int64
	for _, interval := range sorted {
		if interval[1] <= reached {
			continue
		}
		start := interval[0]
		if reached > start {
			start = reached
		}
		total += interval[1] - start
		reached = interval[1]
	}
	return total
}

func RTCConnectedIntervals(ice, sent []UsageInterval) []UsageInterval {
	merged := []UsageInterval{}
	if len(ice) == 0 {
		return merged
	}
	first := ice[0][0]
	for _, interval := range ice {
		if interval[0] < first {
			first = interval[0]
		}
	}
	all := append(append([]UsageInterval{}, ice...), sent...)
	for index := range all {
		if all[index][0] < first {
			all[index][0] = first
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i][0] < all[j][0] })
	for _, interval := range all {
		if interval[1] <= interval[0] {
			continue
		}
		if last := len(merged) - 1; last >= 0 && interval[0] < merged[last][1]+UsageOutageMinMS {
			if interval[1] > merged[last][1] {
				merged[last][1] = interval[1]
			}
			continue
		}
		merged = append(merged, interval)
	}
	return merged
}

type HLSRequest struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
	Opens   bool  `json:"opens,omitempty"`
}

func HLSPlayerIntervals(requests []HLSRequest, targetMS int64, cuts []int64) [][]UsageInterval {
	sorted := append([]HLSRequest{}, requests...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].StartMS < sorted[j].StartMS })
	cutsSorted := append([]int64{}, cuts...)
	sort.Slice(cutsSorted, func(i, j int) bool { return cutsSorted[i] < cutsSorted[j] })
	connections := [][]UsageInterval{}
	var pieces []UsageInterval
	var reached int64
	finish := func(atMost int64) {
		if len(pieces) > 0 {
			end := reached + targetMS
			if atMost < end {
				end = atMost
			}
			pieces[len(pieces)-1][1] = end
			connections = append(connections, pieces)
		}
	}
	for _, request := range sorted {
		if len(pieces) > 0 && request.StartMS <= reached+3*targetMS && !request.Opens {
			known := reached + targetMS
			if request.StartMS > known {
				for _, cut := range cutsSorted {
					if cut > known && cut <= request.StartMS {
						pieces[len(pieces)-1][1] = known
						pieces = append(pieces, UsageInterval{request.StartMS, request.EndMS})
						break
					}
				}
			}
		} else {
			finish(request.StartMS)
			pieces = []UsageInterval{{request.StartMS, request.EndMS}}
			if reached <= request.StartMS {
				reached = request.EndMS
			}
		}
		if request.EndMS > reached {
			reached = request.EndMS
		}
	}
	finish(math.MaxInt64)
	for index, connection := range connections {
		parts := []UsageInterval{}
		for _, interval := range connection {
			from := interval[0]
			for _, cut := range cutsSorted {
				if cut <= from || cut >= interval[1] {
					continue
				}
				parts = append(parts, UsageInterval{from, cut})
				from = cut
			}
			parts = append(parts, UsageInterval{from, interval[1]})
		}
		connections[index] = parts
	}
	return connections
}

type ClassAmounts struct {
	Audio int64 `json:"audio"`
	SD    int64 `json:"sd"`
	HD    int64 `json:"hd"`
	FHD   int64 `json:"fhd"`
	UHD   int64 `json:"uhd"`
}

func (amounts *ClassAmounts) Add(class string, amount int64) {
	switch class {
	case ClassAudio:
		amounts.Audio += amount
	case ClassSD:
		amounts.SD += amount
	case ClassHD:
		amounts.HD += amount
	case ClassFHD:
		amounts.FHD += amount
	case ClassUHD:
		amounts.UHD += amount
	}
}

func (amounts ClassAmounts) Sum() int64 {
	return amounts.Audio + amounts.SD + amounts.HD + amounts.FHD + amounts.UHD
}

type HLSObject struct {
	DoneMS int64  `json:"done_ms"`
	Class  string `json:"class"`
}

func HLSClassMS(intervals []UsageInterval, objects []HLSObject, targetMS int64) ClassAmounts {
	var amounts ClassAmounts
	for _, interval := range intervals {
		for second := interval[0] / 1000 * 1000; second < interval[1]; second += 1000 {
			start, end := second, second+1000
			if interval[0] > start {
				start = interval[0]
			}
			if interval[1] < end {
				end = interval[1]
			}
			inside, latest := "", ""
			latestAt := int64(-1)
			audio := false
			for _, object := range objects {
				if object.DoneMS < second-targetMS || object.DoneMS >= second+1000 {
					continue
				}
				if object.Class == ClassAudio {
					audio = true
					continue
				}
				if object.DoneMS >= second {
					inside = MostDemandingClass([]string{inside, object.Class})
				}
				switch {
				case object.DoneMS > latestAt:
					latest, latestAt = object.Class, object.DoneMS
				case object.DoneMS == latestAt:
					latest = MostDemandingClass([]string{latest, object.Class})
				}
			}
			switch {
			case inside != "":
				amounts.Add(inside, end-start)
			case latest != "":
				amounts.Add(latest, end-start)
			case audio:
				amounts.Add(ClassAudio, end-start)
			}
		}
	}
	return amounts
}

const (
	AnonymousConnectionID = "anonymous"
	UsageIntervalsMax     = 8
	UsageOutageMinMS      = 1_000
	UsageRenditionsMax    = 12
	UsageRowsMax          = 10_000
	ProofMaxLeadMS        = 120_000
	SealedTokenMaxLength  = 5_526
)

const (
	ProofGrant       = "grant"
	ProofSealedToken = "sealed_token"
)

type UsageProof struct {
	Type       string  `json:"type"`
	VerifiedMS int64   `json:"verified_ms"`
	Grant      *string `json:"grant,omitempty"`
	KeyID      *string `json:"key_id,omitempty"`
	Sealed     *string `json:"sealed,omitempty"`
}

type UsageReceipt struct {
	Value      string `json:"value"`
	ReceivedMS int64  `json:"received_ms"`
}

type UsageConnectionV2 struct {
	ConnectionID  string           `json:"connection_id"`
	TokenNonce    *string          `json:"token_nonce"`
	Identity      *string          `json:"identity"`
	Kind          *string          `json:"kind"`
	Direction     string           `json:"direction"`
	Transport     string           `json:"transport"`
	StartMS       int64            `json:"start_ms"`
	EndMS         int64            `json:"end_ms"`
	Intervals     *[]UsageInterval `json:"intervals"`
	Ended         bool             `json:"ended"`
	ConnectedMS   int64            `json:"connected_ms"`
	MediaMS       int64            `json:"media_ms"`
	MediaBytes    int64            `json:"media_bytes"`
	OverheadBytes int64            `json:"overhead_bytes"`
	ClassMS       *ClassAmounts    `json:"class_ms"`
	ClassBytes    *ClassAmounts    `json:"class_bytes"`
	Receipt       *UsageReceipt    `json:"receipt"`
	Proof         *UsageProof      `json:"proof"`
	LossPPM       *int64           `json:"loss_ppm"`
	RTTMS         *int64           `json:"rtt_ms"`
}

type UsageRendition struct {
	RenditionID string `json:"rendition_id"`
	Class       string `json:"class"`
	MediaMS     int64  `json:"media_ms"`
}

type UsageTotalsV2 struct {
	IngressMediaBytes int64 `json:"ingress_media_bytes"`
	EgressMediaBytes  int64 `json:"egress_media_bytes"`
	OverheadBytes     int64 `json:"overhead_bytes"`
	ConnectedMS       int64 `json:"connected_ms"`
}

type UsageReportV2 struct {
	SchemaVersion string              `json:"schema_version"`
	SessionID     string              `json:"session_id"`
	NodeID        string              `json:"node_id"`
	WorkerID      string              `json:"worker_id"`
	Role          string              `json:"role"`
	TopologyEpoch int64               `json:"topology_epoch"`
	Sequence      int64               `json:"sequence"`
	PeriodStartMS int64               `json:"period_start_ms"`
	PeriodEndMS   int64               `json:"period_end_ms"`
	Connections   []UsageConnectionV2 `json:"connections"`
	Renditions    *[]UsageRendition   `json:"renditions"`
	Totals        UsageTotalsV2       `json:"totals"`
}

func TotalsOf(rows []UsageConnectionV2) UsageTotalsV2 {
	var totals UsageTotalsV2
	for _, row := range rows {
		if row.Direction == "ingress" {
			totals.IngressMediaBytes += row.MediaBytes
		} else {
			totals.EgressMediaBytes += row.MediaBytes
		}
		totals.OverheadBytes += row.OverheadBytes
		totals.ConnectedMS += row.ConnectedMS
	}
	return totals
}

type UsageIssue struct {
	Path    string
	Message string
}

func (issue *UsageIssue) Error() string {
	return UsageReportV2Schema + ": " + issue.Path + ": " + issue.Message
}

var usageRoleTransports = map[string][]string{
	RoleSFU:       {"whip", "whep", "child"},
	RoleRelay:     {"whep", "child", "upstream"},
	RoleIngest:    {"whip", "rtmp", "rtmps", "srt", "child"},
	RoleTranscode: {"child", "upstream"},
	RolePackage:   {"hls", "child", "upstream"},
	RoleShield:    {"hls", "child", "upstream"},
	RoleEdge:      {"hls", "child", "upstream"},
}

func inSafeRange(values ...int64) bool {
	for _, value := range values {
		if value < 0 || value > maxSafe {
			return false
		}
	}
	return true
}

func classAmountsValid(amounts *ClassAmounts) bool {
	return amounts == nil || inSafeRange(amounts.Audio, amounts.SD, amounts.HD, amounts.FHD, amounts.UHD)
}

func rowShape(row UsageConnectionV2) string {
	switch {
	case row.ConnectionID != AnonymousConnectionID && !UsageConnectionIDPattern.MatchString(row.ConnectionID):
		return "connection_id"
	case row.TokenNonce != nil && !tokenNoncePattern.MatchString(*row.TokenNonce):
		return "token_nonce"
	case row.Identity != nil && !tokenIdentityPattern.MatchString(*row.Identity):
		return "identity"
	case row.Kind != nil && !contains(ParticipantKinds, *row.Kind):
		return "kind"
	case row.Direction != "ingress" && row.Direction != "egress":
		return "direction"
	case !contains([]string{"whip", "whep", "hls", "rtmp", "rtmps", "srt", "child", "upstream"}, row.Transport):
		return "transport"
	case !inSafeRange(row.StartMS):
		return "start_ms"
	case !inSafeRange(row.EndMS):
		return "end_ms"
	case row.Intervals != nil && (len(*row.Intervals) < 2 || len(*row.Intervals) > UsageIntervalsMax):
		return "intervals"
	case !inSafeRange(row.ConnectedMS):
		return "connected_ms"
	case !inSafeRange(row.MediaMS):
		return "media_ms"
	case !inSafeRange(row.MediaBytes):
		return "media_bytes"
	case !inSafeRange(row.OverheadBytes):
		return "overhead_bytes"
	case !classAmountsValid(row.ClassMS):
		return "class_ms"
	case !classAmountsValid(row.ClassBytes):
		return "class_bytes"
	case row.LossPPM != nil && (*row.LossPPM < 0 || *row.LossPPM > 1_000_000):
		return "loss_ppm"
	case row.RTTMS != nil && !inSafeRange(*row.RTTMS):
		return "rtt_ms"
	}
	if row.Intervals != nil {
		for _, interval := range *row.Intervals {
			if !inSafeRange(interval[0], interval[1]) {
				return "intervals"
			}
		}
	}
	if row.Receipt != nil {
		if validateReceiptText(row.Receipt.Value) != nil {
			return "receipt.value"
		}
		if !inSafeRange(row.Receipt.ReceivedMS) {
			return "receipt.received_ms"
		}
	}
	if row.Proof != nil {
		return proofShape(*row.Proof)
	}
	return ""
}

var sealedPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{66,}$`)

func proofShape(proof UsageProof) string {
	switch {
	case !inSafeRange(proof.VerifiedMS):
		return "proof.verified_ms"
	case proof.Type == ProofGrant:
		if proof.Grant == nil || proof.KeyID != nil || proof.Sealed != nil || validateReceiptGrantText(*proof.Grant) != nil {
			return "proof.grant"
		}
	case proof.Type == ProofSealedToken:
		if proof.KeyID == nil || !SealKeyIDPattern.MatchString(*proof.KeyID) {
			return "proof.key_id"
		}
		if proof.Grant != nil || proof.Sealed == nil || len(*proof.Sealed) > SealedTokenMaxLength || !sealedPattern.MatchString(*proof.Sealed) {
			return "proof.sealed"
		}
	default:
		return "proof.type"
	}
	return ""
}

func rowIssue(row UsageConnectionV2, role string, periodStart, periodEnd int64) (field, message string) {
	if !contains(usageRoleTransports[role], row.Transport) {
		return "transport", "not a transport of this role"
	}
	ingress := contains([]string{"whip", "rtmp", "rtmps", "srt", "upstream"}, row.Transport)
	if (row.Direction == "ingress") != ingress {
		return "direction", "the transport's direction"
	}
	service := row.Transport == "child" || row.Transport == "upstream"
	anonymous := row.ConnectionID == AnonymousConnectionID
	encoder := contains([]string{"whip", "rtmp", "rtmps", "srt"}, row.Transport)
	if anonymous && row.Transport != "hls" {
		return "connection_id", "anonymous is an HLS row"
	}
	switch {
	case service:
		if row.TokenNonce != nil {
			return "token_nonce", "a service row has no token"
		}
		if row.Identity == nil || !strings.HasPrefix(*row.Identity, NodeIdentityPrefix) {
			return "identity", "a service row names the other node"
		}
	case anonymous:
		if row.TokenNonce != nil {
			return "token_nonce", "the anonymous row has no token"
		}
		if row.Identity != nil {
			return "identity", "the anonymous row has no identity"
		}
	default:
		if row.Identity == nil {
			return "identity", "a client row has an identity"
		}
		if strings.HasPrefix(*row.Identity, NodeIdentityPrefix) {
			return "identity", "node identities are service rows"
		}
		streamKey := strings.HasPrefix(*row.Identity, StreamKeyIdentityPrefix)
		if streamKey && !encoder {
			return "identity", "stream keys only publish"
		}
		if !streamKey && row.Transport != "whip" && encoder {
			return "identity", "RTMP and SRT publish with a stream key"
		}
		if streamKey != (row.TokenNonce == nil) {
			return "token_nonce", "a token row has its nonce, a stream key row none"
		}
	}
	if (row.Kind == nil) != (row.TokenNonce == nil) {
		return "kind", "kind is set exactly for a token row"
	}
	if row.StartMS < periodStart {
		return "start_ms", "before the period"
	}
	if row.EndMS < row.StartMS || row.EndMS > periodEnd {
		return "end_ms", "outside the period"
	}
	connected := row.EndMS - row.StartMS
	if row.Intervals != nil {
		connected = 0
		intervals := *row.Intervals
		for index, interval := range intervals {
			if interval[1] <= interval[0] || (index > 0 && interval[0] < intervals[index-1][1]+UsageOutageMinMS) {
				return "intervals", "ordered intervals at least 1 s apart"
			}
			connected += interval[1] - interval[0]
		}
		if intervals[0][0] != row.StartMS || intervals[len(intervals)-1][1] != row.EndMS {
			return "intervals", "start_ms and end_ms are the first and last instants"
		}
	}
	if row.ConnectedMS > connected {
		return "connected_ms", "more than the intervals"
	}
	if anonymous && row.ConnectedMS != 0 {
		return "connected_ms", "the anonymous row has no connected time"
	}
	if row.MediaMS > row.ConnectedMS {
		return "media_ms", "more than connected_ms"
	}
	if service && row.MediaMS != 0 {
		return "media_ms", "a service row has no media time"
	}
	if row.OverheadBytes != 0 && contains([]string{"hls", "rtmp", "rtmps", "srt"}, row.Transport) {
		return "overhead_bytes", "no overhead on this transport"
	}
	if service != (row.ClassMS == nil) {
		return "class_ms", "set exactly for a client row"
	}
	if service != (row.ClassBytes == nil) {
		return "class_bytes", "set exactly for a client row"
	}
	if row.ClassMS != nil && row.ClassMS.Sum() != row.MediaMS {
		return "class_ms", "adds up to media_ms"
	}
	if row.ClassBytes != nil && row.ClassBytes.Sum() != row.MediaBytes {
		return "class_bytes", "adds up to media_bytes"
	}
	if row.Receipt != nil {
		if row.TokenNonce == nil {
			return "receipt", "a receipt is a token row's"
		}
		if row.Receipt.ReceivedMS < periodStart || row.Receipt.ReceivedMS > periodEnd {
			return "receipt.received_ms", "received in the period"
		}
	}
	if row.Proof != nil && row.TokenNonce == nil {
		return "proof", "a proof is a token row's"
	}
	return "", ""
}

func ValidateUsageReportV2(report UsageReportV2) error {
	issue := func(path, message string) error { return &UsageIssue{Path: path, Message: message} }
	switch {
	case report.SchemaVersion != UsageReportV2Schema:
		return issue("schema_version", "another schema")
	case !tokenIDPattern.MatchString(report.SessionID):
		return issue("session_id", "invalid format")
	case !nodeIDPattern.MatchString(report.NodeID):
		return issue("node_id", "invalid format")
	case !printablePattern.MatchString(report.WorkerID):
		return issue("worker_id", "invalid format")
	case usageRoleTransports[report.Role] == nil:
		return issue("role", "invalid enum value")
	case report.TopologyEpoch < 1 || !inSafeRange(report.TopologyEpoch):
		return issue("topology_epoch", "out of range")
	case report.Sequence < 1 || !inSafeRange(report.Sequence):
		return issue("sequence", "out of range")
	case !inSafeRange(report.PeriodStartMS):
		return issue("period_start_ms", "out of range")
	case !inSafeRange(report.PeriodEndMS):
		return issue("period_end_ms", "out of range")
	case len(report.Connections) > UsageRowsMax:
		return issue("connections", "too many rows")
	case report.Renditions != nil && len(*report.Renditions) > UsageRenditionsMax:
		return issue("renditions", "too many renditions")
	}
	for index, row := range report.Connections {
		if field := rowShape(row); field != "" {
			return issue(fmt.Sprintf("connections.%d.%s", index, field), "invalid value")
		}
	}
	if report.Renditions != nil {
		for index, rendition := range *report.Renditions {
			if !renditionIDPattern.MatchString(rendition.RenditionID) || classRank(rendition.Class) < 0 || !inSafeRange(rendition.MediaMS) {
				return issue(fmt.Sprintf("renditions.%d", index), "invalid value")
			}
		}
	}
	if report.PeriodEndMS <= report.PeriodStartMS || report.PeriodStartMS/hourMS != (report.PeriodEndMS-1)/hourMS {
		return issue("period_end_ms", "a period lies in one UTC hour")
	}
	ids := map[string]bool{}
	for index, row := range report.Connections {
		if ids[row.ConnectionID] {
			return issue(fmt.Sprintf("connections.%d.connection_id", index), "duplicate connection_id")
		}
		ids[row.ConnectionID] = true
		if field, message := rowIssue(row, report.Role, report.PeriodStartMS, report.PeriodEndMS); field != "" {
			return issue(fmt.Sprintf("connections.%d.%s", index, field), message)
		}
	}
	if (report.Role == RoleTranscode || report.Role == RolePackage) != (report.Renditions != nil) {
		return issue("renditions", "set exactly for transcode and package nodes")
	}
	if report.Renditions != nil {
		seen := map[string]bool{}
		for index, rendition := range *report.Renditions {
			if seen[rendition.RenditionID] {
				return issue(fmt.Sprintf("renditions.%d.rendition_id", index), "duplicate rendition_id")
			}
			seen[rendition.RenditionID] = true
		}
	}
	totals := TotalsOf(report.Connections)
	for _, check := range []struct {
		name      string
		got, want int64
	}{{"ingress_media_bytes", report.Totals.IngressMediaBytes, totals.IngressMediaBytes}, {"egress_media_bytes", report.Totals.EgressMediaBytes, totals.EgressMediaBytes},
		{"overhead_bytes", report.Totals.OverheadBytes, totals.OverheadBytes}, {"connected_ms", report.Totals.ConnectedMS, totals.ConnectedMS}} {
		if check.got != check.want {
			return issue("totals."+check.name, "the sum of the rows")
		}
	}
	return nil
}

func ValidateUsageReportV2Message(message UsageReportMessage) error {
	if message.Type != UsageReportType || message.SchemaVersion != UsageReportV2Schema || message.Report.Schema != UsageReportV2Schema ||
		message.WorkloadID == "" || len(message.WorkloadID) > 256 || !nodeIDPattern.MatchString(message.NodeID) {
		return ErrEnvelope
	}
	return nil
}

var (
	ErrUsagePayload           = errors.New("payload_invalid")
	ErrUsageCarriesCredential = errors.New("payload_carries_credential")
)

var bearerPattern = regexp.MustCompile(`bmt[0-9]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{86}|bpc[0-9]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{43}|bsk_[A-Za-z0-9_-]{43}`)

var unscannedRowFields = map[string]bool{"identity": true, "token_nonce": true, "proof": true, "receipt": true}

func CarriesCredential(payload []byte) bool {
	var value any
	if json.Unmarshal(payload, &value) != nil {
		return false
	}
	var scan func(value any, row bool, depth int) bool
	scan = func(value any, row bool, depth int) bool {
		if text, ok := value.(string); ok {
			return bearerPattern.MatchString(text)
		}
		if depth >= UsageMaxDepth {
			return false
		}
		switch typed := value.(type) {
		case []any:
			for _, item := range typed {
				if scan(item, row, depth+1) {
					return true
				}
			}
		case map[string]any:
			for key, item := range typed {
				if !(row && unscannedRowFields[key]) && scan(item, row || key == "connections", depth+1) {
					return true
				}
			}
		}
		return false
	}
	return scan(value, false, 0)
}

const UsageMaxDepth = 16

func strictJSONText(payload []byte) bool {
	type level struct {
		keys  map[string]bool
		value bool
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var open []*level
	for {
		token, err := decoder.Token()
		if err != nil {
			return true
		}
		if len(open) > 0 {
			if current := open[len(open)-1]; current.keys != nil && token != json.Delim('}') {
				if !current.value {
					key, _ := token.(string)
					if current.keys[key] {
						return false
					}
					current.keys[key], current.value = true, true
					continue
				}
				current.value = false
			}
		}
		switch typed := token.(type) {
		case json.Delim:
			switch typed {
			case '{':
				open = append(open, &level{keys: map[string]bool{}})
			case '[':
				open = append(open, &level{})
			default:
				open = open[:len(open)-1]
			}
			if len(open) > UsageMaxDepth {
				return false
			}
		case json.Number:
			if strings.ContainsAny(string(typed), "-.eE") {
				return false
			}
		}
	}
}

func DecodeUsageReportV2(envelope SignedEnvelope, nodeKey []byte) (UsageReportV2, error) {
	payload, err := OpenEnvelope(envelope, UsageReportV2Schema, nodeKey)
	if err != nil {
		return UsageReportV2{}, err
	}
	return ParseUsageReportV2(payload)
}

func ParseUsageReportV2(payload []byte) (UsageReportV2, error) {
	var report UsageReportV2
	if !utf8.Valid(payload) || !json.Valid(payload) {
		return report, ErrUsagePayload
	}
	if !strictJSONText(payload) {
		return report, ErrUsagePayload
	}
	if CarriesCredential(payload) {
		return report, ErrUsageCarriesCredential
	}
	if err := UnmarshalStrict(payload, &report); err != nil {
		return UsageReportV2{}, ErrUsagePayload
	}
	if err := ValidateUsageReportV2(report); err != nil {
		return UsageReportV2{}, err
	}
	return report, nil
}

const SealedTokenInfo = "beam-media-sealed-token/v1"

type ProofBinding struct {
	SessionID    string `json:"session_id"`
	NodeID       string `json:"node_id"`
	WorkloadID   string `json:"workload_id"`
	ConnectionID string `json:"connection_id"`
	TokenNonce   string `json:"token_nonce"`
}

func SealedTokenAAD(binding ProofBinding) ([]byte, error) {
	parts := []string{binding.SessionID, binding.NodeID, binding.WorkloadID, binding.ConnectionID, binding.TokenNonce}
	for _, part := range parts {
		if !printablePattern.MatchString(part) {
			return nil, errors.New("mediacontracts: a proof binding is printable ASCII")
		}
	}
	return []byte(strings.Join(append([]string{SealedTokenInfo}, parts...), "\n")), nil
}

func SealToken(token string, key UsageSealKey, binding ProofBinding, verifiedMS int64) (UsageProof, error) {
	return sealToken(token, key, binding, verifiedMS, HPKESeal)
}

func SealTokenDeterministic(token string, key UsageSealKey, binding ProofBinding, verifiedMS int64, ephemeral []byte) (UsageProof, error) {
	return sealToken(token, key, binding, verifiedMS, func(recipient, info, aad, plaintext []byte) ([]byte, error) {
		return HPKESealDeterministic(ephemeral, recipient, info, aad, plaintext)
	})
}

func sealToken(token string, key UsageSealKey, binding ProofBinding, verifiedMS int64, seal func(recipient, info, aad, plaintext []byte) ([]byte, error)) (UsageProof, error) {
	recipient, err := DecodeB64URL(key.PublicKey)
	if err != nil {
		return UsageProof{}, err
	}
	if len(token) > TokenMaxLength {
		return UsageProof{}, errors.New("mediacontracts: cannot seal this token")
	}
	aad, err := SealedTokenAAD(binding)
	if err != nil {
		return UsageProof{}, err
	}
	sealed, err := seal(recipient, []byte(SealedTokenInfo), aad, []byte(token))
	if err != nil {
		return UsageProof{}, err
	}
	keyID, text := key.KeyID, B64URL(sealed)
	return UsageProof{Type: ProofSealedToken, VerifiedMS: verifiedMS, KeyID: &keyID, Sealed: &text}, nil
}

func OpenSealedToken(proof UsageProof, privateKey []byte, binding ProofBinding) (string, error) {
	if proof.Type != ProofSealedToken || proof.Sealed == nil {
		return "", ErrHPKEOpen
	}
	sealed, err := DecodeB64URL(*proof.Sealed)
	if err != nil {
		return "", ErrHPKEOpen
	}
	aad, err := SealedTokenAAD(binding)
	if err != nil {
		return "", ErrHPKEOpen
	}
	token, err := HPKEOpen(privateKey, []byte(SealedTokenInfo), aad, sealed)
	return string(token), err
}

type ProvenAdmission struct {
	Session    string  `json:"session"`
	Nonce      string  `json:"nonce"`
	Identity   string  `json:"identity"`
	Role       string  `json:"role"`
	Kind       string  `json:"kind"`
	IssuedAt   int64   `json:"iat"`
	Expires    int64   `json:"exp"`
	ReceiptKey *string `json:"receipt_key"`
}

var (
	ErrProofKeyUnknown   = errors.New("proof_key_unknown")
	ErrProofNotOpened    = errors.New("proof_not_opened")
	ErrProofTokenRefused = errors.New("proof_token_refused")
	ErrProofGrantRefused = errors.New("proof_grant_refused")
	ErrProofMismatch     = errors.New("proof_mismatch")
)

type ProofContext struct {
	WorkloadID string
	SessionID  string
	NodeID     string
	SealKeys   map[string][]byte
	GrantKeys  []ReceiptGrantKey
	Token      TokenVerifyContext

	ReportedBefore bool
}

func denied(deny TokenDenyList, nonce, identity string, now int64) bool {
	for _, entry := range deny.Nonces {
		if entry.Nonce == nonce {
			return true
		}
	}
	for _, entry := range deny.Identities {
		if entry.Identity == identity && now < entry.Until {
			return true
		}
	}
	return false
}

func VerifyUsageProof(row UsageConnectionV2, context ProofContext) (ProvenAdmission, error) {
	if row.Proof == nil || row.TokenNonce == nil || row.Identity == nil || row.Kind == nil {
		return ProvenAdmission{}, ErrProofMismatch
	}
	proof, now := *row.Proof, row.Proof.VerifiedMS/1000
	if proof.VerifiedMS > row.StartMS || (!context.ReportedBefore && proof.VerifiedMS < row.StartMS-ProofMaxLeadMS) {
		return ProvenAdmission{}, ErrProofMismatch
	}
	var admission ProvenAdmission
	switch {
	case proof.Type == ProofGrant && proof.Grant != nil:
		parsed, err := ParseReceiptGrant(*proof.Grant)
		if err != nil {
			return ProvenAdmission{}, ErrProofGrantRefused
		}
		grant, err := VerifyReceiptGrant(*proof.Grant, ReceiptGrantContext{Keys: context.GrantKeys, Session: context.SessionID, Nonce: *row.TokenNonce,
			Identity: *row.Identity, Kind: *row.Kind, Role: parsed.Role, Now: now})
		if err != nil || denied(context.Token.Deny, grant.Nonce, grant.Identity, now) {
			return ProvenAdmission{}, ErrProofGrantRefused
		}
		admission = ProvenAdmission{Session: grant.Session, Nonce: grant.Nonce, Identity: grant.Identity, Role: grant.Role, Kind: grant.Kind,
			IssuedAt: grant.IssuedAt, Expires: grant.Expires, ReceiptKey: &grant.Key}
	case proof.Type == ProofSealedToken && proof.KeyID != nil:
		key, known := context.SealKeys[*proof.KeyID]
		if !known {
			return ProvenAdmission{}, ErrProofKeyUnknown
		}
		token, err := OpenSealedToken(proof, key, ProofBinding{SessionID: context.SessionID, NodeID: context.NodeID, WorkloadID: context.WorkloadID,
			ConnectionID: row.ConnectionID, TokenNonce: *row.TokenNonce})
		if err != nil {
			return ProvenAdmission{}, ErrProofNotOpened
		}
		verify := context.Token
		verify.Session, verify.Now = context.SessionID, now
		claims, err := VerifyToken(token, verify)
		if err != nil {
			return ProvenAdmission{}, fmt.Errorf("%w: %s", ErrProofTokenRefused, TokenErrorCode(err))
		}
		admission = ProvenAdmission{Session: claims.Session, Nonce: claims.Nonce, Identity: claims.Identity, Role: claims.Role, Kind: ParticipantKindOf(claims),
			IssuedAt: claims.IssuedAt, Expires: claims.Expires}
	default:
		return ProvenAdmission{}, ErrProofMismatch
	}
	if admission.Nonce != *row.TokenNonce || admission.Identity != *row.Identity || admission.Kind != *row.Kind ||
		(row.Transport == "whip" && admission.Role == RoleSubscribe) {
		return ProvenAdmission{}, ErrProofMismatch
	}
	return admission, nil
}

const AdmissionHLSGraceMS = 86_400_000

func AdmissionCovers(admission ProvenAdmission, transport string, startMS int64, deny TokenDenyList) bool {
	lead := int64(ProofMaxLeadMS)
	if transport == "hls" {
		lead = AdmissionHLSGraceMS
	}
	return !denied(deny, admission.Nonce, admission.Identity, startMS/1000) &&
		startMS >= (admission.IssuedAt-TokenClockSkewSeconds)*1000 && startMS < (admission.Expires+TokenClockSkewSeconds)*1000+lead
}
