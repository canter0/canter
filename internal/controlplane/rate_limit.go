package controlplane

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type requestLimit struct {
	bucket string
	max    int
	window time.Duration
}

type requestWindow struct {
	started time.Time
	count   int
}

type requestLimiter struct {
	mu      sync.Mutex
	windows map[string]requestWindow
	now     func() time.Time
}

func newRequestLimiter() *requestLimiter {
	return &requestLimiter{windows: make(map[string]requestWindow), now: time.Now}
}

func (h *HTTPServer) allowRequest(w http.ResponseWriter, r *http.Request) bool {
	limit, ok := limitForRequest(r)
	if !ok {
		return true
	}
	key := limit.bucket + "|" + requestIP(r)
	allowed, retryAfter := h.limits.allow(key, limit)
	if allowed {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter.Seconds()))))
	writeError(w, http.StatusTooManyRequests, ErrCapacity)
	return false
}

func limitForRequest(r *http.Request) (requestLimit, bool) {
	if r.Method != http.MethodPost {
		return requestLimit{}, false
	}
	switch {
	case r.URL.Path == "/v1/auth/signup" || r.URL.Path == "/v1/auth/signin":
		return requestLimit{bucket: "auth", max: 10, window: 10 * time.Minute}, true
	case r.URL.Path == "/v1/device/authorizations":
		return requestLimit{bucket: "device", max: 30, window: 10 * time.Minute}, true
	case r.URL.Path == "/v1/agent/token/refresh":
		return requestLimit{bucket: "refresh", max: 60, window: time.Minute}, true
	case r.URL.Path == "/mcp":
		return requestLimit{bucket: "mcp", max: 600, window: time.Minute}, true
	case strings.HasSuffix(r.URL.Path, "/artifacts"):
		return requestLimit{bucket: "artifact", max: 30, window: time.Hour}, true
	default:
		return requestLimit{}, false
	}
}

func (l *requestLimiter) allow(key string, limit requestLimit) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	current := l.windows[key]
	if current.started.IsZero() || now.Sub(current.started) >= limit.window {
		l.windows[key] = requestWindow{started: now, count: 1}
		return true, 0
	}
	if current.count >= limit.max {
		return false, limit.window - now.Sub(current.started)
	}
	current.count++
	l.windows[key] = current
	if len(l.windows) > 10_000 {
		for candidate, window := range l.windows {
			if now.Sub(window.started) >= time.Hour {
				delete(l.windows, candidate)
			}
		}
	}
	return true, 0
}

func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer != nil && peer.IsLoopback() {
		forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
		if ip := net.ParseIP(forwarded); ip != nil {
			return ip.String()
		}
	}
	if peer != nil {
		return peer.String()
	}
	return "unknown"
}
