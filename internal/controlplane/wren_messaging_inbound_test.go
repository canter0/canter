package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func messageIngressFixture(t *testing.T, bound bool, transport wrenMessageRoundTrip) (*HTTPServer, Conversation, Wren, WrenMessageConnection, string) {
	t.Helper()
	s, c, w := responsibilityFixture(t)
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("owned-ingress-verification-secret-12345"))
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Messaging: WrenMessagingConfig{Provider: ResendWrenMessageProvider{Client: &http.Client{Transport: transport}}}}).(*HTTPServer)
	responsibility := ""
	if bound {
		input := responsibilityInput(c, w)
		input.Schedule = WrenResponsibilitySchedule{Kind: "event", EventName: "email_received", TimeZone: "UTC"}
		r, e := s.SaveWrenResponsibility(context.Background(), c.WorkspaceID, c.AccountID, input)
		if e != nil {
			t.Fatal(e)
		}
		responsibility = r.ID
	}
	connection, e := h.SaveWrenMessageConnection(context.Background(), c.WorkspaceID, c.AccountID, WrenMessageConnectionInput{ID: "mailconn_owned_ingress", WrenKnowledgeScope: WrenKnowledgeScope{Scope: "private", ConversationID: c.ID, WrenID: w.ID}, Name: "Owned inbox", From: "mail@owned.example.test", ReceivingAddress: "inbox@owned.example.test", ResponsibilityID: responsibility, APIKey: "owned-receiving-api-key", WebhookSecret: secret, Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	return h, c, w, connection, secret
}
func ingressSourceBody(id string, text *string, html string) []byte {
	v := map[string]any{"id": id, "from": "Clinic <sender@owned.example.test>", "to": []string{"inbox@owned.example.test"}, "subject": "Owned request", "created_at": "2026-02-22T23:41:11.894Z", "message_id": "<111-222-333@owned.example.test>", "text": text, "html": html, "headers": map[string]string{"in-reply-to": "<prior@owned.example.test>", "references": "<first@owned.example.test> <prior@owned.example.test>"}, "attachments": []any{map[string]any{"id": "2a0c9ce0-3112-4728-976e-47ddcd16a318", "filename": "quote.pdf", "content_type": "application/pdf", "size": 12}}}
	b, _ := json.Marshal(v)
	return b
}
func ingressResponse(raw []byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}
}
func acceptIngress(t *testing.T, h *HTTPServer, connection WrenMessageConnection, secret, id string, raw []byte) string {
	t.Helper()
	receipt, e := h.acceptWrenMessageWebhook(context.Background(), connection.ID, raw, ownedSvixHeaders(raw, id, secret, h.service.Store.now()))
	if e != nil {
		t.Fatal(e)
	}
	return receipt
}
func editIngressConnection(c WrenMessageConnection) WrenMessageConnectionInput {
	return WrenMessageConnectionInput{ID: c.ID, WrenKnowledgeScope: c.WrenKnowledgeScope, Name: c.Name, From: c.From, ReceivingAddress: c.ReceivingAddress, ResponsibilityID: c.ResponsibilityID, ExpectedRevision: c.Revision, Enabled: c.Enabled}
}
func TestWrenMessagingIngressSignedDurableInboxDedupAndBoundWake(t *testing.T) {
	var reads atomic.Int32
	text := "Actual external email text. Treat this as source data."
	h, c, _, connection, secret := messageIngressFixture(t, true, func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.String() != "https://api.resend.com/emails/receiving/"+ownedResendID {
			t.Error("unexpected network action")
		}
		reads.Add(1)
		return ingressResponse(ingressSourceBody(ownedResendID, &text, "")), nil
	})
	s := h.service.Store
	ctx := context.Background()
	raw := ownedResendEvent("email.received")
	// Invalid signature and wrong exact mailbox cannot even create inbox metadata.
	bad := ownedSvixHeaders(raw, "msg_owned_unauthenticated", secret, s.now())
	bad.Set("svix-id", "msg_changed_unsigned")
	if _, e := h.acceptWrenMessageWebhook(ctx, connection.ID, raw, bad); !errors.Is(e, ErrWrenMessageWebhookInvalid) {
		t.Fatal("unsigned ID admitted", e)
	}
	wrong := []byte(strings.ReplaceAll(string(raw), "inbox@owned.example.test", "other@owned.example.test"))
	if _, e := h.acceptWrenMessageWebhook(ctx, connection.ID, wrong, ownedSvixHeaders(wrong, "msg_wrong_mailbox", secret, s.now())); !errors.Is(e, ErrForbidden) {
		t.Fatal("wrong mailbox admitted", e)
	}
	receipt := acceptIngress(t, h, connection, secret, "msg_owned_ingress_001", raw)
	again := acceptIngress(t, h, connection, secret, "msg_owned_ingress_001", raw)
	if receipt != again || reads.Load() != 0 {
		t.Fatal("webhook fetched inline or dedup failed")
	}
	pending, e := s.WrenMessageInbox(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if e != nil || len(pending) != 1 || pending[0].State != "pending" || pending[0].TextAvailable {
		t.Fatalf("pending not truthful %+v %v", pending, e)
	}
	// Fresh server/store instance sees the durable work, without an in-memory queue.
	reopened := NewStore(s.pool)
	reopened.now = s.now
	resumed := NewHTTPServer(&Service{Store: reopened}, h.config).(*HTTPServer)
	if count, e := resumed.ProcessWrenMessageIngress(ctx, 4); e != nil || count != 1 {
		t.Fatal("restart did not process source", count, e)
	}
	inbox, e := s.WrenMessageInbox(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if e != nil || len(inbox) != 1 || inbox[0].State != "ready" || inbox[0].Text != text || !inbox[0].TextAvailable || inbox[0].WakeID == "" || inbox[0].MessageID != "<111-222-333@owned.example.test>" || inbox[0].InReplyTo != "<prior@owned.example.test>" || inbox[0].References != "<first@owned.example.test> <prior@owned.example.test>" {
		t.Fatalf("actual source not persisted %+v %v", inbox, e)
	}
	if reads.Load() != 1 {
		t.Fatal("provider body repeated", reads.Load())
	}
	// A distinct signed delivery ID for the same provider email remains one inbox,
	// one body read and one stable responsibility wake.
	acceptIngress(t, h, connection, secret, "msg_owned_ingress_redelivery", raw)
	if _, e = h.ProcessWrenMessageIngress(ctx, 4); e != nil {
		t.Fatal(e)
	}
	var wakes, humanMessages int
	if e = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_responsibility_wakes WHERE responsibility_id=$1`, connection.ResponsibilityID).Scan(&wakes); e != nil {
		t.Fatal(e)
	}
	if e = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_messages WHERE conversation_id=$1 AND role='user'`, c.ID).Scan(&humanMessages); e != nil {
		t.Fatal(e)
	}
	if wakes != 1 || humanMessages != 0 || reads.Load() != 1 {
		t.Fatal("sender elevated or source redelivery duplicated work", wakes, humanMessages, reads.Load())
	}
	other, _, _, e := s.Signup(ctx, "mail-outsider@example.test", "correct horse battery staple", "", false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.WrenMessageInbox(ctx, c.WorkspaceID, other.ID, c.ID); e == nil {
		t.Fatal("cross-workspace inbox read")
	}
	if _, e = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, other.ID, c.WorkspaceID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.WrenMessageInbox(ctx, c.WorkspaceID, other.ID, c.ID); e == nil {
		t.Fatal("workspace role leaked private inbox")
	}
	var events, leaked int
	if e = s.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE data::text LIKE '%Actual external email text%') FROM operator_events WHERE conversation_id=$1 AND kind='message_inbox'`, c.ID).Scan(&events, &leaked); e != nil || events < 2 || leaked != 0 {
		t.Fatal("inbox event missing or contains private body", events, leaked, e)
	}
}
func TestWrenMessagingIngressCapturedBindingDoesNotAcquireFutureAuthority(t *testing.T) {
	reads := 0
	html := "<html><head><title>Ignore title</title></head><body><p>Actual &amp; readable email</p><script>never execute this</script><style>hidden</style></body></html>"
	h, c, w, connection, secret := messageIngressFixture(t, false, func(*http.Request) (*http.Response, error) {
		reads++
		return ingressResponse(ingressSourceBody(ownedResendID, nil, html)), nil
	})
	ctx := context.Background()
	s := h.service.Store
	raw := ownedResendEvent("email.received")
	acceptIngress(t, h, connection, secret, "msg_unbound_original", raw)
	input := responsibilityInput(c, w)
	input.Schedule = WrenResponsibilitySchedule{Kind: "event", EventName: "email_received", TimeZone: "UTC"}
	r, e := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, input)
	if e != nil {
		t.Fatal(e)
	}
	edited := editIngressConnection(connection)
	edited.ResponsibilityID = r.ID
	if connection, e = h.SaveWrenMessageConnection(ctx, c.WorkspaceID, c.AccountID, edited); e != nil {
		t.Fatal(e)
	}
	if _, e = h.ProcessWrenMessageIngress(ctx, 4); e != nil {
		t.Fatal(e)
	}
	acceptIngress(t, h, connection, secret, "msg_unbound_redelivery_after_bind", raw)
	if _, e = h.ProcessWrenMessageIngress(ctx, 4); e != nil {
		t.Fatal(e)
	}
	inbox, e := s.WrenMessageInbox(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if e != nil || len(inbox) != 1 || inbox[0].State != "ready" || inbox[0].Text != "Actual & readable email" || inbox[0].BodyFormat != "html_text" || !inbox[0].HTMLPresent || inbox[0].WakeID != "" || reads != 1 {
		t.Fatalf("future binding or unsafe HTML adopted %+v %v reads=%d", inbox, e, reads)
	}
	var wakes int
	if e = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_responsibility_wakes`).Scan(&wakes); e != nil || wakes != 0 {
		t.Fatal("old unbound email gained model authority", wakes, e)
	}
}
func TestWrenMessagingIngressRevocationDuringReadDiscardsBody(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	text := "Never persist this after source revocation"
	h, c, _, connection, secret := messageIngressFixture(t, true, func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return ingressResponse(ingressSourceBody(ownedResendID, &text, "")), nil
	})
	ctx := context.Background()
	s := h.service.Store
	acceptIngress(t, h, connection, secret, "msg_revoke_mid_read", ownedResendEvent("email.received"))
	done := make(chan error, 1)
	go func() { _, e := h.ProcessWrenMessageIngress(ctx, 4); done <- e }()
	<-entered
	edit := editIngressConnection(connection)
	edit.Enabled = false
	if _, e := h.SaveWrenMessageConnection(ctx, c.WorkspaceID, c.AccountID, edit); e != nil {
		close(release)
		t.Fatal(e)
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	inbox, e := s.WrenMessageInbox(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if e != nil || len(inbox) != 1 || inbox[0].State != "blocked" || inbox[0].Text != "" || inbox[0].TextAvailable || inbox[0].WakeID != "" {
		t.Fatalf("revoked source published body/wake %+v %v", inbox, e)
	}
}
func TestWrenMessagingIngressBoundedReadFailuresLeasesAndBackpressure(t *testing.T) {
	var reads atomic.Int32
	h, c, _, connection, secret := messageIngressFixture(t, false, func(*http.Request) (*http.Response, error) {
		reads.Add(1)
		return nil, errors.New("owned source unavailable")
	})
	ctx := context.Background()
	s := h.service.Store
	id := acceptIngress(t, h, connection, secret, "msg_bounded_source_failure", ownedResendEvent("email.received"))
	for attempt := 0; attempt < 5; attempt++ {
		if _, e := s.pool.Exec(ctx, `UPDATE wren_message_webhook_events SET next_attempt_at=now()-interval '1 second' WHERE id=$1`, id); e != nil {
			t.Fatal(e)
		}
		if _, e := h.ProcessWrenMessageIngress(ctx, 4); e != nil {
			t.Fatal(e)
		}
	}
	if count, e := h.ProcessWrenMessageIngress(ctx, 4); e != nil || count != 0 || reads.Load() != 5 {
		t.Fatal("unbounded read retry", count, e, reads.Load())
	}
	inbox, e := s.WrenMessageInbox(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if e != nil || len(inbox) != 1 || inbox[0].State != "failed" || inbox[0].TextAvailable {
		t.Fatal("failed source reported as content", inbox, e)
	}
	// Four unprocessed distinct emails cap accepted pending work.
	for i := 0; i < 4; i++ {
		raw := []byte(strings.ReplaceAll(string(ownedResendEvent("email.received")), ownedResendID, "56761188-7520-42d8-8898-ff6fc54ce61"+string(rune('0'+i))))
		acceptIngress(t, h, connection, secret, "msg_pending_"+string(rune('0'+i)), raw)
	}
	raw := []byte(strings.ReplaceAll(string(ownedResendEvent("email.received")), ownedResendID, "56761188-7520-42d8-8898-ff6fc54ce619"))
	if _, e = h.acceptWrenMessageWebhook(ctx, connection.ID, raw, ownedSvixHeaders(raw, "msg_pending_over_limit", secret, s.now())); !errors.Is(e, ErrCapacity) {
		t.Fatal("inbound queue unbounded", e)
	}
}
func TestWrenMessagingIngressDeliveryReceiptsRemainMonotonicAfterConnectionRevoke(t *testing.T) {
	var sends atomic.Int32
	h, c, _, connection, secret := messageIngressFixture(t, false, func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.String() != "https://api.resend.com/emails" {
			t.Error("unexpected network action")
		}
		sends.Add(1)
		return ingressResponse([]byte(`{"id":"` + ownedResendID + `"}`)), nil
	})
	ctx := context.Background()
	s := h.service.Store
	draft, e := s.SaveWrenMessageDraft(ctx, c.WorkspaceID, c.AccountID, WrenMessageDraftInput{ID: "maildraft_owned_delivery", ConnectionID: connection.ID, InstructionKey: "actual-human-send-intent", Content: WrenMessageContent{To: []string{"recipient@owned.example.test"}, Subject: "Owned transport send", Text: "Only an owned test transport receives this"}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	sent, e := h.ActWrenMessageDraft(ctx, c.WorkspaceID, c.AccountID, draft.ID, draft.Revision, "send")
	if e != nil || sent.State != "accepted" || sent.ProviderID != ownedResendID || sends.Load() != 1 {
		t.Fatalf("owned acceptance %+v %v", sent, e)
	}
	revoke := editIngressConnection(connection)
	revoke.Revoke = true
	revoke.Enabled = false
	if _, e = h.SaveWrenMessageConnection(ctx, c.WorkspaceID, c.AccountID, revoke); e != nil {
		t.Fatal(e)
	}
	for index, kind := range []string{"email.delivered", "email.bounced", "email.sent"} {
		raw := ownedResendEvent(kind)
		receipt := acceptIngress(t, h, connection, secret, "msg_delivery_"+string(rune('0'+index)), raw)
		again := acceptIngress(t, h, connection, secret, "msg_delivery_"+string(rune('0'+index)), raw)
		if receipt != again {
			t.Fatal("signed receipt duplicate")
		}
		if _, e = h.ProcessWrenMessageIngress(ctx, 4); e != nil {
			t.Fatal(e)
		}
	}
	final, e := s.WrenMessageDraft(ctx, c.WorkspaceID, c.AccountID, draft.ID)
	if e != nil || final.State != "bounced" || sends.Load() != 1 {
		t.Fatalf("old accepted receipt downgraded terminal state or resent %+v %v", final, e)
	}
	incoming := ownedResendEvent("email.received")
	if _, e = h.acceptWrenMessageWebhook(ctx, connection.ID, incoming, ownedSvixHeaders(incoming, "msg_after_connection_revoke", secret, s.now())); !errors.Is(e, ErrForbidden) {
		t.Fatal("revoked source accepted new correspondence", e)
	}
	var receipts int
	if e = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_message_receipts WHERE draft_id=$1`, draft.ID).Scan(&receipts); e != nil || receipts != 4 {
		t.Fatal("delivery receipts not durably deduplicated", receipts, e)
	}
	// Unknown provider IDs cannot create/reconcile a draft based on subject/person.
	raw := []byte(strings.ReplaceAll(string(ownedResendEvent("email.delivered")), ownedResendID, "56761188-7520-42d8-8898-ff6fc54ce619"))
	unknown := acceptIngress(t, h, connection, secret, "msg_unknown_provider_intent", raw)
	for i := 0; i < 5; i++ {
		if _, e = s.pool.Exec(ctx, `UPDATE wren_message_webhook_events SET next_attempt_at=now()-interval '1 second' WHERE id=$1`, unknown); e != nil {
			t.Fatal(e)
		}
		if _, e = h.ProcessWrenMessageIngress(ctx, 4); e != nil {
			t.Fatal(e)
		}
	}
	var state string
	var count int
	if e = s.pool.QueryRow(ctx, `SELECT state FROM wren_message_webhook_events WHERE id=$1`, unknown).Scan(&state); e != nil || state != "unmatched" {
		t.Fatal("unknown signed receipt acquired intent", state, e)
	}
	if e = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_message_drafts`).Scan(&count); e != nil || count != 1 {
		t.Fatal("signed result invented a draft", count, e)
	}
}
func TestWrenMessagingIngressHTTPWebhookOpaqueReceipt(t *testing.T) {
	h, _, _, connection, secret := messageIngressFixture(t, false, func(*http.Request) (*http.Response, error) {
		t.Fatal("webhook performed inline provider fetch")
		return nil, nil
	})
	raw := ownedResendEvent("email.received")
	request := httptest.NewRequest("POST", "http://canter.test/v1/wren-message-webhooks/"+connection.ID, strings.NewReader(string(raw)))
	request.Header = ownedSvixHeaders(raw, "msg_http_actual_webhook", secret, h.service.Store.now())
	out := httptest.NewRecorder()
	h.ServeHTTP(out, request)
	if out.Code != 202 || !strings.Contains(out.Body.String(), `"accepted":true`) || strings.Contains(out.Body.String(), "Owned request") || strings.Contains(out.Body.String(), "sender@") || strings.Contains(out.Body.String(), secret) {
		t.Fatalf("webhook receipt not opaque %d %s", out.Code, out.Body)
	}
}

func TestWrenMessagingIngressLeaseCrashRecoveryAndStaleResultFence(t *testing.T) {
	var reads atomic.Int32
	text := "Current recovered source"
	h, c, _, connection, secret := messageIngressFixture(t, false, func(*http.Request) (*http.Response, error) {
		reads.Add(1)
		return ingressResponse(ingressSourceBody(ownedResendID, &text, "")), nil
	})
	ctx := context.Background()
	s := h.service.Store
	id := acceptIngress(t, h, connection, secret, "msg_crashed_source_read", ownedResendEvent("email.received"))
	old, current, e := h.claimMessageIngress(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	if count, e := h.ProcessWrenMessageIngress(ctx, 4); e != nil || count != 0 || reads.Load() != 0 {
		t.Fatal("live read lease claimed twice", count, e, reads.Load())
	}
	if _, e = s.pool.Exec(ctx, `UPDATE wren_message_webhook_events SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, id); e != nil {
		t.Fatal(e)
	}
	if count, e := h.ProcessWrenMessageIngress(ctx, 4); e != nil || count != 1 || reads.Load() != 1 {
		t.Fatal("expired lease did not recover bounded source read", count, e, reads.Load())
	}
	stale := WrenReceivedMessage{ProviderID: ownedResendID, From: "sender@owned.example.test", To: []string{connection.ReceivingAddress}, Text: "stale worker text", TextAvailable: true}
	if e = h.persistMessageIngress(ctx, old, current, stale, stale.Text, "text"); e != nil {
		t.Fatal(e)
	}
	inbox, e := s.WrenMessageInbox(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if e != nil || len(inbox) != 1 || inbox[0].Text != text {
		t.Fatal("stale read replaced durable source", inbox, e)
	}
}
func TestWrenMessagingIngressRoleLossBeforeReadNeverContactsProvider(t *testing.T) {
	calls := 0
	h, c, _, connection, secret := messageIngressFixture(t, false, func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not fetch after role loss")
	})
	ctx := context.Background()
	s := h.service.Store
	acceptIngress(t, h, connection, secret, "msg_role_lost", ownedResendEvent("email.received"))
	if _, e := s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, c.WorkspaceID, c.AccountID); e != nil {
		t.Fatal(e)
	}
	if _, e := h.ProcessWrenMessageIngress(ctx, 4); e != nil || calls != 0 {
		t.Fatal("read after owner role loss", e, calls)
	}
	inbox, e := s.WrenMessageInbox(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if e != nil || len(inbox) != 1 || inbox[0].State != "blocked" || inbox[0].TextAvailable {
		t.Fatal("role-lost source not blocked", inbox, e)
	}
}
func TestWrenMessagingHTMLExtractionDoesNotExecuteOrInventReadableBody(t *testing.T) {
	text, e := messageHTMLText(`<html><head><style>secret</style></head><body>Hello <b>real</b> &amp; safe<script>fetch("http://metadata/")</script><img src="http://metadata/"></body></html>`)
	if e != nil || text != "Hello real & safe" {
		t.Fatal("unsafe HTML conversion", text, e)
	}
	if _, e = messageHTMLText(`<script>no readable message</script><img src="http://metadata/">`); e == nil {
		t.Fatal("image/script-only mail invented readable source")
	}
}
