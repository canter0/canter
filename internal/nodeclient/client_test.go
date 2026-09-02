package nodeclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
