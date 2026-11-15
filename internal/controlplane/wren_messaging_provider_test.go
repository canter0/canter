package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type wrenMessageRoundTrip func(*http.Request) (*http.Response, error)

func (f wrenMessageRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const ownedResendID = "56761188-7520-42d8-8898-ff6fc54ce618"

func ownedMessageEnvelope() WrenMessageEnvelope {
	return WrenMessageEnvelope{From: "Owned Clinic <mail@clinic.example.test>", To: []string{"person@owned.example.test"}, CC: []string{"copy@owned.example.test"}, BCC: []string{"private@owned.example.test"}, ReplyTo: "reply@clinic.example.test", Subject: "Owned correspondence test", Text: "A real bounded plain text body.", Headers: map[string]string{"In-Reply-To": "<prior@owned.example.test>", "References": "<prior@owned.example.test>"}, Attachments: []WrenMessageAttachment{{Name: "quote.csv", MediaType: "text/csv", ContentBase64: base64.StdEncoding.EncodeToString([]byte("quantity,cents\n3,3750\n"))}}}
}
func TestWrenMessagingProviderOwnedHTTPWireAndNoRedirect(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/emails" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer owned-provider-key" || r.Header.Get("Idempotency-Key") != "owned-draft-revision-1" {
			t.Error("wrong provider wire metadata")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid actual JSON wire")
		}
		for _, field := range []string{"from", "to", "cc", "bcc", "reply_to", "subject", "text", "headers", "attachments"} {
			if body[field] == nil {
				t.Error("missing envelope field", field)
			}
		}
		if body["html"] != nil || body["apiKey"] != nil || body["idempotencyKey"] != nil {
			t.Error("unrequested HTML/credential fields")
		}
		var attachments []struct {
			Filename, Content string
			ContentType       string `json:"content_type"`
			Path              string
		}
		_ = json.Unmarshal(body["attachments"], &attachments)
		if len(attachments) != 1 || attachments[0].Filename != "quote.csv" || attachments[0].ContentType != "text/csv" || attachments[0].Path != "" {
			t.Error("wrong attachment wire")
		}
		data, err := base64.StdEncoding.DecodeString(attachments[0].Content)
		if err != nil || !bytes.Equal(data, []byte("quantity,cents\n3,3750\n")) {
			t.Error("attachment bytes changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"`+ownedResendID+`"}`)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	provider := ResendWrenMessageProvider{Client: &http.Client{Transport: wrenMessageRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.resend.com" || r.URL.Path != "/emails" || r.GetBody != nil {
			t.Error("destination is not fixed or POST is implicitly replayable")
		}
		clone := r.Clone(r.Context())
		clone.URL.Scheme = target.Scheme
		clone.URL.Host = target.Host
		return http.DefaultTransport.RoundTrip(clone)
	})}}
	receipt, err := provider.Send(context.Background(), "owned-provider-key", "owned-draft-revision-1", ownedMessageEnvelope())
	if err != nil || receipt.ID != ownedResendID || receipt.Status != "accepted" || calls.Load() != 1 {
		t.Fatalf("acceptance %+v %v calls=%d", receipt, err, calls.Load())
	}
	// Even a caller-provided permissive redirect policy is overridden.
	var redirectCalls atomic.Int32
	provider.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }, Transport: wrenMessageRoundTrip(func(r *http.Request) (*http.Response, error) {
		redirectCalls.Add(1)
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://credential-leak.example.invalid/steal"}}, Body: io.NopCloser(strings.NewReader("redirect")), Request: r}, nil
	})}
	if _, err = provider.Send(context.Background(), "owned-provider-key", "owned-draft-revision-1", ownedMessageEnvelope()); err == nil || errors.Is(err, ErrWrenMessageRejected) || redirectCalls.Load() != 1 {
		t.Fatal("redirect followed or definite success invented", err, redirectCalls.Load())
	}
}
func TestWrenMessagingProviderDefiniteRejectionVersusUnknown(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		network  bool
		rejected bool
	}{
		{"validation", 422, `{"name":"validation_error","message":"do not echo owned-provider-key"}`, false, true},
		{"bad key", 401, `{"name":"validation_error"}`, false, true},
		{"rate limit", 429, `{"name":"rate_limit_exceeded"}`, false, true},
		{"changed idempotent request", 409, `{"name":"invalid_idempotent_request"}`, false, true},
		{"inflight idempotent request", 409, `{"name":"concurrent_idempotent_requests"}`, false, false},
		{"unknown conflict", 409, `{}`, false, false},
		{"server error", 503, `{"name":"internal_server_error"}`, false, false},
		{"timeout", 408, `{}`, false, false},
		{"malformed acceptance", 200, `not JSON`, false, false},
		{"blank acceptance", 200, `{}`, false, false},
		{"wrong receipt ID", 200, `{"id":"not-a-provider-id"}`, false, false},
		{"network", 0, "", true, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			p := ResendWrenMessageProvider{Client: &http.Client{Transport: wrenMessageRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if test.network {
					return nil, errors.New("owned-provider-key is an intentionally untrusted transport diagnostic")
				}
				return &http.Response{StatusCode: test.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})}}
			receipt, err := p.Send(context.Background(), "owned-provider-key", "same-stable-key", ownedMessageEnvelope())
			if err == nil || receipt.ID != "" || receipt.Status != "" || errors.Is(err, ErrWrenMessageRejected) != test.rejected || calls != 1 || strings.Contains(err.Error(), "owned-provider-key") {
				t.Fatalf("wrong outcome receipt=%+v rejected=%v err=%v calls=%d", receipt, test.rejected, err, calls)
			}
		})
	}
}
func TestWrenMessagingProviderInvalidEnvelopeNeverDispatched(t *testing.T) {
	base := ownedMessageEnvelope()
	cases := []func(*WrenMessageEnvelope){func(v *WrenMessageEnvelope) { v.From = "" }, func(v *WrenMessageEnvelope) { v.To = nil }, func(v *WrenMessageEnvelope) { v.BCC = []string{"person@example.test\r\nBcc: injected@example.test"} }, func(v *WrenMessageEnvelope) { v.Subject = "bad\r\nSubject: injection" }, func(v *WrenMessageEnvelope) { v.Text = " " }, func(v *WrenMessageEnvelope) { v.Headers = map[string]string{"From": "override@example.test"} }, func(v *WrenMessageEnvelope) { v.Headers = map[string]string{"In-Reply-To": "bad\nheader"} }, func(v *WrenMessageEnvelope) {
		v.Attachments = []WrenMessageAttachment{{Name: "../private", MediaType: "text/plain", ContentBase64: "YQ=="}}
	}, func(v *WrenMessageEnvelope) {
		v.Attachments = []WrenMessageAttachment{{Name: "note.txt", MediaType: "text/plain", ContentBase64: "not base64"}}
	}}
	calls := 0
	p := ResendWrenMessageProvider{Client: &http.Client{Transport: wrenMessageRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected network") })}}
	for i, mutate := range cases {
		v := base
		mutate(&v)
		if _, err := p.Send(context.Background(), "owned-key", "stable-key", v); !errors.Is(err, ErrWrenMessageRejected) {
			t.Fatal("invalid envelope was not rejected", i, err)
		}
	}
	if _, err := p.Send(context.Background(), "", "stable-key", base); !errors.Is(err, ErrWrenMessageRejected) {
		t.Fatal("adapter supplied default auth identity")
	}
	if calls != 0 {
		t.Fatal("invalid content reached network", calls)
	}
}
func ownedSvixHeaders(raw []byte, id, secret string, at time.Time) http.Header {
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	timestamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(id + "." + timestamp + "."))
	_, _ = mac.Write(raw)
	headers := make(http.Header)
	headers.Set("svix-id", id)
	headers.Set("svix-timestamp", timestamp)
	headers.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return headers
}
func ownedResendEvent(kind string) []byte {
	return []byte(`{"type":"` + kind + `","created_at":"2026-02-22T23:41:12.126Z","data":{"email_id":"` + ownedResendID + `","created_at":"2026-02-22T23:41:11.894Z","from":"Clinic <sender@owned.example.test>","to":["inbox@owned.example.test"],"subject":"Owned request","message_id":"<111-222-333@owned.example.test>"}}`)
}
func TestWrenMessagingSvixAuthenticatesBodyIDTimestampAndRotation(t *testing.T) {
	now := time.Unix(1800000000, 0)
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("an-owned-webhook-signing-key-123456"))
	raw := ownedResendEvent("email.received")
	headers := ownedSvixHeaders(raw, "msg_owned_actual_wire_001", secret, now)
	event, err := ResendSvixWebhook(raw, headers, secret, now)
	if err != nil || event.ID != "msg_owned_actual_wire_001" || event.Kind != "received" || event.ProviderID != ownedResendID || event.MessageID != "<111-222-333@owned.example.test>" || event.CreatedAt != "2026-02-22T23:41:11.894Z" {
		t.Fatalf("actual Resend normalization %+v %v", event, err)
	}
	cases := []struct {
		name   string
		change func([]byte, http.Header) ([]byte, http.Header)
	}{
		{"changed raw body", func(b []byte, h http.Header) ([]byte, http.Header) { return append(append([]byte{}, b...), ' '), h }},
		{"unsigned ID", func(b []byte, h http.Header) ([]byte, http.Header) {
			h.Set("svix-id", "msg_other_unsigned")
			return b, h
		}},
		{"timestamp changed", func(b []byte, h http.Header) ([]byte, http.Header) {
			h.Set("svix-timestamp", strconv.FormatInt(now.Unix()+1, 10))
			return b, h
		}},
		{"missing timestamp", func(b []byte, h http.Header) ([]byte, http.Header) { h.Del("svix-timestamp"); return b, h }},
		{"unsigned JSON", func(b []byte, h http.Header) ([]byte, http.Header) { h.Del("svix-signature"); return b, h }},
		{"duplicate ID", func(b []byte, h http.Header) ([]byte, http.Header) { h.Add("svix-id", h.Get("svix-id")); return b, h }},
		{"case duplicate ID", func(b []byte, h http.Header) ([]byte, http.Header) {
			h["svix-id"] = []string{h.Get("svix-id")}
			return b, h
		}},
		{"malformed signature", func(b []byte, h http.Header) ([]byte, http.Header) { h.Set("svix-signature", "v1,@@@"); return b, h }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			b, h := test.change(raw, headers.Clone())
			if _, err := ResendSvixWebhook(b, h, secret, now); !errors.Is(err, ErrWrenMessageWebhookInvalid) {
				t.Fatal("unsigned/tampered event accepted", err)
			}
		})
	}
	for _, offset := range []int64{-301, 301} {
		h := ownedSvixHeaders(raw, "msg_owned_actual_wire_001", secret, now.Add(time.Duration(offset)*time.Second))
		if _, err := ResendSvixWebhook(raw, h, secret, now); !errors.Is(err, ErrWrenMessageWebhookInvalid) {
			t.Fatal("stale/future signed event accepted", offset, err)
		}
	}
	for _, offset := range []int64{-300, 300} {
		h := ownedSvixHeaders(raw, "msg_owned_actual_wire_001", secret, now.Add(time.Duration(offset)*time.Second))
		if _, err := ResendSvixWebhook(raw, h, secret, now); err != nil {
			t.Fatal("exact freshness boundary rejected", offset, err)
		}
	}
	rotated := headers.Clone()
	rotated.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(make([]byte, 32))+" v2,ignored "+headers.Get("svix-signature"))
	if _, err := ResendSvixWebhook(raw, rotated, secret, now); err != nil {
		t.Fatal("valid rotation signature rejected", err)
	}
	if _, err := ResendSvixWebhook(raw, headers, "whsec_@@@", now); !errors.Is(err, ErrWrenMessageWebhookInvalid) {
		t.Fatal("malformed signing key accepted")
	}
}
func TestWrenMessagingWebhookStatusMappingDoesNotInventDelivery(t *testing.T) {
	now := time.Unix(1800000000, 0)
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("an-owned-webhook-signing-key-123456"))
	for wire, want := range map[string]string{"email.sent": "accepted", "email.delivered": "delivered", "email.bounced": "bounced", "email.complained": "complained", "email.failed": "failed", "email.suppressed": "failed"} {
		raw := ownedResendEvent(wire)
		event, err := ResendSvixWebhook(raw, ownedSvixHeaders(raw, "msg_owned_status_001", secret, now), secret, now)
		if err != nil || event.Kind != want {
			t.Fatal("wrong delivery evidence", wire, event, err)
		}
	}
	for _, kind := range []string{"email.opened", "email.delivery_delayed"} {
		raw := ownedResendEvent(kind)
		if _, err := ResendSvixWebhook(raw, ownedSvixHeaders(raw, "msg_owned_ignored_001", secret, now), secret, now); !errors.Is(err, ErrWrenMessageUnsupportedEvent) {
			t.Fatal("unsupported event invented delivery", kind, err)
		}
	}
}
func TestWrenMessagingReceivingFixedURLActualNullableBodyAndNoRemoteDownloads(t *testing.T) {
	calls := 0
	p := ResendWrenMessageProvider{Client: &http.Client{Transport: wrenMessageRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.String() != "https://api.resend.com/emails/receiving/"+ownedResendID || r.Header.Get("Authorization") != "Bearer owned-receiving-key" {
			t.Error("unbounded receiving destination")
		}
		body := `{"id":"` + ownedResendID + `","from":"sender@owned.example.test","to":["inbox@owned.example.test"],"subject":"Actual source","created_at":"2026-02-22T23:41:11.894Z","message_id":"<wire@owned.example.test>","text":null,"html":"<script>untrusted</script>","headers":{"in-reply-to":"<prior@owned.example.test>"},"raw":{"download_url":"http://169.254.169.254/latest/meta-data"},"attachments":[{"id":"2a0c9ce0-3112-4728-976e-47ddcd16a318","filename":"quote.pdf","content_type":"application/pdf","size":123}]}
`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	message, err := p.Receive(context.Background(), "owned-receiving-key", ownedResendID)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextAvailable || message.Text != "" || message.HTML != "<script>untrusted</script>" || message.Headers["in-reply-to"] != "<prior@owned.example.test>" || len(message.Attachments) != 1 || calls != 1 {
		t.Fatalf("receiving body semantics lost %+v calls=%d", message, calls)
	}
	encoded, _ := json.Marshal(message)
	if bytes.Contains(encoded, []byte("<script>")) || bytes.Contains(encoded, []byte("169.254")) {
		t.Fatal("untrusted HTML/raw download endpoint escaped into normal DTO")
	}
	if _, err = p.Receive(context.Background(), "owned-receiving-key", "../emails"); !errors.Is(err, ErrWrenMessageRejected) || calls != 1 {
		t.Fatal("provider ID enabled arbitrary endpoint")
	}
}
