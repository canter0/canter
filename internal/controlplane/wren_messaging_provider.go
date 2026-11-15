package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrWrenMessageRejected = errors.New("message request rejected")
var ErrWrenMessageWebhookInvalid = errors.New("invalid messaging webhook")
var ErrWrenMessageUnsupportedEvent = errors.New("unsupported messaging event")

// The adapter receives only an explicitly scoped sender/key from the caller.
// It never borrows Canter auth-mail credentials or supplies a default identity.
type WrenMessageAttachment struct {
	Name          string `json:"name"`
	MediaType     string `json:"mediaType"`
	ContentBase64 string `json:"contentBase64"`
}
type WrenMessageEnvelope struct {
	From        string                  `json:"from"`
	To          []string                `json:"to"`
	CC          []string                `json:"cc,omitempty"`
	BCC         []string                `json:"bcc,omitempty"`
	ReplyTo     string                  `json:"replyTo,omitempty"`
	Subject     string                  `json:"subject"`
	Text        string                  `json:"text"`
	Headers     map[string]string       `json:"headers,omitempty"`
	Attachments []WrenMessageAttachment `json:"attachments,omitempty"`
}
type WrenMessageProviderReceipt struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}
type WrenMessageProvider interface {
	Send(context.Context, string, string, WrenMessageEnvelope) (WrenMessageProviderReceipt, error)
}
type ResendWrenMessageProvider struct{ Client *http.Client }

const wrenMessageMaxAttachmentBytes = 8 << 20
const wrenMessageMaxProviderResponse = 2 << 20

