package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientDeviceIdentityAndBootstrap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/device/authorizations":
			_ = json.NewEncoder(w).Encode(DeviceAuthorization{DeviceCode: "device", UserCode: "ABCD-EFGH", VerificationURI: "https://canter.test/authorize", ExpiresAt: time.Now().Add(time.Minute), IntervalSeconds: 1})
		case "/v1/device/token":
			_ = json.NewEncoder(w).Encode(TokenPair{AccessToken: "access", RefreshToken: "refresh", Installation: Installation{ID: "agt_1"}})
		case "/v1/agent/whoami":
			if r.Header.Get("Authorization") != "Bearer access" {
				t.Fatal("missing bearer token")
			}
			_ = json.NewEncoder(w).Encode(Identity{Installation: Installation{ID: "agt_1"}, Session: Session{ID: "ass_1"}})
		case "/v1/agent/bootstrap":
			_ = json.NewEncoder(w).Encode(map[string]any{"protocolVersion": "v1", "installation": map[string]any{"id": "agt_1"}, "session": map[string]any{"id": "ass_1"}, "workspace": map[string]any{"id": "wrk_1"}, "systems": []any{}, "pendingChanges": []any{}, "incidents": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	device, err := client.BeginDeviceAuthorization(context.Background(), BeginInput{Name: "Codex", Harness: "codex", Authority: Authority{Inspect: true, Draft: true, ApplyMode: "human-approval-required"}})
	if err != nil || device.DeviceCode != "device" {
		t.Fatalf("begin: %#v %v", device, err)
	}
	pair, err := client.ExchangeDevice(context.Background(), device.DeviceCode, "task")
	if err != nil || pair.AccessToken != "access" {
		t.Fatalf("exchange: %#v %v", pair, err)
	}
	identity, err := client.WhoAmI(context.Background(), pair.AccessToken)
	if err != nil || identity.Installation.ID != "agt_1" {
		t.Fatalf("whoami: %#v %v", identity, err)
	}
	bootstrap, err := client.Bootstrap(context.Background(), pair.AccessToken)
	if err != nil || bootstrap.ProtocolVersion != "v1" {
		t.Fatalf("bootstrap: %#v %v", bootstrap, err)
	}
}

func TestClientRecognizesPendingDeviceAuthorization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"pending"}}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, server.Client())
	_, err := client.ExchangeDevice(context.Background(), "device", "task")
	if !errors.Is(err, ErrAuthorizationPending) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClientRejectsCredentialBearingBaseURL(t *testing.T) {
	if _, err := New("https://user:pass@canter.test", nil); err == nil {
		t.Fatal("credential-bearing URL was accepted")
	}
}

func TestClientRejectsCleartextOutsideLoopback(t *testing.T) {
	if _, err := New("http://canter.test", nil); err == nil {
		t.Fatal("public cleartext API URL was accepted")
	}
	for _, raw := range []string{"http://localhost:8081", "http://127.0.0.1:8081", "http://[::1]:8081"} {
		if _, err := New(raw, nil); err != nil {
			t.Fatalf("loopback development URL %q was rejected: %v", raw, err)
		}
	}
}

func TestClientDoesNotFollowCredentialBearingRedirects(t *testing.T) {
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		if r.Header.Get("Authorization") != "" {
			t.Fatal("bearer credential reached redirect target")
		}
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client, err := New(source.URL, source.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.WhoAmI(context.Background(), "sensitive-access-token"); err == nil {
		t.Fatal("redirect response was accepted")
	}
	if redirected {
		t.Fatal("client followed a credential-bearing redirect")
	}
}
