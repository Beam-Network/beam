package roomtransfer

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

// Worker certificates are authenticated by the certificate fingerprint in the
// coordinator-authorized runtime assignment, rather than by a public DNS name.
// Keys remain in memory and are replaced when the worker listener restarts.
func newWorkerCertificate(now time.Time) (tls.Certificate, string, time.Time, error) {
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

type workerCertificate struct {
	certificate tls.Certificate
	fingerprint string
	expiry      time.Time
}
type workerCertificates struct {
	mu           sync.Mutex
	now          func() time.Time
	current      *workerCertificate
	certificates map[string]*workerCertificate
}

func (store *workerCertificates) forLease(expiresAt time.Time) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now().UTC()
	if !expiresAt.After(now) || expiresAt.After(now.Add(24*time.Hour)) {
		return "", errors.New("worker TLS lease expiry is invalid")
	}
	for name, entry := range store.certificates {
		if !now.Before(entry.expiry) {
			delete(store.certificates, name)
		}
	}
	if store.current == nil || store.current.expiry.Before(expiresAt) {
		certificate, fingerprint, expiry, err := newWorkerCertificate(now)
		if err != nil {
			return "", errors.New("worker TLS certificate generation failed")
		}
		entry := &workerCertificate{certificate: certificate, fingerprint: fingerprint, expiry: expiry}
		store.current = entry
		store.certificates[fingerprint[:32]+"."+fingerprint[32:]+".room-worker.invalid"] = entry
	}
	return store.current.fingerprint, nil
}

func secureWorkerListener(listener net.Listener, now func() time.Time) (net.Listener, *workerCertificates) {
	store := &workerCertificates{now: now, certificates: make(map[string]*workerCertificate)}
	config := &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		store.mu.Lock()
		defer store.mu.Unlock()
		entry := store.certificates[strings.ToLower(hello.ServerName)]
		if entry == nil || !store.now().Before(entry.expiry) {
			return nil, errors.New("worker certificate is not assigned")
		}
		return &entry.certificate, nil
	}}
	return tls.NewListener(listener, config), store
}
