package roomtransfer

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestWorkerTLSBindsAssignedCertificateAndRotatesWithoutBreakingActivePins(t *testing.T) {
	var mu sync.Mutex
	now := time.Now().UTC()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	secure, certificates := secureWorkerListener(listener, clock)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("scoped bytes")) })}
	go func() { _ = server.Serve(secure) }()
	t.Cleanup(func() { _ = server.Close() })
	pin, err := certificates.forLease(clock().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	read := func(pin string) error {
		transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13,
			ServerName: pin[:32] + "." + pin[32:] + ".room-worker.invalid", InsecureSkipVerify: true,
			VerifyConnection: func(state tls.ConnectionState) error {
				digest := sha256.Sum256(state.PeerCertificates[0].Raw)
				if hex.EncodeToString(digest[:]) != pin {
					return errors.New("certificate mismatch")
				}
				return nil
			}}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: time.Second}
		response, err := client.Get("https://" + listener.Addr().String())
		if err != nil {
			return err
		}
		defer response.Body.Close()
		payload, err := io.ReadAll(response.Body)
		if err == nil && string(payload) != "scoped bytes" {
			return errors.New("payload mismatch")
		}
		return err
	}
	if err := read(pin); err != nil {
		t.Fatal(err)
	}
	unknown := "0000000000000000000000000000000000000000000000000000000000000000"
	if read(unknown) == nil {
		t.Fatal("unassigned TLS certificate was accepted")
	}
	mu.Lock()
	now = now.Add(47 * time.Hour)
	mu.Unlock()
	newPin, err := certificates.forLease(clock().Add(3 * time.Hour))
	if err != nil || newPin == pin {
		t.Fatalf("certificate did not rotate: %v", err)
	}
	if err := read(newPin); err != nil {
		t.Fatal(err)
	}
	if err := read(pin); err != nil {
		t.Fatalf("active old pin broke during rotation: %v", err)
	}
	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	if read(pin) == nil {
		t.Fatal("expired certificate was accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", "http://"+listener.Addr().String(), nil)
	response, err := http.DefaultClient.Do(request)
	if err == nil {
		defer response.Body.Close()
		payload, _ := io.ReadAll(response.Body)
		if string(payload) == "scoped bytes" {
			t.Fatal("plaintext worker access was accepted")
		}
	}
}
