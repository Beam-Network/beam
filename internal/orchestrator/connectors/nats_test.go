package connectors

import (
	"context"
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
			_, _ = writer.Write([]byte(
				`{"hotkey":"hotkey-1","current_key_role":"orchestrator","orchestrator_status":"active"}`))
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

func TestHasActiveDuplicateControlSessionIgnoresInactiveOrchestrator(t *testing.T) {
	// An orchestrator that owns the slot but is offline is not a duplicate session.
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Api-Key") != "orchestrator-api-key" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/auth/me" {
			_, _ = writer.Write([]byte(
				`{"hotkey":"hotkey-1","current_key_role":"orchestrator","orchestrator_status":"inactive"}`))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
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
	if duplicate {
		t.Fatal("an inactive orchestrator must not be reported as a duplicate control session")
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

func TestDuplicateControlSessionStartupHandoff(t *testing.T) {
	wanted := &natsConnector{}
	calls := 0
	session, err := retryDuplicateControlSession(context.Background(), time.Second, time.Millisecond,
		func() (*natsConnector, error) {
			calls++
			if calls < 3 {
				return nil, errDuplicateControlSession
			}
			return wanted, nil
		})
	if err != nil || session != wanted || calls != 3 {
		t.Fatalf("handoff did not connect after predecessor closed: calls=%d err=%v", calls, err)
	}
}

func TestDuplicateControlSessionStartupRemainsBounded(t *testing.T) {
	calls := 0
	_, err := retryDuplicateControlSession(context.Background(), 5*time.Millisecond, time.Millisecond,
		func() (*natsConnector, error) {
			calls++
			return nil, errDuplicateControlSession
		})
	if !errors.Is(err, errDuplicateControlSession) || calls < 1 {
		t.Fatalf("persistent duplicate was not rejected: calls=%d err=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = retryDuplicateControlSession(ctx, time.Second, time.Millisecond,
		func() (*natsConnector, error) {
			cancel()
			return nil, errDuplicateControlSession
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown did not stop handoff wait: %v", err)
	}
	configurationError := errors.New("invalid NATS credentials")
	calls = 0
	_, err = retryDuplicateControlSession(context.Background(), time.Second, time.Millisecond,
		func() (*natsConnector, error) {
			calls++
			return nil, configurationError
		})
	if !errors.Is(err, configurationError) || calls != 1 {
		t.Fatalf("unrelated configuration error was retried: calls=%d err=%v", calls, err)
	}
}
