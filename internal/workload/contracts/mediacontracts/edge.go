package mediacontracts

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"regexp"
	"strings"
)

var (
	dnsLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	ErrEdgeDomain   = errors.New("mediacontracts: invalid edge domain")
)

func EdgeLabel(workerID string) string {
	if dnsLabelPattern.MatchString(workerID) && !strings.HasPrefix(workerID, "x-") &&
		!(len(workerID) >= 4 && workerID[2:4] == "--") {
		return workerID
	}
	digest := sha256.Sum256([]byte(workerID))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:20])
	return "x-" + strings.ToLower(encoded)
}

func ValidHostname(value string) bool {
	return len(value) <= 253 && hostnamePattern.MatchString(value)
}

func EdgeHostname(workerID, domain string) (string, error) {
	if !ValidHostname(domain) {
		return "", ErrEdgeDomain
	}
	host := EdgeLabel(workerID) + ".edge." + domain
	if !ValidHostname(host) {
		return "", ErrEdgeDomain
	}
	return host, nil
}
