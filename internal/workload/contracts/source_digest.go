package contracts

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// SourceDigestAlgorithms are the digests every source digest computes, in one pass.
var SourceDigestAlgorithms = []string{"sha1", "sha256"}

// SourceDigest is a transfer.source_digest workload: one GET of the whole source
// object, hashed while it streams. Size is the object length the read must match.
type SourceDigest struct {
	Source HTTPEndpoint `json:"source"`
	Size   int64        `json:"size"`
}

// Validate requires an HTTPS source (plain HTTP only on loopback), no Range
// header, since the whole object is read, and a positive size.
func (digest SourceDigest) Validate() error {
	parsed, err := url.Parse(digest.Source.URL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("source digest requires an absolute source URL")
	}
	if ip := net.ParseIP(parsed.Hostname()); parsed.Scheme != "https" && !(parsed.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return errors.New("source digest requires a TLS source outside loopback")
	}
	for key := range digest.Source.Headers {
		if strings.EqualFold(key, "Range") {
			return errors.New("source digest reads the whole object and takes no Range header")
		}
	}
	if digest.Size < 1 {
		return errors.New("source digest requires a positive size")
	}
	return nil
}
