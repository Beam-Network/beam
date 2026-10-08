// Package localauth protects the owner-local APIs with a bearer token.
package localauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ResolveToken returns the configured token, or the token stored at path, creating it when missing.
func ResolveToken(configured, path string) (string, error) {
	if token := strings.TrimSpace(configured); token != "" {
		return token, nil
	}
	if stored, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(stored)); token != "" {
			return token, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read control token: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate control token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create control token directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".control-token-*")
	if err != nil {
		return "", fmt.Errorf("write control token: %w", err)
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		temp.Close()
		return "", fmt.Errorf("write control token: %w", err)
	}
	if _, err := temp.WriteString(token + "\n"); err != nil {
		temp.Close()
		return "", fmt.Errorf("write control token: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("write control token: %w", err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return "", fmt.Errorf("write control token: %w", err)
	}
	return token, nil
}

// Require rejects every request without the bearer token except GET /healthz.
func Require(token string, next http.Handler) http.Handler {
	expected := []byte(token)
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
			next.ServeHTTP(response, request)
			return
		}
		got, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
		if !ok || len(expected) == 0 || subtle.ConstantTimeCompare([]byte(got), expected) != 1 {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusUnauthorized)
			_, _ = response.Write([]byte(`{"error":"unauthorized"}` + "\n"))
			return
		}
		next.ServeHTTP(response, request)
	})
}
