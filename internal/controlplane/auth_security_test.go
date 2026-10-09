package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestTOTPWindowAndReplay(t *testing.T) {
	secret := "JBSWY3DPEHPK3PXP"
	now := time.Unix(1800000010, 0)
	step := now.Unix() / 30
	for _, offset := range []int64{-2, -1, 0, 1, 2} {
		code, err := totp.GenerateCode(secret, now.Add(time.Duration(offset)*30*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		matched, ok := acceptedTOTPStep(secret, code, now, -1)
		if ok != (offset >= -1 && offset <= 1) {
			t.Fatal(offset, ok)
		}
		if ok && matched != step+offset {
			t.Fatal("incorrect timestep")
		}
		if _, ok = acceptedTOTPStep(secret, code, now, step+offset); ok {
			t.Fatal("replayed step accepted")
		}
	}
}

func TestOfflinePasswordDenylist(t *testing.T) {
	if len(commonPasswordDenylist) < 10000 || !passwordDenied(" Password1 ", commonPasswordDenylist) {
		t.Fatal("bundled common-password corpus missing")
	}
	password := "compromised offline passphrase"
	hash := sha256.Sum256([]byte(password))
	list, err := ReadPasswordDenylist(strings.NewReader(fmt.Sprintf("# operator corpus\n%x\n", hash)))
	if err != nil || !passwordDenied(strings.ToUpper(password), list) {
		t.Fatal("offline corpus not applied", err)
	}
	h := &HTTPServer{config: HTTPConfig{Auth: AuthConfig{PasswordDenylist: list}}}
	if h.checkPassword(password) == nil || h.checkPassword("a different unique passphrase") != nil {
		t.Fatal("additional password policy not enforced")
	}
	if _, err = ReadPasswordDenylist(strings.NewReader("invalid")); err == nil {
		t.Fatal("invalid corpus accepted")
	}
}

type authRoundTripper func(*http.Request) (*http.Response, error)

func (fn authRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestResendAdapterDeliveryAndFailureClassification(t *testing.T) {
	for _, status := range []int{200, 400, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			mailer := ResendMailer{APIKey: "test-key", From: "security@canter.test", Client: &http.Client{Transport: authRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://api.resend.com/emails" || r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Idempotency-Key") != "mail-test" {
					t.Fatal("wrong delivery request")
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"id":"email-test"}`))}, nil
			})}}
			id, err := mailer.Send(context.Background(), "mail-test", AuthEmail{To: "recipient@example.test", Text: "test"})
			if status == 200 && (err != nil || id != "email-test") {
				t.Fatal(id, err)
			}
			if status != 200 && err == nil {
				t.Fatal("delivery failure ignored")
			}
			_, permanent := err.(permanentMailError)
			if permanent != (status == 400) {
				t.Fatal("wrong retry classification", err)
			}
		})
	}
}

func TestResendAdapterDoesNotReplayEmailToRedirect(t *testing.T) {
	calls := 0
	mailer := ResendMailer{APIKey: "test-key", From: "security@canter.test", Client: &http.Client{Transport: authRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			t.Fatal("email request followed redirect and replayed its body")
		}
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {"https://collector.test/"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}}
	if _, err := mailer.Send(context.Background(), "mail-test", AuthEmail{To: "recipient@example.test", Text: "private email body"}); err == nil {
		t.Fatal("redirect response accepted")
	}
	if calls != 1 {
		t.Fatalf("transport called %d times", calls)
	}
}

func TestTurnstileDoesNotReplaySecretToRedirect(t *testing.T) {
	calls := 0
	old := turnstileHTTPClient
	t.Cleanup(func() { turnstileHTTPClient = old })
	turnstileHTTPClient = &http.Client{Transport: authRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			t.Fatal("verification request followed redirect and replayed its body")
		}
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {"https://collector.test/"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	h := &HTTPServer{config: HTTPConfig{PublicURL: "https://canter.test", Auth: AuthConfig{TurnstileSecret: "test-secret"}}}
	w := httptest.NewRecorder()
	if h.verifyBot(w, httptest.NewRequest("POST", "/", nil), "test-token") {
		t.Fatal("redirect response accepted")
	}
	if calls != 1 {
		t.Fatalf("transport called %d times", calls)
	}
}

func TestTurnstileRejectsWrongHostActionAndFailure(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	h := &HTTPServer{config: HTTPConfig{PublicURL: "https://canter.test", Auth: AuthConfig{TurnstileSecret: "test-secret"}}}
	for _, test := range []struct {
		body  string
		valid bool
	}{
		{`{"success":true,"hostname":"canter.test","action":"auth"}`, true},
		{`{"success":true,"hostname":"other.test","action":"auth"}`, false},
		{`{"success":true,"hostname":"canter.test","action":"other"}`, false},
		{`{"success":false,"hostname":"canter.test","action":"auth"}`, false},
		{`invalid`, false},
	} {
		http.DefaultTransport = authRoundTripper(func(r *http.Request) (*http.Response, error) {
			if err := r.ParseForm(); err != nil || r.Form.Get("secret") != "test-secret" || r.Form.Get("response") != "test-token" {
				t.Fatal("incorrect bot verification request", err)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(test.body))}, nil
		})
		w := httptest.NewRecorder()
		if h.verifyBot(w, httptest.NewRequest("POST", "/", nil), "test-token") != test.valid {
			t.Fatal(test.body, w.Body.String())
		}
	}
}
func TestAuthPasswordUnicodeAndMalformedHash(t *testing.T) {
	if err := validateNewPassword(strings.Repeat("界", 5)); err == nil {
		t.Fatal("password counted bytes instead of characters")
	}
	if err := validateNewPassword(strings.Repeat("界", 15)); err != nil {
		t.Fatal(err)
	}
	if err := validateNewPassword("passwordpassword"); err == nil {
		t.Fatal("common password accepted")
	}
	for _, hash := range []string{"$argon2id$v=19$m=0,t=0,p=0$AA$AA", "$argon2id$v=19$m=999999999,t=3,p=2$AA$AA"} {
		if verifyPassword(hash, "password") {
			t.Fatal("bad hash accepted")
		}
	}
}
func TestResendWebhookSignatureAndFreshness(t *testing.T) {
	key := []byte("a sufficiently long test signing key")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(key)
	now := time.Now()
	body := []byte(`{"type":"email.delivered"}`)
	headers := http.Header{}
	headers.Set("svix-id", "msg-test")
	headers.Set("svix-timestamp", fmt.Sprint(now.Unix()))
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "msg-test.%d.", now.Unix())
	mac.Write(body)
	headers.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	if !validResendSignature(secret, headers, body, now) {
		t.Fatal("valid signature rejected")
	}
	if validResendSignature(secret, headers, append(body, ' '), now) || validResendSignature(secret, headers, body, now.Add(6*time.Minute)) || validResendSignature("", headers, body, now) {
		t.Fatal("invalid signature accepted")
	}
}
