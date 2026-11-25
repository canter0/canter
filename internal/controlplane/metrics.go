package controlplane

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/felixge/httpsnoop"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type httpMetrics struct {
	registry *prometheus.Registry
	handler  http.Handler
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

func newHTTPMetrics() *httpMetrics {
	registry := prometheus.NewRegistry()
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "canter",
		Subsystem: "http",
		Name:      "requests_total",
		Help:      "Control-plane HTTP requests partitioned by normalized route, method, and status class.",
	}, []string{"route", "method", "status_class"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "canter",
		Subsystem: "http",
		Name:      "request_duration_seconds",
		Help:      "Control-plane HTTP request duration by normalized route and method.",
		Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{"route", "method"})
	inFlight := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "canter",
		Subsystem: "http",
		Name:      "requests_in_flight",
		Help:      "Control-plane HTTP requests currently being served.",
	})
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		requests,
		duration,
		inFlight,
	)
	return &httpMetrics{
		registry: registry,
		handler:  promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		requests: requests,
		duration: duration,
		inFlight: inFlight,
	}
}

func (m *httpMetrics) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !metricsRemoteAllowed(r) {
		http.NotFound(w, r)
		return
	}
	m.handler.ServeHTTP(w, r)
}

func (m *httpMetrics) observe(w http.ResponseWriter, r *http.Request, route string, next http.Handler) {
	method := metricsMethod(r.Method)
	m.inFlight.Inc()
	defer m.inFlight.Dec()
	captured := httpsnoop.CaptureMetricsFn(w, func(capturedW http.ResponseWriter) {
		next.ServeHTTP(capturedW, r)
	})
	m.requests.WithLabelValues(route, method, metricsStatusClass(captured.Code)).Inc()
	m.duration.WithLabelValues(route, method).Observe(captured.Duration.Seconds())
}

func metricsRemoteAllowed(r *http.Request) bool {
	if r.Header.Get("Forwarded") != "" || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" {
		return false
	}
	if !metricsHostIsLoopback(r.Host) {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func metricsHostIsLoopback(value string) bool {
	host, _, err := net.SplitHostPort(value)
	if err != nil {
		host = value
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func metricsMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
		return method
	default:
		return "other"
	}
}

func metricsStatusClass(status int) string {
	if status < 100 || status > 999 {
		return "unknown"
	}
	return strconv.Itoa(status/100) + "xx"
}

var metricsV1Routes = map[string]struct{}{
	"acquisition":                   {},
	"agent":                         {},
	"agent-pairings":                {},
	"auth":                          {},
	"billing":                       {},
	"change-approvals":              {},
	"ci":                            {},
	"device":                        {},
	"executions":                    {},
	"initial-deployment-executions": {},
	"installations":                 {},
	"me":                            {},
	"node":                          {},
	"wren-extension-events":         {},
	"workspace-invites":             {},
	"workspaces":                    {},
}

func metricsRoute(h *HTTPServer, r *http.Request) string {
	if h.isPreviewHost(r) {
		return "sandbox-preview"
	}
	path := r.URL.Path
	switch path {
	case "/healthz", "/readyz", "/mcp", "/internal/preview-ask", "/v1/wren-model/chat/completions":
		return path
	}
	if strings.HasPrefix(path, "/v1/wren-message-webhooks/") {
		return "/v1/wren-message-webhooks"
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "v1" {
		if _, ok := metricsV1Routes[parts[1]]; ok {
			return "/v1/" + parts[1]
		}
		return "/v1/unknown"
	}
	return "other"
}
