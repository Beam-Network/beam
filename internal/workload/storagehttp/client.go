package storagehttp

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

const MaxSourceRedirects = 3

var (
	errAddressNotPublic  = errors.New("storage address is not public")
	errRedirectLimit     = errors.New("storage source redirect limit reached")
	errRedirectDowngrade = errors.New("storage source redirect leaves HTTPS")
)

// NewClient returns the HTTP client for storage URLs.
func NewClient(timeout time.Duration) *http.Client {
	return newClient(timeout, func(address netip.AddrPort) bool {
		return contracts.PublicAddressAllowed(address.Addr())
	})
}

func newClient(timeout time.Duration, allowed func(netip.AddrPort) bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			target, err := netip.ParseAddrPort(address)
			if err != nil || !allowed(target) {
				return errAddressNotPublic
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: transport, CheckRedirect: checkRedirect}
}

func checkRedirect(next *http.Request, via []*http.Request) error {
	if via[0].Method != http.MethodGet {
		return http.ErrUseLastResponse
	}
	if len(via) > MaxSourceRedirects {
		return errRedirectLimit
	}
	if via[len(via)-1].URL.Scheme == "https" && next.URL.Scheme != "https" {
		return errRedirectDowngrade
	}
	return nil
}
