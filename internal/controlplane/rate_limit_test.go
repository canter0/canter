package controlplane

import (
	"fmt"
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

func TestRequestIPUsesProxyAppendedPeerRatherThanClientXFFPrefix(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/auth/signin", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.77, 203.0.113.8")
	if got := requestIP(request); got != "203.0.113.8" {
		t.Fatalf("proxy-appended peer=%q", got)
	}
}

func TestRequestLimiterBoundsHighCardinalityKeys(t *testing.T) {
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	limiter := newRequestLimiter()
	limiter.now = func() time.Time { return now }
	limit := requestLimit{bucket: "auth", max: 2, window: time.Minute}
	for i := 0; i < maxRequestWindows; i++ {
		if ok, _ := limiter.allow(fmt.Sprintf("auth|192.0.2.%d", i), limit); !ok {
			t.Fatalf("request %d was rejected before capacity", i)
		}
	}
	if ok, _ := limiter.allow("auth|198.51.100.1", limit); ok {
		t.Fatal("new key admitted beyond capacity")
	}
	if got := len(limiter.windows); got != maxRequestWindows {
		t.Fatalf("window count=%d, want %d", got, maxRequestWindows)
	}
	if ok, _ := limiter.allow("auth|192.0.2.0", limit); !ok {
		t.Fatal("existing client's remaining budget was rejected at capacity")
	}
	if ok, _ := limiter.allow("auth|192.0.2.0", limit); ok {
		t.Fatal("existing client's exhausted budget was accepted at capacity")
	}
	now = now.Add(time.Minute)
	if ok, _ := limiter.allow("auth|198.51.100.1", limit); !ok {
		t.Fatal("expired keys did not free capacity")
	}
	if got := len(limiter.windows); got != 1 {
		t.Fatalf("window count after expiry=%d, want 1", got)
	}
}

func TestRequestIPRejectsMalformedProxyAppendedAddress(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/auth/signin", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.77, malformed")
	if got := requestIP(request); got != "127.0.0.1" {
		t.Fatalf("malformed proxy suffix allowed client-controlled prefix: %q", got)
	}
}