var resendMessageID = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func messagingSafeLine(value string, max int) bool {
	return utf8.ValidString(value) && len(value) <= max && !strings.ContainsAny(value, "\r\n\x00")
}
func messagingAddress(value string) bool {
	if value == "" || !messagingSafeLine(value, 512) {
		return false
	}
	_, err := mail.ParseAddress(value)
	return err == nil
}
func rejectMessage(reason string) error { return fmt.Errorf("%w: %s", ErrWrenMessageRejected, reason) }
func validateMessageEnvelope(envelope WrenMessageEnvelope) error {
	if !messagingAddress(envelope.From) || len(envelope.To) == 0 || len(envelope.To)+len(envelope.CC)+len(envelope.BCC) > 50 {
		return rejectMessage("provide an explicit sender and between one and fifty recipients")
	}
	for _, list := range [][]string{envelope.To, envelope.CC, envelope.BCC} {
		for _, address := range list {
			if !messagingAddress(address) {
				return rejectMessage("invalid recipient")
			}
		}
	}
	if envelope.ReplyTo != "" && !messagingAddress(envelope.ReplyTo) {
		return rejectMessage("invalid reply address")
	}
	if !messagingSafeLine(envelope.Subject, 998) || !utf8.ValidString(envelope.Text) || len(envelope.Text) > 128<<10 || strings.TrimSpace(envelope.Text) == "" || strings.ContainsRune(envelope.Text, '\x00') {
		return rejectMessage("provide a bounded subject and nonempty plain text message")
	}
	if len(envelope.Headers) > 16 || len(envelope.Attachments) > 10 {
		return rejectMessage("too many headers or attachments")
	}
	headerBytes := 0
	for key, value := range envelope.Headers {
		if key == "" || !messagingSafeLine(value, 4096) {
			return rejectMessage("invalid email header")
		}
		for _, r := range key {
			if r > 126 || r < 33 || strings.ContainsRune("()<>@,;:\\\"/[]?={} ", r) {
				return rejectMessage("invalid email header name")
			}
		}
		switch strings.ToLower(key) {
		case "from", "to", "cc", "bcc", "reply-to", "subject", "return-path", "received", "mime-version", "content-type", "content-transfer-encoding", "authorization", "proxy-authorization":
			return rejectMessage("structural or credential headers cannot override the envelope")
		}
		headerBytes += len(key) + len(value)
	}
	if headerBytes > 8192 {
		return rejectMessage("email headers exceed the limit")
	}
	total := 0
	for _, attachment := range envelope.Attachments {
		if attachment.Name == "" || !messagingSafeLine(attachment.Name, 200) || strings.ContainsAny(attachment.Name, "/\\") || len(attachment.ContentBase64) > base64.StdEncoding.EncodedLen(wrenMessageMaxAttachmentBytes) {
			return rejectMessage("invalid attachment name or size")
		}
		media, params, err := mime.ParseMediaType(attachment.MediaType)
		if err != nil || media == "" || len(params) > 0 {
			return rejectMessage("invalid attachment media type")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(attachment.ContentBase64)
		if err != nil || len(data) == 0 {
			return rejectMessage("attachment must contain actual Base64 bytes")
		}
		total += len(data)
	}
	if total > wrenMessageMaxAttachmentBytes {
		return rejectMessage("attachments exceed the combined 8 MiB limit")
	}
	return nil
}
func (p ResendWrenMessageProvider) client() *http.Client {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	safe := credentialHTTPClient(client)
	if safe.Timeout <= 0 || safe.Timeout > 30*time.Second {
		safe.Timeout = 30 * time.Second
	}
	return safe
}

func (p ResendWrenMessageProvider) Send(ctx context.Context, apiKey, idempotencyKey string, envelope WrenMessageEnvelope) (WrenMessageProviderReceipt, error) {
	var receipt WrenMessageProviderReceipt
	if apiKey == "" || !messagingSafeLine(apiKey, 4096) || idempotencyKey == "" || !messagingSafeLine(idempotencyKey, 256) {
		return receipt, rejectMessage("an explicit API credential and stable idempotency key are required")
	}
	if err := validateMessageEnvelope(envelope); err != nil {
		return receipt, err
	}
	type attachment struct {
		Filename    string `json:"filename"`
		Content     string `json:"content"`
		ContentType string `json:"content_type,omitempty"`
	}
	body := struct {
		From        string            `json:"from"`
		To          []string          `json:"to"`
		CC          []string          `json:"cc,omitempty"`
		BCC         []string          `json:"bcc,omitempty"`
		ReplyTo     string            `json:"reply_to,omitempty"`
		Subject     string            `json:"subject"`
		Text        string            `json:"text"`
		Headers     map[string]string `json:"headers,omitempty"`
		Attachments []attachment      `json:"attachments,omitempty"`
	}{From: envelope.From, To: envelope.To, CC: envelope.CC, BCC: envelope.BCC, ReplyTo: envelope.ReplyTo, Subject: envelope.Subject, Text: envelope.Text, Headers: envelope.Headers}
	for _, file := range envelope.Attachments {
		body.Attachments = append(body.Attachments, attachment{file.Name, file.ContentBase64, file.MediaType})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return receipt, rejectMessage("invalid email payload")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(raw))
	if err != nil {
		return receipt, rejectMessage("invalid request")
	}
	// Go's transport considers an Idempotency-Key POST replayable when GetBody is
	// populated. Disable implicit transport retries; only the durable caller may
	// choose a reconciliation action, and Resend retains keys for only 24 hours.
	request.GetBody = nil
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := p.client().Do(request)
	if err != nil {
		return receipt, fmt.Errorf("message provider outcome is unknown; no automatic retry")
	}
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(result) > 65536 {
		return receipt, fmt.Errorf("message provider response is incomplete; outcome is unknown")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return receipt, resendMessageHTTPError(response.StatusCode, result)
	}
	var decoded struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(result, &decoded) != nil || !resendMessageID.MatchString(decoded.ID) {
		return receipt, fmt.Errorf("message provider did not return an acceptance receipt; outcome is unknown")
	}
	return WrenMessageProviderReceipt{ID: decoded.ID, Status: "accepted"}, nil
}
func resendMessageHTTPError(status int, body []byte) error {
	var response struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(body, &response)
	// A concurrent key explicitly means the original send may still be in flight.
	// Unknown/conflicting 409 bodies are not evidence that no email was accepted.
	definite := status >= 400 && status < 500 && status != 408 && status != 409
	if status == 409 && response.Name == "invalid_idempotent_request" {
		definite = true
	}
	if definite {
		return fmt.Errorf("%w: provider HTTP %d", ErrWrenMessageRejected, status)
	}
	return fmt.Errorf("message provider HTTP %d; acceptance outcome is unknown", status)
}

type WrenReceivedMessageAttachment struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
}
type WrenReceivedMessage struct {
	ProviderID    string                          `json:"providerId"`
	From          string                          `json:"from"`
	To            []string                        `json:"to"`
	CC            []string                        `json:"cc,omitempty"`
	BCC           []string                        `json:"bcc,omitempty"`
	ReplyTo       []string                        `json:"replyTo,omitempty"`
	ReceivedFor   []string                        `json:"receivedFor,omitempty"`
	Subject       string                          `json:"subject"`
	Text          string                          `json:"text"`
	TextAvailable bool                            `json:"textAvailable"`
	HTML          string                          `json:"-"` // Untrusted source content, never an executable app view.
	Headers       map[string]string               `json:"headers"`
	MessageID     string                          `json:"messageId"`
	CreatedAt     string                          `json:"createdAt"`
	Attachments   []WrenReceivedMessageAttachment `json:"attachments"`
}

