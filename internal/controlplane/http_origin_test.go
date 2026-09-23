package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHumanOriginLocalAliases(t *testing.T) {
	for _, test := range []struct {
		name, public, origin string
		secure, allowed      bool
	}{
		{"exact local", "http://127.0.0.1:3000", "http://127.0.0.1:3000", false, true},
		{"localhost alias", "http://127.0.0.1:3000", "http://localhost:3000", false, true},
		{"reverse alias", "http://localhost:3000", "http://127.0.0.1:3000", false, true},
		{"ipv6 alias", "http://127.0.0.1:3000", "http://[::1]:3000", false, true},
		{"other port", "http://127.0.0.1:3000", "http://localhost:3006", false, false},
		{"remote origin", "http://127.0.0.1:3000", "http://evil.test:3000", false, false},
		{"hostname suffix", "http://127.0.0.1:3000", "http://localhost.evil.test:3000", false, false},
		{"missing origin", "http://127.0.0.1:3000", "", false, false},
		{"null origin", "http://127.0.0.1:3000", "null", false, false},
		{"userinfo", "http://127.0.0.1:3000", "http://user@127.0.0.1:3000", false, false},
		{"secure cookies", "http://127.0.0.1:3000", "http://localhost:3000", true, false},
		{"https local", "https://127.0.0.1:3000", "https://localhost:3000", false, false},
		{"production exact", "https://canter.dev", "https://canter.dev", true, true},
		{"production alias", "https://canter.dev", "https://localhost", true, false},
		{"production http alias", "http://canter.dev:3000", "http://localhost:3000", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHTTPServer(&Service{}, HTTPConfig{PublicURL: test.public, CookieSecure: test.secure})
			// Invalid JSON stops before any database access. An accepted origin
			// reaches validation (400), while a rejected origin stops at 403.
			request := httptest.NewRequest(http.MethodPost, "/v1/auth/signin", strings.NewReader(`{"_origin_probe":true}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", test.origin)
			cookieName := "canter_session"
			if test.secure {
				cookieName = "__Host-canter_session"
			}
			request.AddCookie(&http.Cookie{Name: cookieName, Value: "origin-probe"})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			want := http.StatusForbidden
			if test.allowed {
				want = http.StatusBadRequest
			}
			if response.Code != want {
				t.Fatalf("origin %q: got %d, want %d: %s", test.origin, response.Code, want, response.Body.String())
			}
		})
	}
}
