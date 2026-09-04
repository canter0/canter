package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestLimiterRejectsAndResets(t *testing.T) {
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	limiter := newRequestLimiter()
	limiter.now = func() time.Time { return now }
	limit := requestLimit{bucket: "auth", max: 2, window: time.Minute}
	if ok, _ := limiter.allow("auth|127.0.0.1", limit); !ok {
		t.Fatal("first request was rejected")
	}
	if ok, _ := limiter.allow("auth|127.0.0.1", limit); !ok {
		t.Fatal("second request was rejected")
	}
	if ok, retry := limiter.allow("auth|127.0.0.1", limit); ok || retry != time.Minute {
		t.Fatalf("third request ok=%t retry=%s", ok, retry)
	}
	now = now.Add(time.Minute)
	if ok, _ := limiter.allow("auth|127.0.0.1", limit); !ok {
		t.Fatal("request remained blocked after its window")
	}
}

func TestRequestIPTrustsForwardingOnlyFromLoopback(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/auth/signin", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.8")
	if got := requestIP(request); got != "203.0.113.8" {
		t.Fatalf("loopback proxy address=%q", got)
	}
	request.RemoteAddr = "198.51.100.7:1234"
	if got := requestIP(request); got != "198.51.100.7" {
		t.Fatalf("public peer spoofed forwarded address: %q", got)
	}
}
