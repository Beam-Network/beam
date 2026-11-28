package mediafleet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

const (
	certRetryAssigned      = time.Minute
	certRetryAssignedFirst = 2 * time.Second
	certRetryCA            = 5 * time.Minute
	certRetryBusyFirst     = 15 * time.Second
	certRetryReleased      = 6 * time.Hour
	certReleasedAfter      = 3
	certForceInterval      = 10 * time.Minute
	certRetryEvicted       = time.Hour
	certCheckInterval      = time.Minute
)

type hostCertificate struct {
	key         *ecdsa.PrivateKey
	certificate *tls.Certificate
	renewAfter  time.Time
	notAfter    time.Time
	requestID   string
	requestedAt time.Time
	retryAfter  time.Time
	lastFailure string
	notAssigned int
	busy        int
	forcedAt    time.Time
	retry       *time.Timer
}

type Certificates struct {
	workerID string
	dir      string
	send     func(string, json.RawMessage) error

	mu      sync.Mutex
	hosts   map[string]*hostCertificate
	stopped bool
}

func newCertificates(workerID, stateDir string, send func(string, json.RawMessage) error) (*Certificates, error) {
	certificates := &Certificates{workerID: workerID, send: send, hosts: map[string]*hostCertificate{}}
	if stateDir == "" {
		return certificates, nil
	}
	certificates.dir = filepath.Join(stateDir, "media-certs")
	if err := os.MkdirAll(certificates.dir, 0o700); err != nil {
		return nil, err
	}
	if stale, err := filepath.Glob(filepath.Join(certificates.dir, "*.ingest.*.pem")); err == nil {
		for _, path := range stale {
			if ingestHostname(strings.TrimSuffix(filepath.Base(path), ".pem")) {
				_ = os.Remove(path)
			}
		}
	}
	keys, err := filepath.Glob(filepath.Join(certificates.dir, "*.key"))
	if err != nil {
		return nil, err
	}
	for _, path := range keys {
		hostname := strings.TrimSuffix(filepath.Base(path), ".key")
		if !mediacontracts.ValidHostname(hostname) {
			continue
		}
		if ingestHostname(hostname) {
			_ = os.Remove(path)
			_ = os.Remove(filepath.Join(certificates.dir, hostname+".pem"))
			log.Printf("media certificate %s: retired (ingest is served on the edge host)", hostname)
			continue
		}
		host, err := certificates.load(hostname)
		if err != nil {
			log.Printf("media certificate %s: %v", hostname, err)
			continue
		}
		certificates.hosts[hostname] = host
	}
	return certificates, nil
}

func (c *Certificates) load(hostname string) (*hostCertificate, error) {
	encodedKey, err := os.ReadFile(filepath.Join(c.dir, hostname+".key"))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(encodedKey)
	if block == nil {
		return nil, errors.New("key file holds no PEM block")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	host := &hostCertificate{key: key}
	if chain, err := os.ReadFile(filepath.Join(c.dir, hostname+".pem")); err == nil {
		if certificate, leaf, err := pairCertificate(chain, key, hostname); err == nil {
			host.certificate, host.notAfter = certificate, leaf.NotAfter
			host.renewAfter = leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) * 2 / 3)
		}
	}
	return host, nil
}

