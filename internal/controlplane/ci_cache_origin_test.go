package controlplane

import (
	"errors"
	"strings"
	"testing"
)

func TestCICacheOriginCanonicalRealms(t *testing.T) {
	cases := []struct {
		raw, origin, hash string
		loopback          bool
	}{
		{"https://cache.example.com", "https://cache.example.com", "d7a2c78ef18c6d7712d66e426cd7c633d0d543151ea00805410b23944d202211", false},
		{"HTTPS://CACHE.EXAMPLE.COM:0443/", "https://cache.example.com", "d7a2c78ef18c6d7712d66e426cd7c633d0d543151ea00805410b23944d202211", false},
		{"https://cache-staging.example.com/", "https://cache-staging.example.com", "5d4a4117106594d258018ab30f8492ea107e35c5e1775aacfd8edd506c2556c9", false},
		{"https://cache.example.com:8443", "https://cache.example.com:8443", "29eae4a13b91bad0a872bbe4684b4604c4546aaad3f71bf1c040368797117b73", false},
		{"http://127.0.0.1:8080/", "http://127.0.0.1:8080", "a61a3d913c1cb864d0325fb977b8bdd41598fd9cb701c08dab8534b262e9da63", true},
		{"http://[::ffff:127.0.0.1]:8080", "http://127.0.0.1:8080", "a61a3d913c1cb864d0325fb977b8bdd41598fd9cb701c08dab8534b262e9da63", true},
		{"https://[2001:0db8:0:0:0:0:0:1]:443/", "https://[2001:db8::1]", "a26d2992b2738bf482c74c219be8e40e48ee716920a0af0ff192c4e4731a8af7", false},
		{"http://[::1]:8080", "http://[::1]:8080", "2edd7508d9d545d409650be0d678d38e304b22be1ea4de6dae1f0255696b0b67", true},
	}
	for _, tc := range cases {
		origin, audience, err := ciCacheAudienceForURL(tc.raw, tc.loopback)
		if err != nil || origin != tc.origin || audience != "urn:canter:ci:cache:v1:"+tc.hash {
			t.Errorf("origin %q: got %q, %q, %v", tc.raw, origin, audience, err)
		}
	}
}

func TestCICacheOriginRejectsInvalid(t *testing.T) {
	for _, raw := range []string{
		"", "cache.example.com", "https:cache.example.com", "//cache.example.com",
		" https://cache.example.com", "https://cache.example.com\n",
		"https://cache.example.com/a", "https://cache.example.com//",
		"https://cache.example.com/%2f", "https://cache.example.com?",
		"https://cache.example.com?key=x", "https://cache.example.com#",
		"https://cache.example.com/#part", "https://user:password@cache.example.com",
		"https://@cache.example.com", "https://cache.example.com:",
		"https://cache.example.com:0", "https://cache.example.com:65536",
		"https://cache.example.com:bad", "https://cache..example.com",
		"https://cache.example.com.", "https://-cache.example.com",
		"https://cache-.example.com", "https://cache_example.com",
		"https://" + strings.Repeat("a", 64) + ".example.com",
		"https://caf\xc3\xa9.example.com", "https://[fe80::1%25en0]",
		"http://cache.example.com", "http://localhost:8080",
		"http://192.168.1.1:8080", "http://127.000.0.1:8080",
		"ftp://127.0.0.1:8080", "https://cache.example.com\\@evil.example",
	} {
		for _, allow := range []bool{false, true} {
			origin, audience, err := ciCacheAudienceForURL(raw, allow)
			if !errors.Is(err, errCICacheOrigin) || origin != "" || audience != "" {
				t.Errorf("invalid origin %q (loopback=%v): got %q, %q, %v", raw, allow, origin, audience, err)
			}
		}
	}
	for _, raw := range []string{"http://127.0.0.1:8080", "http://[::1]:8080"} {
		if _, _, err := ciCacheAudienceForURL(raw, false); !errors.Is(err, errCICacheOrigin) {
			t.Errorf("plaintext loopback without explicit opt-in: %q", raw)
		}
	}
}
