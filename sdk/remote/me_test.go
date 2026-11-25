package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientMeSendsPersonalKeyAndDecodesIdentity(t *testing.T) {
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account":            map[string]any{"id": "usr_1", "email": "a@example.com", "createdAt": time.Now().UTC().Format(time.RFC3339)},
			"workspaces":         []map[string]any{{"id": "wrk_1", "name": "default", "revision": 3, "role": "owner", "agentAuthority": map[string]any{"inspect": true, "draft": true, "applyMode": "human-approval-required"}}},
			"onboardingComplete": true,
		})
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := client.Me(context.Background(), "canter_pk_synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/me" || gotAuth != "Bearer canter_pk_synthetic" {
		t.Fatalf("request %s %q", gotPath, gotAuth)
	}
	if identity.Account.ID != "usr_1" || identity.Account.Email != "a@example.com" || len(identity.Workspaces) != 1 || identity.Workspaces[0].Role != "owner" || !identity.OnboardingComplete {
		t.Fatalf("identity %+v", identity)
	}
}

func TestClientMeSurfacesAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Me(context.Background(), "canter_pk_bad")
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("error %v", err)
	}
}