func (c *Certificates) Hostnames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := make([]string, 0, len(c.hosts))
	for name := range c.hosts {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func ingestHostname(hostname string) bool {
	labels := strings.Split(hostname, ".")
	return len(labels) > 2 && labels[1] == "ingest"
}

func (c *Certificates) Ensure(hostname string) error {
	if !mediacontracts.ValidHostname(hostname) {
		return fmt.Errorf("invalid media host name %q", hostname)
	}
	if ingestHostname(hostname) {
		return fmt.Errorf("media host %q is an ingest name: ingest is served on the edge host, no certificate", hostname)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if host := c.hosts[hostname]; host != nil && host.lastFailure == "hostname_not_assigned" {
		host.retryAfter, host.notAssigned = time.Time{}, 0
	}
	return c.ensureLocked(hostname, time.Now())
}

func (c *Certificates) Forget(hostname string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if host := c.hosts[hostname]; host != nil && host.retry != nil {
		host.retry.Stop()
	}
	delete(c.hosts, hostname)
	if c.dir != "" {
		_ = os.Remove(filepath.Join(c.dir, hostname+".key"))
		_ = os.Remove(filepath.Join(c.dir, hostname+".pem"))
	}
}

func (c *Certificates) ensureLocked(hostname string, now time.Time) error {
	host := c.hosts[hostname]
	if host == nil {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		host = &hostCertificate{key: key}
		if err := c.saveKey(hostname, key); err != nil {
			return err
		}
		c.hosts[hostname] = host
	}
	if host.certificate != nil && now.Before(host.renewAfter) {
		return nil
	}
	if now.Before(host.retryAfter) || (host.requestID != "" && now.Sub(host.requestedAt) < certRetryAssigned) {
		return nil
	}
	return c.requestLocked(hostname, host, now)
}

func (c *Certificates) Require(hostname string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	host := c.hosts[hostname]
	if host == nil || c.stopped {
		return nil
	}
	now := time.Now()
	if host.requestID != "" && now.Sub(host.requestedAt) < certRetryAssigned {
		return nil
	}
	if !host.forcedAt.IsZero() && now.Sub(host.forcedAt) < certForceInterval {
		return nil
	}
	switch host.lastFailure {
	case "ca_unavailable", "rate_limited", "csr_invalid", "busy", "worker_evicted":
		if now.Before(host.retryAfter) {
			return nil
		}
	}
	host.forcedAt, host.retryAfter = now, time.Time{}
	log.Printf("media certificate for %s: Core holds no valid one, asking a fresh one", hostname)
	return c.requestLocked(hostname, host, now)
}

func (c *Certificates) requestLocked(hostname string, host *hostCertificate, now time.Time) error {
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{hostname}}, host.key)
	if err != nil {
		return err
	}
	requestID := "crt_" + randomID(12)
	request := mediacontracts.CertRequest{Type: mediacontracts.CertRequestType, SchemaVersion: mediacontracts.EdgeCertSchema,
		RequestID: requestID, WorkerID: c.workerID, Hostname: hostname,
		CSRPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})), RequestedAtMS: now.UnixMilli()}
	encoded, err := mediacontracts.MarshalWire(request)
	if err != nil {
		return err
	}
	host.requestID, host.requestedAt = requestID, now
	return c.send(mediacontracts.CertRequestType, encoded)
}

func (c *Certificates) handleResult(result mediacontracts.CertResult) error {
	if result.Type != mediacontracts.CertResultType || result.SchemaVersion != mediacontracts.EdgeCertSchema || result.WorkerID != c.workerID {
		return errors.New("media_cert_result is not for this Worker")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	host := c.hosts[result.Hostname]
	if host == nil {
		return fmt.Errorf("media_cert_result for a host this Worker does not hold: %s", result.Hostname)
	}
	if result.Status != "issued" && host.requestID != result.RequestID {
		return nil
	}
	host.requestID = ""
	now := time.Now()
	if result.Status != "issued" {
		reason := "refused"
		if result.Reason != nil {
			reason = *result.Reason
		}
		host.lastFailure = reason
		if reason != "hostname_not_assigned" {
			host.notAssigned = 0
		}
		if reason != "busy" {
			host.busy = 0
		}
		switch reason {
		case "hostname_not_assigned":
			host.notAssigned++
			if host.certificate != nil && host.notAssigned >= certReleasedAfter {
				host.retryAfter = now.Add(certRetryReleased)
				break
			}
			wait := min(certRetryAssignedFirst<<min(host.notAssigned-1, 5), certRetryAssigned)
			host.retryAfter = now.Add(wait)
			c.retryLocked(result.Hostname, host, wait)
		case "busy":
			wait := min(certRetryBusyFirst<<min(host.busy, 5), certRetryCA)
			host.busy++
			host.retryAfter = now.Add(wait)
			c.retryLocked(result.Hostname, host, wait)
		case "worker_evicted":
			host.retryAfter = now.Add(certRetryEvicted)
		case "csr_invalid":
			if key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err == nil && c.saveKey(result.Hostname, key) == nil {
				host.key = key
			}
			host.retryAfter = now.Add(certRetryCA)
		default:
			host.retryAfter = now.Add(certRetryCA)
		}
		log.Printf("media certificate for %s refused: %s", result.Hostname, reason)
		return nil
	}
	if result.CertificatePEM == nil {
		return errors.New("issued media_cert_result carries no certificate")
	}
	certificate, leaf, err := pairCertificate([]byte(*result.CertificatePEM), host.key, result.Hostname)
	if err != nil {
		host.retryAfter = now.Add(certRetryCA)
		return fmt.Errorf("issued media certificate for %s: %w", result.Hostname, err)
	}
	host.certificate, host.notAfter, host.lastFailure, host.notAssigned, host.busy = certificate, leaf.NotAfter, "", 0, 0
	host.renewAfter = leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) * 2 / 3)
	if result.RenewAfterMS != nil {
		host.renewAfter = time.UnixMilli(*result.RenewAfterMS)
	}
	if c.dir != "" {
		if err := writePrivate(filepath.Join(c.dir, result.Hostname+".pem"), []byte(*result.CertificatePEM)); err != nil {
			return err
		}
	}
	log.Printf("media certificate for %s issued, valid until %s", result.Hostname, leaf.NotAfter.UTC().Format(time.RFC3339))
	return nil
}

