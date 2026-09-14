package connectors

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHasActiveDuplicateControlSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Api-Key") != "orchestrator-api-key" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth/me":
			_, _ = writer.Write([]byte(`{"hotkey":"hotkey-1","current_key_role":"orchestrator"}`))
		case "/validators/orchestrators":
			_, _ = writer.Write([]byte(`{"orchestrators":[{"hotkey":"hotkey-1","status":"active"}]}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	duplicate, err := hasActiveDuplicateControlSession(NATSConfig{
		PublicAPIURL:   server.URL + "/",
		Hotkey:         "hotkey-1",
		User:           "hotkey-1",
		Password:       "orchestrator-api-key",
		RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("classify authorization rejection: %v", err)
	}
	if !duplicate {
		t.Fatal("expected the active same-hotkey orchestrator to classify as a duplicate")
	}
}

func TestHasActiveDuplicateControlSessionRequiresValidMatchingIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	duplicate, err := hasActiveDuplicateControlSession(NATSConfig{
		PublicAPIURL:   server.URL,
		Hotkey:         "hotkey-1",
		User:           "hotkey-1",
		Password:       "invalid-key",
		RequestTimeout: time.Second,
	})
	if err == nil {
		t.Fatal("expected invalid credentials to leave the authorization error unclassified")
	}
	if duplicate {
		t.Fatal("invalid credentials must not be reported as a duplicate control session")
	}
}

func TestNATSAuthorizationErrorRecognition(t *testing.T) {
	if !isNATSAuthorizationError(errors.New("nats: Authorization Violation")) {
		t.Fatal("expected stock NATS authorization error to be recognized")
	}
	if isNATSAuthorizationError(errors.New("connection refused")) {
		t.Fatal("transport failures must not be treated as authorization rejections")
	}
}
