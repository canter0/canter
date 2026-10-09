package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestGatewayNeverExposesHumanOrModelEndpoints(t *testing.T) {
	called := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Header.Get("Authorization") != "Bearer node-test-token" || r.Header.Get("X-Forwarded-Proto") != "https" {
			t.Error("gateway lost node authentication or TLS context")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	handler := nodeGatewayHandler(target)
	for _, path := range []string{"/v1/auth/signup", "/v1/me", "/v1/workspaces/work/conversations", "/mcp", "/v1/nodeevil", "/"} {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("exposed %s: HTTP %d", path, w.Code)
		}
	}
	if called != 0 {
		t.Fatal("a disallowed request reached the control plane")
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/node/nodes/example/heartbeat", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer node-test-token")
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("node request was not forwarded: status=%d calls=%d", w.Code, called)
	}

	// A listener bound to a public interface must not turn an arbitrary
	// network caller's X-Forwarded-Proto into trusted loopback TLS context.
	r = httptest.NewRequest(http.MethodPost, "/v1/node/nodes/example/heartbeat", nil)
	r.RemoteAddr = "198.51.100.20:4321"
	r.Header.Set("Authorization", "Bearer node-test-token")
	r.Header.Set("X-Forwarded-Proto", "https")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || called != 1 {
		t.Fatalf("non-loopback peer crossed proxy trust boundary: status=%d calls=%d", w.Code, called)
	}
}
