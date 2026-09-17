// Package storagehttp supplies source-only provider transport restrictions.
package storagehttp

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

func hfEndpoint(target *url.URL, entry bool) bool {
	host := strings.ToLower(target.Hostname())
	return target.Scheme == "https" && target.User == nil && target.Fragment == "" &&
		(target.Port() == "" || target.Port() == "443") &&
		(host == "s3.hf.co" || (!entry && strings.HasSuffix(host, ".cdn.hf.co")))
}

// Get follows only the verified HF provider redirect chain. The supplied
// client's TLS transport, timeout and request context remain authoritative.
// Assignment control, agent requests and destination uploads do not call Get.
func Get(client *http.Client, request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet {
		return nil, errors.New("storage source requires GET")
	}
	restricted := *client
	restricted.Jar = nil
	restricted.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) > 5 || !hfEndpoint(request.URL, true) || !hfEndpoint(next.URL, false) || next.Method != http.MethodGet {
			return errors.New("storage source redirect rejected")
		}
		// The CDN URL is independently signed. Copy only range conditions;
		// never disclose authorization, cookies, path tokens or signed Referers.
		next.Header = make(http.Header)
		for _, key := range []string{"Range", "If-Match", "If-Unmodified-Since", "Accept-Encoding"} {
			if value := via[0].Header.Get(key); value != "" {
				next.Header.Set(key, value)
			}
		}
		return nil
	}
	return restricted.Do(request)
}
