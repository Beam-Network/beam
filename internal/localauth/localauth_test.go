package localauth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveTokenGeneratesOnceAndReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "control-token")
	first, err := ResolveToken("", path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 40 {
		t.Fatalf("generated token is too short: %d characters", len(first))
	}
	second, err := ResolveToken("  ", path)
	if err != nil || second != first {
		t.Fatalf("second start returned a different token (err %v)", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("token file mode = %v (err %v), want 0600", info.Mode().Perm(), err)
		}
	}
	configured, err := ResolveToken("operator-token", path)
	if err != nil || configured != "operator-token" {
		t.Fatalf("configured token = %q (err %v)", configured, err)
	}
}

func TestResolveTokenReplacesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control-token")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := ResolveToken("", path)
	if err != nil || token == "" {
		t.Fatalf("token = %q (err %v)", token, err)
	}
	stored, _ := os.ReadFile(path)
	if string(stored) != token+"\n" {
		t.Fatal("generated token was not stored")
	}
}

func TestRequire(t *testing.T) {
	protected := Require("secret-token", http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	for _, test := range []struct {
		method, path, authorization string
		want                        int
	}{
		{http.MethodGet, "/healthz", "", http.StatusNoContent},
		{http.MethodPost, "/healthz", "", http.StatusUnauthorized},
		{http.MethodGet, "/v1/status", "", http.StatusUnauthorized},
		{http.MethodGet, "/v1/status", "Bearer wrong", http.StatusUnauthorized},
		{http.MethodGet, "/v1/status", "secret-token", http.StatusUnauthorized},
		{http.MethodPost, "/v1/orchestrator/memberships", "Bearer secret-token", http.StatusNoContent},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		if test.authorization != "" {
			request.Header.Set("Authorization", test.authorization)
		}
		recorder := httptest.NewRecorder()
		protected.ServeHTTP(recorder, request)
		if recorder.Code != test.want {
			t.Fatalf("%s %s with %q = %d, want %d", test.method, test.path, test.authorization, recorder.Code, test.want)
		}
	}
	empty := Require("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("empty token admitted a request") }))
	request := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	request.Header.Set("Authorization", "Bearer ")
	recorder := httptest.NewRecorder()
	empty.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("empty token returned %d", recorder.Code)
	}
}