// Receive fetches only Resend's fixed receiving endpoint. Signed raw/attachment
// URLs in its response are deliberately not followed or exposed as authority.
func (p ResendWrenMessageProvider) Receive(ctx context.Context, apiKey, providerID string) (WrenReceivedMessage, error) {
	var out WrenReceivedMessage
	if apiKey == "" || !messagingSafeLine(apiKey, 4096) || !resendMessageID.MatchString(providerID) {
		return out, rejectMessage("explicit receiving credential and provider email ID required")
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.resend.com/emails/receiving/"+providerID, nil)
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := p.client().Do(request)
	if err != nil {
		return out, fmt.Errorf("received message could not be retrieved")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, wrenMessageMaxProviderResponse+1))
	if err != nil || len(raw) > wrenMessageMaxProviderResponse {
		return out, fmt.Errorf("received message exceeds the bounded response limit or is incomplete")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return out, resendMessageHTTPError(response.StatusCode, raw)
	}
	var wire struct {
		ID, From, Subject string
		To, CC, BCC       []string
		ReplyTo           []string `json:"reply_to"`
		ReceivedFor       []string `json:"received_for"`
		CreatedAt         string   `json:"created_at"`
		MessageID         string   `json:"message_id"`
		Text, HTML        *string
		Headers           map[string]string
		Attachments       []struct {
			ID          string
			Filename    string
			ContentType string `json:"content_type"`
			Size        int64
		}
	}
	if json.Unmarshal(raw, &wire) != nil || wire.ID != providerID || !messagingAddress(wire.From) || len(wire.To) > 50 || len(wire.CC) > 50 || len(wire.BCC) > 50 {
		return out, fmt.Errorf("received message metadata is invalid")
	}
	if _, err = time.Parse(time.RFC3339Nano, wire.CreatedAt); err != nil {
		return out, fmt.Errorf("received message has no valid creation time")
	}
	for _, list := range [][]string{wire.To, wire.CC, wire.BCC, wire.ReplyTo, wire.ReceivedFor} {
		for _, address := range list {
			if !messagingAddress(address) {
				return out, fmt.Errorf("received message contains invalid recipient metadata")
			}
		}
	}
	if !messagingSafeLine(wire.Subject, 998) || !messagingSafeLine(wire.MessageID, 998) {
		return out, fmt.Errorf("received message headers are invalid")
	}
	out = WrenReceivedMessage{ProviderID: wire.ID, From: wire.From, To: wire.To, CC: wire.CC, BCC: wire.BCC, ReplyTo: wire.ReplyTo, Subject: wire.Subject, Headers: wire.Headers, MessageID: wire.MessageID, CreatedAt: wire.CreatedAt, TextAvailable: wire.Text != nil, ReceivedFor: wire.ReceivedFor, Attachments: []WrenReceivedMessageAttachment{}}
	if wire.Text != nil {
		out.Text = *wire.Text
	}
	if wire.HTML != nil {
		out.HTML = *wire.HTML
	}
	for _, file := range wire.Attachments {
		if !resendMessageID.MatchString(file.ID) || !messagingSafeLine(file.Filename, 512) || file.Size < 0 {
			return WrenReceivedMessage{}, fmt.Errorf("received attachment metadata is invalid")
		}
		out.Attachments = append(out.Attachments, WrenReceivedMessageAttachment{file.ID, file.Filename, file.ContentType, file.Size})
	}
	return out, nil
}

type WrenMessageWebhookEvent struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	ProviderID  string   `json:"providerId"`
	From        string   `json:"from"`
	To          []string `json:"to"`
	Subject     string   `json:"subject"`
	MessageID   string   `json:"messageId"`
	CreatedAt   string   `json:"createdAt"`
	CC          []string `json:"cc,omitempty"`
	BCC         []string `json:"bcc,omitempty"`
	ReceivedFor []string `json:"receivedFor,omitempty"`
}

func singleSvixHeader(headers http.Header, name string) (string, error) {
	value := ""
	count := 0
	for key, values := range headers {
		if strings.EqualFold(key, name) {
			count += len(values)
			if len(values) == 1 {
				value = values[0]
			}
		}
	}
	if count != 1 || value == "" || !messagingSafeLine(value, 4096) {
		return "", ErrWrenMessageWebhookInvalid
	}
	return value, nil
}

