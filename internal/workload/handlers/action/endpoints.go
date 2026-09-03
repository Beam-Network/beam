package action

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

func validateCapabilityEndpoint(rawURL string, allowedHosts []string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return errors.New("capability endpoint must be an absolute HTTP(S) URL")
	}
	hostname := strings.ToLower(parsed.Hostname())
	if parsed.Scheme == "http" && !isLoopbackHost(hostname) {
		return errors.New("non-loopback capability endpoints require HTTPS")
	}
	for _, allowed := range allowedHosts {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == hostname || (strings.HasPrefix(allowed, "*.") && strings.HasSuffix(hostname, allowed[1:])) {
			return nil
		}
	}
	return errors.New("capability endpoint host is not allowed by Worker policy")
}

// restrictedHTTPClient validates a redirect before the transport connects to
// it. Post-response validation is too late for requests carrying a capability
// token or an artifact body.
func restrictedHTTPClient(client *http.Client, allowedHosts []string) *http.Client {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	restricted := *client
	upstream := client.CheckRedirect
	restricted.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if err := validateCapabilityEndpoint(request.URL.String(), allowedHosts); err != nil {
			return err
		}
		if upstream != nil {
			return upstream(request, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &restricted
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func boundedHeaders(headers map[string]string) bool {
	if len(headers) > 32 {
		return false
	}
	for name, value := range headers {
		if name == "" || len(name) > 128 || len(value) > 8<<10 || strings.ContainsAny(name+value, "\r\n") {
			return false
		}
	}
	return true
}

func containsPermission(permissions []string, required string) bool {
	if slices.Contains(permissions, required) || slices.Contains(permissions, "*") {
		return true
	}
	category, operation, _ := strings.Cut(required, ":")
	for _, permission := range permissions {
		candidateCategory, candidateOperation, _ := strings.Cut(permission, ":")
		if candidateCategory == category && (candidateOperation == "*" ||
			(strings.HasSuffix(candidateOperation, "*") && strings.HasPrefix(operation, strings.TrimSuffix(candidateOperation, "*")))) {
			return true
		}
	}
	return false
}
