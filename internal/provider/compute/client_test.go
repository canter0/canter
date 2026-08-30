package compute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeImage(t *testing.T) {
	if normalizeImage("Ubuntu-24.04-amd64") != normalizeImage("ubuntu-24.04") {
		t.Fatal("public image alias does not resolve to the provider-neutral form")
	}
}

func TestDeleteSecurityPolicyTreatsNotFoundAsDeleted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v2.0/security-groups/already-gone" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, `{"NeutronError":{"type":"SecurityGroupNotFound"}}`, http.StatusNotFound)
	}))
	defer server.Close()

	client := &Client{http: server.Client(), session: session{Token: "test", NetworkURL: server.URL, Expires: time.Now().Add(time.Hour)}}
	if err := client.DeleteSecurityPolicy(context.Background(), "already-gone"); err != nil {
		t.Fatalf("idempotent delete failed: %v", err)
	}
}

func TestDeleteSecurityPolicyRetriesConflict(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "still attached", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := &Client{http: server.Client(), session: session{Token: "test", NetworkURL: server.URL, Expires: time.Now().Add(time.Hour)}}
	if err := client.DeleteSecurityPolicy(context.Background(), "eventually-gone"); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("got %d attempts, want 2", attempts.Load())
	}
}
