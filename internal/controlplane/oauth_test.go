package controlplane

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	if got := safeOAuthNext("", "create-account"); got != "/onboarding/agent" {
		t.Fatal(got)
	}
}

func TestGoogleIdentityValidatesSignedClaims(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	verifier := oidc.NewVerifier("https://accounts.google.com", &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}, &oidc.Config{ClientID: "canter-client"})
	sign := func(claims map[string]any) string {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
		raw, _ := json.Marshal(claims)
		body := header + "." + base64.RawURLEncoding.EncodeToString(raw)
		hash := sha256.Sum256([]byte(body))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
		if err != nil {
			t.Fatal(err)
		}
		return body + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	for _, tc := range []struct {
		name, key string
		value     any
		valid     bool
	}{
		{name: "valid", valid: true}, {name: "wrong audience", key: "aud", value: "another-client"}, {name: "wrong issuer", key: "iss", value: "https://evil.test"}, {name: "expired", key: "exp", value: time.Now().Add(-time.Hour).Unix()}, {name: "wrong nonce", key: "nonce", value: "other-browser"}, {name: "unverified email", key: "email_verified", value: false}, {name: "missing subject", key: "sub", value: ""},
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
			} else if err == nil {
				t.Fatal("invalid claims accepted")
			}
		})
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
