package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
			_ = json.NewEncoder(w).Encode(map[string]any{"protocolVersion": "v1", "installation": map[string]any{"id": "agt_1"}, "session": map[string]any{"id": "ass_1"}, "workspace": map[string]any{"id": "wrk_1"}, "systems": []any{}, "changes": []any{map[string]any{"id": "change_1"}}, "changesHasMore": true, "changesNextCursor": "changes-cursor", "pendingChanges": []any{map[string]any{"id": "change_pending"}}, "pendingChangesHasMore": true, "pendingChangesNextCursor": "pending-cursor", "initialDeployments": []any{map[string]any{"id": "deploy_1"}}, "initialDeploymentsHasMore": true, "initialDeploymentsNextCursor": "deployments-cursor", "incidents": []any{}})
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
	if err != nil || bootstrap.ProtocolVersion != "v1" || len(bootstrap.Changes) != 1 || string(bootstrap.Changes[0]) != `{"id":"change_1"}` || !bootstrap.ChangesHasMore || bootstrap.ChangesNextCursor != "changes-cursor" || !bootstrap.PendingChangesHasMore || bootstrap.PendingChangesNextCursor != "pending-cursor" || !bootstrap.InitialDeploymentsHasMore || bootstrap.InitialDeploymentsCursor != "deployments-cursor" {
		t.Fatalf("bootstrap: %#v %v", bootstrap, err)
	}
	serialized, err := json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"changes"`, `"changesHasMore"`, `"changesNextCursor"`, `"pendingChangesHasMore"`, `"pendingChangesNextCursor"`, `"initialDeploymentsHasMore"`, `"initialDeploymentsNextCursor"`} {
		if !strings.Contains(string(serialized), field) {
			t.Fatalf("remote bootstrap serialization dropped %s: %s", field, serialized)
		}
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

func TestClientRejectsOversizedJSONResponseEvenWhenTruncatedPrefixParses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"installation":{"id":"agt_1"},"session":{"id":"ass_1"}}`)
		_, _ = w.Write([]byte(strings.Repeat(" ", maxResponseBytes)))
	}))
	defer server.Close()
	client, _ := New(server.URL, server.Client())

	if _, err := client.WhoAmI(context.Background(), "access"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized valid-JSON prefix was accepted or rejected for the wrong reason: %v", err)
	}
}

func TestPollDeviceStopsAfterAuthorizationExpires(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusPreconditionRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"pending"}}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, server.Client())

	_, err := client.PollDevice(context.Background(), DeviceAuthorization{
		DeviceCode: "device", ExpiresAt: time.Now().Add(time.Second), IntervalSeconds: 60,
	}, "task")
	if !errors.Is(err, ErrAuthorizationExpired) {
		t.Fatalf("expected expired authorization, got %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("made %d token requests; expected one before expiry", requests.Load())
	}
}

func TestPollDeviceDoesNotRequestAlreadyExpiredAuthorization(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	client, _ := New(server.URL, server.Client())

	_, err := client.PollDevice(context.Background(), DeviceAuthorization{
		DeviceCode: "device", ExpiresAt: time.Now().Add(-time.Second), IntervalSeconds: 1,
	}, "task")
	if !errors.Is(err, ErrAuthorizationExpired) {
		t.Fatalf("expected expired authorization, got %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("made %d token requests for expired authorization", requests.Load())
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

func TestRouteIdentifiersCannotChangePathSegments(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		call func() error
	}{
		{"upload workspace dot segment", func() error {
			_, err := client.UploadArtifact(context.Background(), "access", "..", "file", "application/octet-stream", strings.NewReader("x"))
			return err
		}},
		{"draft workspace slash", func() error {
			_, err := client.DraftInitialDeployment(context.Background(), "access", "wrk/other", DraftInitialDeploymentInput{})
			return err
		}},
		{"list workspace control", func() error {
			_, err := client.ListInitialDeployments(context.Background(), "access", "wrk\nother")
			return err
		}},
		{"inspect deployment dot segment", func() error {
			_, err := client.InspectInitialDeployment(context.Background(), "access", "wrk_1", "..")
			return err
		}},
		{"inspect execution backslash", func() error {
			_, err := client.InspectInitialDeploymentExecution(context.Background(), "access", `exec\other`)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Fatal("invalid route identifier was accepted")
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("sent %d requests with invalid route identifiers", got)
	}
}
