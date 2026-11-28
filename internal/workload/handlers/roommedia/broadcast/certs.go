package broadcast

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"math/big"
	"net"
	"sync"
	"time"
)

var fallbackCertificates struct {
	sync.Mutex
	byHost map[string]*tls.Certificate
}

var servedHosts struct {
	sync.Mutex
	count map[string]int
}

func serveHost(host string) func() {
	servedHosts.Lock()
	defer servedHosts.Unlock()
	if servedHosts.count == nil {
		servedHosts.count = map[string]int{}
	}
	servedHosts.count[host]++
	var once sync.Once
	return func() {
		once.Do(func() {
			servedHosts.Lock()
			defer servedHosts.Unlock()
			if servedHosts.count[host]--; servedHosts.count[host] <= 0 {
				delete(servedHosts.count, host)
			}
			fallbackCertificates.Lock()
			delete(fallbackCertificates.byHost, host)
			fallbackCertificates.Unlock()
		})
	}
}

func served(host string) bool {
	servedHosts.Lock()
	defer servedHosts.Unlock()
	return servedHosts.count[host] > 0
}

func fallbackCertificate(host string) (*tls.Certificate, error) {
	if !served(host) {
		host = ""
	}
	fallbackCertificates.Lock()
	defer fallbackCertificates.Unlock()
	if certificate := fallbackCertificates.byHost[host]; certificate != nil && time.Now().Before(certificate.Leaf.NotAfter) {
		return certificate, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(30 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else if host != "" {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	leaf, _ := x509.ParseCertificate(der)
	certificate := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	if fallbackCertificates.byHost == nil {
		fallbackCertificates.byHost = map[string]*tls.Certificate{}
	}
	fallbackCertificates.byHost[host] = certificate
	return certificate, nil
}

func SHA256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
