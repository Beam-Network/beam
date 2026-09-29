package workertls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

type certificate struct {
	value       tls.Certificate
	fingerprint string
	expiresAt   time.Time
}

// Certificates owns short-lived, in-memory worker certificates. A runtime
// assignment carries the fingerprint; clients pin that fingerprint instead of
// relying on public DNS certificate issuance for transient worker listeners.
type Certificates struct {
	mu       sync.Mutex
	now      func() time.Time
	current  *certificate
	byServer map[string]*certificate
}

func WrapListener(listener net.Listener, now func() time.Time) (net.Listener, *Certificates) {
	store := &Certificates{now: now, byServer: make(map[string]*certificate)}
	config := &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		store.mu.Lock()
		defer store.mu.Unlock()
		entry := store.byServer[strings.ToLower(hello.ServerName)]
		if entry == nil || !store.now().Before(entry.expiresAt) {
			return nil, errors.New("worker certificate is not assigned")
		}
		return &entry.value, nil
	}}
	return tls.NewListener(listener, config), store
}

func (store *Certificates) ForLease(expiresAt time.Time) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now().UTC()
	if !expiresAt.After(now) || expiresAt.After(now.Add(24*time.Hour)) {
		return "", errors.New("worker TLS lease expiry is invalid")
	}
	for name, entry := range store.byServer {
		if !now.Before(entry.expiresAt) {
			delete(store.byServer, name)
		}
	}
	if store.current == nil || store.current.expiresAt.Before(expiresAt) {
		value, fingerprint, expiry, err := newCertificate(now)
		if err != nil {
			return "", errors.New("worker TLS certificate generation failed")
		}
		entry := &certificate{value: value, fingerprint: fingerprint, expiresAt: expiry}
		store.current = entry
		store.byServer[ServerName(fingerprint)] = entry
	}
	return store.current.fingerprint, nil
}

func ServerName(fingerprint string) string {
	if len(fingerprint) != 64 {
		return "invalid.room-worker.invalid"
	}
	return fingerprint[:32] + "." + fingerprint[32:] + ".room-worker.invalid"
}

func newCertificate(now time.Time) (tls.Certificate, string, time.Time, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", time.Time{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", time.Time{}, err
	}
	expiry := now.Add(48 * time.Hour)
	template := &x509.Certificate{SerialNumber: serial, NotBefore: now.Add(-time.Minute), NotAfter: expiry,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", time.Time{}, err
	}
	digest := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(digest[:]), expiry, nil
}
