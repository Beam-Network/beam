package mediacontracts

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var RecordingBucketPrefixPattern = regexp.MustCompile(`^media/[A-Za-z0-9_-]{1,64}/[A-Za-z0-9_-]{1,64}/mrec_[0-9a-z]{26}/$`)

var (
	recordingIDPattern   = regexp.MustCompile(`^mrec_[0-9a-z]{26}$`)
	bucketNamePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	bucketRegionPattern  = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	bucketKeyPattern     = regexp.MustCompile(`^media/[A-Za-z0-9_./-]{1,1000}$`)
	headerNamePattern    = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	recordingNodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

var BucketGrantPrefixPattern = regexp.MustCompile(`^media/[A-Za-z0-9_-]{1,64}/[A-Za-z0-9_-]{1,64}/mrec_[0-9a-z]{26}/(g([1-9]|[1-5][0-9]|6[0-4])/)?$`)

func BucketGrantFor(recording NodeRecording, vod bool) (string, []string) {
	if vod {
		return recording.BucketPrefix, []string{"GET"}
	}
	return RecordingGenerationPrefix(recording.BucketPrefix, recording.RecordingGeneration()), []string{"PUT"}
}

var (
	ErrNodeRecording = errors.New("mediacontracts: invalid node recording")
	ErrBucketAccess  = errors.New("mediacontracts: invalid bucket_access")
)

func RecordingGenerationPrefix(bucketPrefix string, generation int64) string {
	return bucketPrefix + "g" + strconv.FormatInt(generation, 10) + "/"
}

func (recording NodeRecording) RecordingGeneration() int64 {
	if recording.Generation == nil {
		return 1
	}
	return *recording.Generation
}

func ValidateNodeRecording(recording NodeRecording, vod bool) error {
	if !recordingIDPattern.MatchString(recording.RecordingID) || !RecordingBucketPrefixPattern.MatchString(recording.BucketPrefix) ||
		!strings.HasSuffix(recording.BucketPrefix, "/"+recording.RecordingID+"/") {
		return ErrNodeRecording
	}
	inRange := func(value *int64) bool { return value == nil || (*value >= 1 && *value <= RecordingMaxGenerations) }
	if !inRange(recording.Generation) || !inRange(recording.Generations) {
		return ErrNodeRecording
	}
	if vod != (recording.Generations != nil) || (vod && recording.Generation != nil) {
		return ErrNodeRecording
	}
	return nil
}

func ValidateBucketAccess(access BucketAccess) error {
	if !BucketGrantPrefixPattern.MatchString(access.Prefix) || (access.Signer == nil) == (access.Credential == nil) ||
		len(access.Methods) < 1 || len(access.Methods) > 2 {
		return ErrBucketAccess
	}
	seen := map[string]bool{}
	for _, method := range access.Methods {
		if (method != "PUT" && method != "GET") || seen[method] {
			return ErrBucketAccess
		}
		seen[method] = true
	}
	if signer := access.Signer; signer != nil {
		if !httpsURL(signer.URL) || signer.Token == "" || len(signer.Token) > 4096 || signer.ExpiresAtMS < 1 {
			return ErrBucketAccess
		}
	}
	if credential := access.Credential; credential != nil {
		if !httpsURL(credential.Endpoint) || !bucketNamePattern.MatchString(credential.Bucket) || !bucketRegionPattern.MatchString(credential.Region) ||
			credential.AccessKeyID == "" || len(credential.AccessKeyID) > 256 || credential.SecretAccessKey == "" || len(credential.SecretAccessKey) > 512 ||
			credential.SessionToken == "" || len(credential.SessionToken) > 8192 || credential.ExpiresAtMS < 1 {
			return ErrBucketAccess
		}
	}
	return nil
}

func CheckNodeBucketAccess(access BucketAccess, recording NodeRecording, vod bool) error {
	if err := ValidateBucketAccess(access); err != nil {
		return err
	}
	prefix, methods := BucketGrantFor(recording, vod)
	if access.Prefix != prefix || len(access.Methods) != len(methods) || access.Methods[0] != methods[0] {
		return ErrBucketAccess
	}
	return nil
}

const (
	BucketMaxSegmentBytes       = 67_108_864
	BucketMaxInitBytes          = 1_048_576
	BucketMaxPlaylistBytes      = 8_388_608
	BucketMaxManifestPageBytes  = 1_048_576
	BucketMaxRecordingFileBytes = 65_536
)

var (
	segmentObjectPattern  = regexp.MustCompile(`^seg_[0-9]{1,20}\.m4s$`)
	manifestObjectPattern = regexp.MustCompile(`^manifest-[0-9]{1,6}\.json$`)
)

func BucketObjectMaxBytes(key string) (int64, bool) {
	name := key[strings.LastIndex(key, "/")+1:]
	switch {
	case segmentObjectPattern.MatchString(name):
		return BucketMaxSegmentBytes, true
	case InitURIPattern.MatchString(name):
		return BucketMaxInitBytes, true
	case name == "master.m3u8" || name == "playlist.m3u8":
		return BucketMaxPlaylistBytes, true
	case manifestObjectPattern.MatchString(name):
		return BucketMaxManifestPageBytes, true
	case name == "recording.json":
		return BucketMaxRecordingFileBytes, true
	}
	return 0, false
}

func ValidateBucketSignRequestShape(request BucketSignRequest) error {
	if (request.Method != "PUT" && request.Method != "GET") || !bucketKeyPattern.MatchString(request.Key) ||
		strings.Contains(request.Key, "..") || strings.Contains(request.Key, "//") ||
		(request.ContentType != nil && (*request.ContentType == "" || len(*request.ContentType) > 256)) {
		return ErrBucketAccess
	}
	if request.Method == "GET" {
		if request.ContentLength != nil {
			return ErrBucketAccess
		}
		return nil
	}
	limit, ok := BucketObjectMaxBytes(request.Key)
	if !ok || request.ContentLength == nil || *request.ContentLength < 1 || *request.ContentLength > limit {
		return ErrBucketAccess
	}
	return nil
}

func ValidBucketSignRequest(request BucketSignRequest, access BucketAccess) bool {
	if ValidateBucketSignRequestShape(request) != nil {
		return false
	}
	allowed := false
	for _, method := range access.Methods {
		allowed = allowed || method == request.Method
	}
	return allowed && strings.HasPrefix(request.Key, access.Prefix) && len(request.Key) > len(access.Prefix)
}

func ValidBucketSignResponse(response BucketSignResponse) bool {
	if !httpsURL(response.URL) || response.ExpiresAtMS < 1 || len(response.Headers) > 16 {
		return false
	}
	for name, value := range response.Headers {
		if !headerNamePattern.MatchString(name) || len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
			return false
		}
	}
	return true
}