func (c *Certificates) Served() []mediacontracts.ServedCertificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	out := []mediacontracts.ServedCertificate{}
	for hostname, host := range c.hosts {
		if host.certificate == nil || len(host.certificate.Certificate) == 0 || !now.Before(host.notAfter) {
			continue
		}
		sum := sha256.Sum256(host.certificate.Certificate[0])
		out = append(out, mediacontracts.ServedCertificate{Hostname: hostname, SHA256: hex.EncodeToString(sum[:])})
	}
	slices.SortFunc(out, func(left, right mediacontracts.ServedCertificate) int {
		return strings.Compare(left.Hostname, right.Hostname)
	})
	if len(out) > mediacontracts.ServedCertificatesMax {
		out = out[:mediacontracts.ServedCertificatesMax]
	}
	return out
}

func (c *Certificates) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	c.mu.Lock()
	defer c.mu.Unlock()
	if host := c.hosts[name]; host != nil && host.certificate != nil && time.Now().Before(host.notAfter) {
		return host.certificate, nil
	}
	return nil, fmt.Errorf("no media certificate for %q", hello.ServerName)
}

func (c *Certificates) Certificate(hostname string) *tls.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	if host := c.hosts[hostname]; host != nil && host.certificate != nil && time.Now().Before(host.notAfter) {
		return host.certificate
	}
	return nil
}

func (c *Certificates) retryLocked(hostname string, host *hostCertificate, wait time.Duration) {
	if host.retry != nil {
		host.retry.Stop()
		host.retry = nil
	}
	if c.stopped {
		return
	}
	host.retry = time.AfterFunc(wait, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.stopped || c.hosts[hostname] != host {
			return
		}
		host.retry = nil
		if err := c.ensureLocked(hostname, time.Now()); err != nil {
			log.Printf("media certificate for %s: %v", hostname, err)
		}
	})
}

func (c *Certificates) run(ctx context.Context) {
	ticker := time.NewTicker(certCheckInterval)
	defer ticker.Stop()
	defer c.stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			c.renewAll(now)
		}
	}
}

func (c *Certificates) renewAll(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for hostname := range c.hosts {
		if err := c.ensureLocked(hostname, now); err != nil {
			log.Printf("media certificate for %s: %v", hostname, err)
		}
	}
}

func (c *Certificates) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	for _, host := range c.hosts {
		if host.retry != nil {
			host.retry.Stop()
			host.retry = nil
		}
	}
}

func (c *Certificates) saveKey(hostname string, key *ecdsa.PrivateKey) error {
	if c.dir == "" {
		return nil
	}
	encoded, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(c.dir, hostname+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded}))
}

func pairCertificate(chainPEM []byte, key *ecdsa.PrivateKey, hostname string) (*tls.Certificate, *x509.Certificate, error) {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	pair, err := tls.X509KeyPair(chainPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	if err := leaf.VerifyHostname(hostname); err != nil {
		return nil, nil, err
	}
	pair.Leaf = leaf
	return &pair, leaf, nil
}

func writePrivate(path string, data []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func randomID(bytes int) string {
	buffer := make([]byte, bytes)
	_, _ = rand.Read(buffer)
	return base64.RawURLEncoding.EncodeToString(buffer)
}
