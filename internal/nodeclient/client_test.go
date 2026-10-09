package nodeclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/canter0/canter/sdk"
)

func TestClientUsesScopedBearerAndTypedRoutes(t *testing.T) {
	var observed sdk.ObservedRelease
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cn_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/node/snapshot":
			_ = json.NewEncoder(w).Encode(sdk.NodeSnapshot{SchemaVersion: "v1", System: "api", Generation: "one"})
		case "/v1/node/artifacts/abc":
			_, _ = w.Write([]byte("artifact"))
		case "/v1/node/observed":
			_ = json.NewDecoder(r.Body).Decode(&observed)
			_ = json.NewEncoder(w).Encode(map[string]bool{"accepted": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "node.token")
	if err := os.WriteFile(tokenFile, []byte("cn_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewWithHTTPClient(server.URL, tokenFile, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(t.Context())
	if err != nil || snapshot.System != "api" {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	b, err := client.Artifact(t.Context(), "abc")
	if err != nil || string(b) != "artifact" {
		t.Fatalf("artifact=%q err=%v", b, err)
	}
	if err := client.PutObserved(t.Context(), sdk.ObservedRelease{SchemaVersion: "v1", System: "api"}); err != nil {
		t.Fatal(err)
	}
	if observed.System != "api" {
		t.Fatalf("observed=%#v", observed)
	}
}

func TestClientRejectsPlainHTTP(t *testing.T) {
	if _, err := New("http://127.0.0.1:8081", "/tmp/token"); err == nil {
		t.Fatal("plain HTTP gateway was accepted")
	}
}

func TestClientRefusesRedirectWithoutMutatingCallerClient(t *testing.T) {
	targetRequests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/capture" {
			targetRequests++
			if r.Header.Get("Authorization") == "Bearer cn_test" {
				t.Error("same-origin redirect forwarded node credential")
			}
			_, _ = w.Write([]byte(`{"schemaVersion":"v1"}`))
			return
		}
		http.Redirect(w, r, "/capture", http.StatusFound)
	}))
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "node.token")
	if err := os.WriteFile(tokenFile, []byte("cn_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	callerClient := server.Client()
	client, err := NewWithHTTPClient(server.URL, tokenFile, callerClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Snapshot(t.Context()); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirect response err=%v, want HTTP 302", err)
	}
	if targetRequests != 0 {
		t.Fatalf("redirect target received %d requests", targetRequests)
	}
	// The supplied client retains its normal redirect behavior after construction.
	resp, err := callerClient.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if targetRequests != 1 {
		t.Fatalf("caller client redirect behavior changed: target requests=%d", targetRequests)
	}
}

func TestExchangeEnrollmentRejectsUnsafeURLAndID(t *testing.T) {
	for _, gatewayURL := range []string{"https://user:pass@example.test", "https://example.test/path?query=1", "https://example.test/path#frag"} {
		if _, err := ExchangeEnrollment(context.Background(), gatewayURL, "nen_valid", "en_test", nil); err == nil {
			t.Errorf("accepted unsafe gateway URL %q", gatewayURL)
		}
	}
	for _, id := range []string{"", "../other", "nested/id", "x?y", "x#y"} {
		if _, err := ExchangeEnrollment(context.Background(), "https://example.test/base", id, "en_test", nil); err == nil {
			t.Errorf("accepted unsafe enrollment ID %q", id)
		}
	}
}

func TestExchangeEnrollmentRejectsRedirectAndOversizedResponse(t *testing.T) {
	targetRequests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/capture" {
			targetRequests++
			if r.Header.Get("Authorization") == "Bearer en_test" {
				t.Error("same-origin redirect forwarded enrollment credential")
			}
			_, _ = w.Write([]byte(`{"nodeToken":"cn_target"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer en_test" {
			t.Error("missing enrollment credential")
		}
		http.Redirect(w, r, "/capture", http.StatusFound)
	}))
	defer server.Close()
	response, err := ExchangeEnrollment(t.Context(), server.URL, "nen_valid", "en_test", server.Client())
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirect result=%#v err=%v, want HTTP 302", response, err)
	}
	if targetRequests != 0 {
		t.Fatalf("redirect target received %d requests", targetRequests)
	}

	oversized := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"nodeToken":"cn_ok"}` + strings.Repeat(" ", 64<<10)))
	}))
	defer oversized.Close()
	if _, err := ExchangeEnrollment(t.Context(), oversized.URL, "nen_valid", "en_test", oversized.Client()); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("oversized response err=%v", err)
	}
}

func TestTokenRejectsSpecialAndOversizedFiles(t *testing.T) {
	client, err := New("https://example.test", filepath.Join(t.TempDir(), "token"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(client.tokenFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := client.token(); err == nil {
		t.Fatal("directory credential was accepted")
	}
	if err := os.Remove(client.tokenFile); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(client.tokenFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.token(); err == nil {
		t.Fatal("FIFO credential was accepted")
	}
	if err := os.Remove(client.tokenFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(client.tokenFile, []byte("cn_"+strings.Repeat("a", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.token(); err == nil {
		t.Fatalf("oversized credential err=%v", err)
	}
}
