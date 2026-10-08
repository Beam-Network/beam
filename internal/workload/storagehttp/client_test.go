package storagehttp

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func allowPorts(servers ...*httptest.Server) func(netip.AddrPort) bool {
	allowed := map[uint16]bool{}
	for _, server := range servers {
		parsed, _ := url.Parse(server.URL)
		port, _ := netip.ParseAddrPort(parsed.Host)
		allowed[port.Port()] = true
	}
	return func(address netip.AddrPort) bool { return allowed[address.Port()] }
}

func TestNewClientRefusesNonPublicAddresses(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer internal.Close()
	port := strings.TrimPrefix(internal.URL, "http://127.0.0.1")
	client := NewClient(5 * time.Second)
	for _, target := range []string{internal.URL, "http://localhost" + port} {
		response, err := client.Get(target)
		if err == nil {
			response.Body.Close()
			t.Fatalf("%s: connection to a non-public address was allowed", target)
		}
		if !errors.Is(err, errAddressNotPublic) {
			t.Fatalf("%s: err = %v, want the public-address refusal", target, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("internal server received %d requests", hits.Load())
	}
}

func TestNewClientRefusesRedirectToNonPublicAddress(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer internal.Close()
	entry := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, internal.URL+"/metadata", http.StatusFound)
	}))
	defer entry.Close()
	client := newClient(5*time.Second, allowPorts(entry))
	response, err := client.Get(entry.URL)
	if err == nil {
		response.Body.Close()
		t.Fatal("redirect to a refused address was followed")
	}
	if !errors.Is(err, errAddressNotPublic) || hits.Load() != 0 {
		t.Fatalf("err = %v, internal hits = %d", err, hits.Load())
	}
}

func TestNewClientLimitsSourceRedirects(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, server.URL+"/next", http.StatusFound)
	}))
	defer server.Close()
	client := newClient(5*time.Second, allowPorts(server))
	if _, err := client.Get(server.URL); !errors.Is(err, errRedirectLimit) {
		t.Fatalf("err = %v, want the redirect limit", err)
	}
	var hops atomic.Int32
	final := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, "ok")
	}))
	defer final.Close()
	chain := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if hops.Add(1) < MaxSourceRedirects {
			http.Redirect(response, request, "/again", http.StatusFound)
			return
		}
		http.Redirect(response, request, final.URL, http.StatusFound)
	}))
	defer chain.Close()
	response, err := newClient(5*time.Second, allowPorts(chain, final)).Get(chain.URL)
	if err != nil {
		t.Fatalf("%d redirects were refused: %v", MaxSourceRedirects, err)
	}
	response.Body.Close()
}

func TestNewClientRefusesTLSDowngradeRedirect(t *testing.T) {
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, plain.URL, http.StatusFound)
	}))
	defer secure.Close()
	client := newClient(5*time.Second, allowPorts(secure, plain))
	client.Transport.(*http.Transport).TLSClientConfig = secure.Client().Transport.(*http.Transport).TLSClientConfig
	response, err := client.Get(secure.URL)
	if err == nil {
		response.Body.Close()
		t.Fatal("HTTPS to HTTP redirect was followed")
	}
	if !errors.Is(err, errRedirectDowngrade) || hits.Load() != 0 {
		t.Fatalf("err = %v, plain hits = %d", err, hits.Load())
	}
}

func TestNewClientDoesNotRedirectUploads(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		http.Redirect(response, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer destination.Close()
	client := newClient(5*time.Second, allowPorts(destination, target))
	request, _ := http.NewRequest(http.MethodPut, destination.URL, strings.NewReader("payload"))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || hits.Load() != 0 {
		t.Fatalf("status = %d, redirect target hits = %d", response.StatusCode, hits.Load())
	}
}

func TestNewClientIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	if proxy := NewClient(time.Second).Transport.(*http.Transport).Proxy; proxy != nil {
		t.Fatal("storage client uses an environment proxy")
	}
}
