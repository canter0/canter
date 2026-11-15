package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func ciHealthTestServer(t *testing.T, e *ciEnforcerEngine, token []byte) *httptest.Server {
	t.Helper()
	srv := &CIEnforcerServer{engine: e, token: token}
	srv.handler = srv.routes()
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)
	return httpSrv
}

func ciHealthToken() []byte { return []byte(strings.Repeat("h", 40)) }

func ciHealthGet(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/v1/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestCIEnforcerHealthEndpoint(t *testing.T) {
	_, _, github := newGitHubFixtureRepo(t, 123, 789, "financial-tests", "repo")
	fc := newFakeCICompute()
	e, dir := newTestEngine(t, fc, github)
	token := ciHealthToken()
	srv := ciHealthTestServer(t, e, token)

	t.Run("schema and app id", func(t *testing.T) {
		resp := ciHealthGet(t, srv.URL, string(token))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("health: %d", resp.StatusCode)
		}
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		if len(raw) != 3 || raw["version"] == nil || raw["running"] == nil || raw["githubAppId"] == nil {
			t.Fatalf("health DTO keys: %v", raw)
		}
		var health ciEnforcerHealth
		if err := json.Unmarshal(mustJSON(t, raw), &health); err != nil {
			t.Fatal(err)
		}
		if health.Version != 1 || health.Running || health.GitHubAppID != 42 {
			t.Fatalf("unexpected health: %+v", health)
		}
	})

	t.Run("auth required", func(t *testing.T) {
		for _, tok := range []string{"", "wrong-token-value-0123456789abcdef", string(token) + "x"} {
			resp := ciHealthGet(t, srv.URL, tok)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("token %q: %d", tok, resp.StatusCode)
			}
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		// Unauthorized wrong-method requests still fail auth first.
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/health", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthorized POST: %d", resp.StatusCode)
		}
		req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/health", nil)
		req.Header.Set("Authorization", "Bearer "+string(token))
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("authorized POST: %d", resp.StatusCode)
		}
	})

	t.Run("no journal mutation", func(t *testing.T) {
		before := ciHealthDirSnapshot(t, dir)
		resp := ciHealthGet(t, srv.URL, string(token))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("health: %d", resp.StatusCode)
		}
		after := ciHealthDirSnapshot(t, dir)
		if strings.Join(before, "|") != strings.Join(after, "|") {
			t.Fatalf("state dir mutated: %v -> %v", before, after)
		}
	})

	t.Run("running transitions", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client, err := newCIEnforcerClient(srv.URL, token, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		go e.Run(ctx)
		deadline := time.Now().Add(5 * time.Second)
		for {
			h, err := client.health(context.Background())
			if err == nil && h.Running {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("engine never reported running")
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		deadline = time.Now().Add(5 * time.Second)
		for {
			h, err := client.health(context.Background())
			if err == nil && !h.Running {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("engine still reported running after cancel")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run("closed reports not running", func(t *testing.T) {
		e2, _ := newTestEngine(t, newFakeCICompute(), nil)
		srv2 := ciHealthTestServer(t, e2, token)
		client2, err := newCIEnforcerClient(srv2.URL, token, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go e2.Run(ctx)
		deadline := time.Now().Add(5 * time.Second)
		for {
			h, err := client2.health(context.Background())
			if err == nil && h.Running {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("engine never reported running")
			}
			time.Sleep(20 * time.Millisecond)
		}
		e2.close()
		deadline = time.Now().Add(5 * time.Second)
		for {
			h, err := client2.health(context.Background())
			if err == nil && !h.Running {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("closed engine still reported running")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run("nil github reports app id zero", func(t *testing.T) {
		e3, _ := newTestEngine(t, newFakeCICompute(), nil)
		srv3 := ciHealthTestServer(t, e3, token)
		resp := ciHealthGet(t, srv3.URL, string(token))
		defer resp.Body.Close()
		var health ciEnforcerHealth
		if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
			t.Fatal(err)
		}
		if health.GitHubAppID != 0 {
			t.Fatalf("expected app id 0, got %d", health.GitHubAppID)
		}
	})
}

func mustJSON(t *testing.T, m map[string]json.RawMessage) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ciHealthDirSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e.Name()+":"+info.Mode().String()+":"+info.ModTime().UTC().Format(time.RFC3339Nano))
	}
	sort.Strings(out)
	return out
}

func TestCIEnforcerHealthClient(t *testing.T) {
	token := ciHealthToken()

	t.Run("happy path", func(t *testing.T) {
		_, _, github := newGitHubFixtureRepo(t, 123, 789, "financial-tests", "repo")
		e, _ := newTestEngine(t, newFakeCICompute(), github)
		srv := ciHealthTestServer(t, e, token)
		client, err := newCIEnforcerClient(srv.URL, token, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		h, err := client.health(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if h.Version != 1 || h.Running || h.GitHubAppID != 42 {
			t.Fatalf("unexpected health: %+v", h)
		}
	})

	cases := []struct {
		name string
		body string
	}{
		{"wrong version", `{"version":2,"running":true,"githubAppId":42}`},
		{"version string", `{"version":"1","running":true,"githubAppId":42}`},
		{"duplicate keys", `{"version":1,"version":1,"running":true,"githubAppId":42}`},
		{"unknown key", `{"version":1,"running":true,"githubAppId":42,"extra":1}`},
		{"missing key", `{"version":1,"running":true}`},
		{"null field", `{"version":1,"running":null,"githubAppId":42}`},
		{"running wrong type", `{"version":1,"running":"yes","githubAppId":42}`},
		{"malformed", `not json`},
		{"trailing data", `{"version":1,"running":true,"githubAppId":42} trailing`},
		{"array root", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			client, err := newCIEnforcerClient(srv.URL, token, nil, true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.health(context.Background()); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}

	t.Run("redirect refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/v1/health", http.StatusFound)
		}))
		t.Cleanup(srv.Close)
		client, err := newCIEnforcerClient(srv.URL, token, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.health(context.Background()); err == nil {
			t.Fatal("expected redirect to fail closed")
		}
	})

	t.Run("non-200 fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		client, err := newCIEnforcerClient(srv.URL, token, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.health(context.Background())
		if err == nil || strings.Contains(err.Error(), string(token)) {
			t.Fatalf("expected sanitized error, got %v", err)
		}
	})

	t.Run("url policy", func(t *testing.T) {
		for _, u := range []string{
			"http://example.com",
			"http://127.0.0.1:8080",
			"http://203.0.113.10:8080",
			"https://user:pw@example.com",
			"https://example.com?x=1",
			"notaurl",
		} {
			if _, err := newCIEnforcerClient(u, token, nil, false); err == nil {
				t.Fatalf("expected rejection for %q", u)
			}
		}
		// Loopback HTTP is allowed only with the explicit private seam.
		if _, err := newCIEnforcerClient("http://127.0.0.1:8080", token, nil, true); err != nil {
			t.Fatalf("loopback http with seam: %v", err)
		}
		if _, err := newCIEnforcerClient("http://203.0.113.10:8080", token, nil, true); err == nil {
			t.Fatal("non-loopback http must be rejected even with seam")
		}
		// Short tokens rejected.
		if _, err := newCIEnforcerClient("https://enforcer.example.com", []byte("short"), nil, false); err == nil {
			t.Fatal("short token must be rejected")
		}
	})
}