func ValidateJoinManifestKeys(join JoinResponse) error {
	if join.HLS == nil {
		return nil
	}
	keys := join.HLS.ManifestKeys
	if len(keys) > RecordingMaxGenerations {
		return ErrManifestKeys
	}
	if join.DeliveryProfile != DeliveryVOD {
		if len(keys) > 1 || (len(keys) == 1 && keys[0].Generation != nil) {
			return ErrManifestKeys
		}
		return nil
	}
	previous := int64(0)
	for _, key := range keys {
		if key.Generation == nil || *key.Generation < 1 || *key.Generation > RecordingMaxGenerations || *key.Generation <= previous {
			return ErrManifestKeys
		}
		previous = *key.Generation
	}
	return nil
}

var ErrManifestKeys = errors.New("mediacontracts: invalid manifest_keys")

func ValidateRecordingFile(file RecordingFile) error {
	if file.SchemaVersion != RecordingFileSchema || !recordingIDPattern.MatchString(file.RecordingID) || !manifestSessionPattern.MatchString(file.SessionID) ||
		file.Generation < 1 || file.Generation > RecordingMaxGenerations || !recordingNodePattern.MatchString(file.PackagerNodeID) ||
		file.DurationMS < 0 || file.Bytes < 0 || file.UpdatedAtMS < 0 || len(file.Renditions) > 12 {
		return ErrNodeRecording
	}
	if key, err := DecodeB64URL(file.PackagerNodeKey); err != nil || len(key) != 32 {
		return ErrNodeRecording
	}
	for _, rendition := range file.Renditions {
		if !renditionIDPattern.MatchString(rendition) {
			return ErrNodeRecording
		}
	}
	return nil
}

func httpsURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}