// ResendSvixWebhook verifies id.timestamp.rawBody using the decoded whsec_ key,
// then normalizes supported email events. Signature verification never replaces
// the caller's durable svix-id deduplication or mailbox/tenant routing checks.
func ResendSvixWebhook(raw []byte, headers http.Header, secret string, now time.Time) (WrenMessageWebhookEvent, error) {
	var out WrenMessageWebhookEvent
	if len(raw) == 0 || len(raw) > 1<<20 || !strings.HasPrefix(secret, "whsec_") || len(secret) > 1024 {
		return out, ErrWrenMessageWebhookInvalid
	}
	id, err := singleSvixHeader(headers, "svix-id")
	if err != nil {
		return out, err
	}
	timestamp, err := singleSvixHeader(headers, "svix-timestamp")
	if err != nil {
		return out, err
	}
	signature, err := singleSvixHeader(headers, "svix-signature")
	if err != nil {
		return out, err
	}
	if len(id) > 200 || strings.IndexFunc(id, func(r rune) bool { return r > 127 || unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return out, ErrWrenMessageWebhookInvalid
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || strconv.FormatInt(seconds, 10) != timestamp || seconds < now.Unix()-300 || seconds > now.Unix()+300 {
		return out, ErrWrenMessageWebhookInvalid
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) < 16 || len(key) > 128 {
		return out, ErrWrenMessageWebhookInvalid
	}
	defer clear(key)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(id + "." + timestamp + "."))
	_, _ = mac.Write(raw)
	expected := mac.Sum(nil)
	valid := false
	for _, candidate := range strings.Fields(signature) {
		version, encoded, ok := strings.Cut(candidate, ",")
		if !ok || version != "v1" {
			continue
		}
		actual, e := base64.StdEncoding.DecodeString(encoded)
		if e == nil && hmac.Equal(expected, actual) {
			valid = true
		}
	}
	if !valid {
		return out, ErrWrenMessageWebhookInvalid
	}
	var wire struct {
		Type      string `json:"type"`
		CreatedAt string `json:"created_at"`
		Data      struct {
			EmailID     string `json:"email_id"`
			From        string
			To          []string
			CC          []string `json:"cc"`
			BCC         []string `json:"bcc"`
			ReceivedFor []string `json:"received_for"`
			Subject     string
			MessageID   string `json:"message_id"`
			CreatedAt   string `json:"created_at"`
		}
	}
	if json.Unmarshal(raw, &wire) != nil {
		return out, ErrWrenMessageWebhookInvalid
	}
	kind := ""
	switch wire.Type {
	case "email.received":
		kind = "received"
	case "email.sent":
		kind = "accepted"
	case "email.delivered":
		kind = "delivered"
	case "email.bounced":
		kind = "bounced"
	case "email.complained":
		kind = "complained"
	case "email.failed", "email.suppressed":
		kind = "failed"
	default:
		return out, ErrWrenMessageUnsupportedEvent
	}
	if !resendMessageID.MatchString(wire.Data.EmailID) {
		return out, ErrWrenMessageWebhookInvalid
	}
	created := wire.Data.CreatedAt
	if created == "" {
		created = wire.CreatedAt
	}
	if _, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return out, ErrWrenMessageWebhookInvalid
	}
	if !messagingAddress(wire.Data.From) || len(wire.Data.To)+len(wire.Data.CC)+len(wire.Data.BCC)+len(wire.Data.ReceivedFor) == 0 || len(wire.Data.To)+len(wire.Data.CC)+len(wire.Data.BCC)+len(wire.Data.ReceivedFor) > 100 || !messagingSafeLine(wire.Data.Subject, 998) || !messagingSafeLine(wire.Data.MessageID, 998) {
		return out, ErrWrenMessageWebhookInvalid
	}
	for _, to := range append(append(append(append([]string{}, wire.Data.To...), wire.Data.CC...), wire.Data.BCC...), wire.Data.ReceivedFor...) {
		if !messagingAddress(to) {
			return out, ErrWrenMessageWebhookInvalid
		}
	}
	return WrenMessageWebhookEvent{ID: id, Kind: kind, ProviderID: wire.Data.EmailID, From: wire.Data.From, To: wire.Data.To, CC: wire.Data.CC, BCC: wire.Data.BCC, ReceivedFor: wire.Data.ReceivedFor, Subject: wire.Data.Subject, MessageID: wire.Data.MessageID, CreatedAt: created}, nil
}
