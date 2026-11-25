package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsEndpointRequiresLoopback(t *testing.T) {
	handler := NewHTTPServer(&Service{}, HTTPConfig{PublicURL: "http://canter.test"})
	for _, test := range []struct {
		name       string
		remoteAddr string
		host       string
		forwarded  string
		want       int
	}{
		{"ipv4 loopback", "127.0.0.1:4567", "127.0.0.1:8081", "", http.StatusOK},
		{"ipv6 loopback", "[::1]:4567", "[::1]:8081", "", http.StatusOK},
		{"localhost", "127.0.0.1:4567", "localhost:8081", "", http.StatusOK},
		{"public peer", "203.0.113.10:4567", "127.0.0.1:8081", "", http.StatusNotFound},
		{"public host", "127.0.0.1:4567", "canter.dev", "", http.StatusNotFound},
		{"forwarded peer", "127.0.0.1:4567", "127.0.0.1:8081", "203.0.113.10", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8081/metrics", nil)
			request.RemoteAddr = test.remoteAddr
			request.Host = test.host
			if test.forwarded != "" {
				request.Header.Set("X-Forwarded-For", test.forwarded)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("metrics status %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestMetricsEndpointRejectsNonGetMethods(t *testing.T) {
	handler := NewHTTPServer(&Service{}, HTTPConfig{PublicURL: "http://canter.test"})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8081/metrics", nil)
	request.RemoteAddr = "127.0.0.1:4567"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("metrics method response %d allow %q", response.Code, response.Header().Get("Allow"))
	}
}

func TestMetricsEndpointExportsNormalizedHTTPMetrics(t *testing.T) {
	handler := NewHTTPServer(&Service{}, HTTPConfig{PublicURL: "http://canter.test"})
	health := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	health.RemoteAddr = "127.0.0.1:4567"
	healthResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthResponse, health)
	if healthResponse.Code != http.StatusOK {
		t.Fatalf("health status %d: %s", healthResponse.Code, healthResponse.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8081/metrics", nil)
	request.RemoteAddr = "127.0.0.1:4567"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status %d: %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("metrics content type %q must not be the API JSON type", contentType)
	}
	body := response.Body.String()
	for _, expected := range []string{
		"canter_http_requests_total",
		`route="/healthz"`,
		`status_class="2xx"`,
		"canter_http_request_duration_seconds_bucket",
		"canter_http_requests_in_flight",
		"go_goroutines",
		"process_resident_memory_bytes",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics response missing %q:\n%s", expected, body)
		}
	}
}

func TestMetricsRouteNormalizationBoundsCardinality(t *testing.T) {
	server := &HTTPServer{config: HTTPConfig{Sandboxes: SandboxConfig{PreviewDomain: "previews.canter.dev"}}}
	for _, test := range []struct {
		name string
		host string
		path string
		want string
	}{
		{"health", "canter.dev", "/healthz", "/healthz"},
		{"workspace family", "canter.dev", "/v1/workspaces/ws_secret/executions/ex_secret", "/v1/workspaces"},
		{"auth family", "canter.dev", "/v1/auth/oauth/google/callback", "/v1/auth"},
		{"message webhook secret", "canter.dev", "/v1/wren-message-webhooks/secret-token", "/v1/wren-message-webhooks"},
		{"unknown v1", "canter.dev", "/v1/enumerated-unknown-value", "/v1/unknown"},
		{"non api", "canter.dev", "/enumerate/me", "other"},
		{"sandbox preview", "abc.previews.canter.dev", "/some/user-controlled/path", "sandbox-preview"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://"+test.host+test.path, nil)
			if got := metricsRoute(server, request); got != test.want {
				t.Fatalf("route %q, want %q", got, test.want)
			}
		})
	}
}

func TestMetricsMethodAndStatusNormalization(t *testing.T) {
	if got := metricsMethod("PROPFIND"); got != "other" {
		t.Fatalf("unknown method normalized to %q", got)
	}
	if got := metricsMethod(http.MethodPatch); got != http.MethodPatch {
		t.Fatalf("known method normalized to %q", got)
	}
	for status, want := range map[int]string{200: "2xx", 302: "3xx", 404: "4xx", 503: "5xx", 99: "unknown", 1000: "unknown"} {
		if got := metricsStatusClass(status); got != want {
			t.Fatalf("status %d normalized to %q, want %q", status, got, want)
		}
	}
}
