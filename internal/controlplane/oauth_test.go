package controlplane

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

func TestOAuthRedirectSafety(t *testing.T) {
	for _, next := range []string{"https://evil.test", "//evil.test", "/\\evil.test", "/%2f%2fevil.test", "/%5cevil.test", "/\r\nLocation: evil", "javascript:alert(1)"} {
		if got := safeOAuthNext(next, "sign-in"); got != "/app" {
			t.Fatalf("unsafe redirect %q => %q", next, got)
		}
	}
	if got := safeOAuthNext("/onboarding/authorize?code=ABCD-EFGH", "sign-in"); got != "/onboarding/authorize?code=ABCD-EFGH" {
		t.Fatal(got)
	}
	if got := safeOAuthNext("", "create-account"); got != "/app?welcome=1" {
		t.Fatal(got)
	}
}

func testGoogleSigner(t *testing.T) (*rsa.PublicKey, func(map[string]any) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(claims map[string]any) string {
		t.Helper()
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT","kid":"test-key"}`))
		raw, _ := json.Marshal(claims)
		body := header + "." + base64.RawURLEncoding.EncodeToString(raw)
		hash := sha256.Sum256([]byte(body))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
		if err != nil {
			t.Fatal(err)
		}
		return body + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	return &key.PublicKey, sign
}

func TestGoogleIdentityValidatesSignedClaims(t *testing.T) {
	key, sign := testGoogleSigner(t)
	verifier := oidc.NewVerifier("https://accounts.google.com", &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{key}}, &oidc.Config{ClientID: "canter-client"})
	for _, tc := range []struct {
		name, key string
		value     any
		valid     bool
	}{
		{name: "valid", valid: true},
		{name: "Google bare issuer", key: "iss", value: "accounts.google.com", valid: true},
		{name: "Google string verified claim", key: "email_verified", value: "true", valid: true},
		{name: "wrong audience", key: "aud", value: "another-client"},
		{name: "wrong issuer", key: "iss", value: "https://evil.test"},
		{name: "expired", key: "exp", value: time.Now().Add(-time.Hour).Unix()},
		{name: "wrong nonce", key: "nonce", value: "other-browser"},
		{name: "missing nonce", key: "nonce", value: ""},
		{name: "unverified email", key: "email_verified", value: false},
		{name: "string unverified email", key: "email_verified", value: "false"},
		{name: "missing verification", key: "email_verified", value: nil},
		{name: "truthy string", key: "email_verified", value: "TRUE"},
		{name: "truthy number", key: "email_verified", value: 1},
		{name: "missing email", key: "email", value: ""},
		{name: "malformed email", key: "email", value: 1},
		{name: "missing subject", key: "sub", value: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := map[string]any{"iss": "https://accounts.google.com", "aud": "canter-client", "sub": "stable-subject", "email": "user@example.com", "email_verified": true, "nonce": "browser-nonce", "exp": time.Now().Add(time.Hour).Unix()}
			if tc.key != "" {
				claims[tc.key] = tc.value
			}
			token := (&oauth2.Token{}).WithExtra(map[string]any{"id_token": sign(claims)})
			identity, err := googleIdentity(context.Background(), verifier, token, "browser-nonce")
			if tc.valid {
				if err != nil || identity.Subject != "stable-subject" {
					t.Fatalf("%+v %v", identity, err)
				}
			} else if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("invalid claims accepted or unexpected error: %v", err)
			}
		})
	}
}

func TestGoogleSigningKeyHTTPClientUsesIPv4AndRejectsRedirects(t *testing.T) {
	collectorCalls := 0
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		collectorCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, collector.URL, http.StatusFound)
			return
		}
		fmt.Fprint(w, `{"keys":[]}`)
	}))
	defer source.Close()
	client := newGoogleSigningKeyHTTPClient()
	defer client.CloseIdleConnections()
	response, err := client.Get(source.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusFound || collectorCalls != 0 {
		t.Fatal("signing key fetch followed a redirect")
	}
	response, err = client.Get(source.URL)
	if err != nil {
		t.Fatal("IPv4 key endpoint is unavailable", err)
	}
	response.Body.Close()
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("IPv6 loopback is unavailable")
	}
	defer listener.Close()
	// The key client must not dial the IPv6 route that returns 403 in production.
	if conn, err := client.Transport.(*http.Transport).DialContext(context.Background(), "tcp", listener.Addr().String()); err == nil {
		conn.Close()
		t.Fatal("signing key client dialed IPv6")
	}
}

func TestGitHubIdentityRequiresVerifiedPrimaryEmail(t *testing.T) {
	verified := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing access token")
		}
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"id":123,"email":"untrusted@example.com"}`)
		case "/user/emails":
			fmt.Fprintf(w, `[{"email":"verified@example.com","verified":true,"primary":false},{"email":"primary@example.com","verified":%t,"primary":true}]`, verified)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	identity, err := githubIdentity(context.Background(), server.Client(), server.URL, "test-token")
	if err != nil || identity.Email != "primary@example.com" || identity.Subject != "123" {
		t.Fatalf("%+v %v", identity, err)
	}
	verified = false
	if _, err = githubIdentity(context.Background(), server.Client(), server.URL, "test-token"); err == nil {
		t.Fatal("unverified primary email accepted")
	}
}

func TestGitHubIdentityDoesNotForwardTokenAcrossRedirect(t *testing.T) {
	var leaked bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization") == "Bearer test-token"
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":123}`)
	}))
	defer attacker.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/collect", http.StatusFound)
	}))
	defer source.Close()
	if _, err := githubIdentity(context.Background(), source.Client(), source.URL, "test-token"); err == nil {
		t.Fatal("redirect accepted as GitHub identity")
	}
	if leaked {
		t.Fatal("GitHub token reached redirect target")
	}
}

func TestOAuthUnavailableProviderAndMissingState(t *testing.T) {
	handler := NewHTTPServer(&Service{}, HTTPConfig{PublicURL: "http://127.0.0.1:3000", GoogleOAuth: OAuthCredentials{ClientID: "client", ClientSecret: "secret"}})
	for _, path := range []string{"/v1/auth/oauth/github", "/v1/auth/oauth/google/callback?code=attacker-code"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "http://127.0.0.1:3000/sign-in?error=") {
			t.Fatalf("%d %s", response.Code, response.Body.String())
		}
	}
}
