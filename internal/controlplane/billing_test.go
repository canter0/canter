package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func billingSignature(body, secret string, at time.Time) string {
	stamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stamp + "." + body))
	return "t=" + stamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
func TestBillingSignatureRejectsTamperingAndStaleEvents(t *testing.T) {
	now := time.Now()
	body := `{"id":"evt_one"}`
	secret := "whsec_test"
	signature := billingSignature(body, secret, now)
	if !verifyBillingSignature([]byte(body), signature, secret, now) {
		t.Fatal("valid signature rejected")
	}
	for _, tc := range []struct{ body, signature, secret string }{{body + " ", signature, secret}, {body, signature, "wrong"}, {body, signature, ""}, {body, billingSignature(body, secret, now.Add(-6*time.Minute)), secret}, {body, billingSignature(body, secret, now.Add(6*time.Minute)), secret}, {body, "t=invalid,v1=00", secret}} {
		if verifyBillingSignature([]byte(tc.body), tc.signature, tc.secret, now) {
			t.Fatal("unsafe signature accepted")
		}
	}
}

type billingFixture struct {
	mu                     sync.Mutex
	active                 bool
	canceled               bool
	paid                   bool
	wrongCredit            bool
	failMeter              bool
	failCheckout           bool
	customers              int
	checkoutKeys           []string
	sessions               int
	meterCalls             int
	identifiers            map[string]bool
	periodStart, periodEnd int64
}

func billingTestGateway(t *testing.T) (*BillingGateway, *billingFixture) {
	t.Helper()
	f := &billingFixture{paid: true, identifiers: map[string]bool{}, periodStart: time.Now().Add(-time.Hour).Unix(), periodEnd: time.Now().Add(29 * 24 * time.Hour).Unix()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		respond := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Error(err)
			}
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/prices/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/prices/")
			recurring := map[string]any{"interval": "month", "interval_count": 1, "usage_type": "metered", "meter": "mtr_test"}
			p := map[string]any{"active": true, "currency": "usd", "billing_scheme": "per_unit", "unit_amount": 1, "recurring": recurring}
			if id == "price_pro" {
				recurring["usage_type"] = "licensed"
				delete(recurring, "meter")
				p["unit_amount"] = 2000
			}
			if id == "price_overage" {
				credit := 2000
				if f.wrongCredit {
					credit = 1999
				}
				p["billing_scheme"] = "tiered"
				p["tiers_mode"] = "graduated"
				p["tiers"] = []any{map[string]any{"up_to": credit, "unit_amount": 0, "flat_amount": 0}, map[string]any{"up_to": nil, "unit_amount": 1, "flat_amount": 0}}
			}
			respond(p)
		case r.URL.Path == "/v1/billing/meters/mtr_test":
			respond(map[string]any{"event_name": "canter_usage_cents_v1", "status": "active", "default_aggregation": map[string]string{"formula": "sum"}, "customer_mapping": map[string]string{"event_payload_key": "stripe_customer_id", "type": "by_id"}, "value_settings": map[string]string{"event_payload_key": "value"}})
		case r.URL.Path == "/v1/customers":
			f.customers++
			respond(map[string]string{"id": "cus_test"})
		case r.URL.Path == "/v1/subscriptions":
			data := []any{}
			if f.active || f.canceled {
				status := "active"
				invoiceStatus := "open"
				if f.paid {
					invoiceStatus = "paid"
				}
				if f.canceled {
					status = "canceled"
				}
				data = append(data, map[string]any{"id": "sub_test", "customer": "cus_test", "status": status, "latest_invoice": map[string]string{"status": invoiceStatus}, "items": map[string]any{"data": []any{map[string]any{"quantity": 1, "price": map[string]string{"id": "price_pro"}, "current_period_start": f.periodStart, "current_period_end": f.periodEnd}, map[string]any{"price": map[string]string{"id": "price_overage"}, "current_period_start": f.periodStart, "current_period_end": f.periodEnd}}}})
			}
			respond(map[string]any{"data": data})
		case r.URL.Path == "/v1/checkout/sessions":
			r.ParseForm()
			f.checkoutKeys = append(f.checkoutKeys, r.Header.Get("Idempotency-Key"))
			if r.Form.Get("line_items[0][price]") != "price_pro" || r.Form.Get("line_items[1][price]") != "price_overage" || r.Form.Get("line_items[1][quantity]") != "" {
				t.Error("incorrect checkout credit setup")
			}
			if f.failCheckout {
				http.Error(w, "uncertain checkout result", 500)
				return
			}
			f.sessions++
			respond(map[string]any{"id": "cs_test", "url": "https://checkout.stripe.com/c/pay/test", "expires_at": time.Now().Add(time.Hour).Unix()})
		case r.URL.Path == "/v1/billing/meter_events":
			r.ParseForm()
			f.meterCalls++
			if r.Form.Get("payload[value]") != "2100" {
				t.Error("expected full rated usage before credit")
			}
			if f.failMeter {
				http.Error(w, "temporary failure", 500)
				return
			}
			f.identifiers[r.Form.Get("identifier")] = true
			respond(map[string]string{"identifier": r.Form.Get("identifier")})
		default:
			t.Errorf("unexpected Stripe request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	gateway := NewBillingGateway(BillingConfig{Enabled: true, SecretKey: "sk_test_fake", WebhookSecret: "whsec_test", IngestToken: strings.Repeat("x", 32), PaygPriceID: "price_payg", ProPriceID: "price_pro", ProUsagePriceID: "price_overage", MeterID: "mtr_test", MeterEventName: "canter_usage_cents_v1", PortalConfigurationID: "bpc_test"})
	gateway.baseURL = server.URL
	return gateway, f
}
func TestBillingRejectsIncorrectStripeCredit(t *testing.T) {
	b, f := billingTestGateway(t)
	f.wrongCredit = true
	if err := b.ValidatePrices(context.Background()); err == nil {
		t.Fatal("incorrect credit tier accepted")
	}
	f.wrongCredit = false
	if err := b.ValidatePrices(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestBillingRejectsWrongSubscriptionProducts(t *testing.T) {
	b, _ := billingTestGateway(t)
	var sub stripeSubscription
	json.Unmarshal([]byte(`{"items":{"data":[{"price":{"id":"price_pro"},"quantity":2},{"price":{"id":"price_overage"}}]}}`), &sub)
	if _, err := b.subscriptionPlan(sub); err == nil {
		t.Fatal("multiple base fees accepted")
	}
}

func TestBillingCheckoutWebhookAndUsageLifecycle(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	b, f := billingTestGateway(t)
	_, err := store.pool.Exec(ctx, `TRUNCATE billing_webhook_events`)
	if err != nil {
		t.Fatal(err)
	}
	account, workspace, token, err := store.Signup(ctx, "billing@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "canter_session", Value: token}
	var handler http.Handler = NewHTTPServer(&Service{Store: store}, HTTPConfig{PublicURL: "http://canter.test", Billing: b})
	path := "/v1/workspaces/" + workspace.ID + "/billing"
	// No auth, another workspace, and a viewer cannot open a checkout.
	noAuth := requestJSON(t, handler, http.MethodPost, path+"/checkout", map[string]string{"planId": "pro"}, nil)
	if noAuth.Code != 401 {
		t.Fatalf("unauthenticated checkout: %d", noAuth.Code)
	}
	other, otherWorkspace, otherToken, err := store.Signup(ctx, "billing-other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	denied := requestJSON(t, handler, http.MethodPost, "/v1/workspaces/"+otherWorkspace.ID+"/billing/checkout", map[string]string{"planId": "pro"}, cookie)
	if denied.Code != 403 && denied.Code != 404 {
		t.Fatalf("cross workspace checkout: %d", denied.Code)
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, other.ID, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	denied = requestJSON(t, handler, http.MethodPost, path+"/checkout", map[string]string{"planId": "pro"}, &http.Cookie{Name: "canter_session", Value: otherToken})
	if denied.Code != 403 {
		t.Fatalf("viewer checkout: %d", denied.Code)
	}
	// A bearer principal must not get payment authority, even with owner data.
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path+"/checkout", strings.NewReader(`{"planId":"pro"}`))
	req.Header.Set("Authorization", "Bearer agent")
	req.Header.Set("Origin", "http://canter.test")
	handler.(*HTTPServer).workspaceBilling(recorder, req, Principal{Account: &account}, workspace.ID, []string{"checkout"})
	if recorder.Code != 403 {
		t.Fatal("bearer checkout accepted")
	}
	// Same-origin remains required for owner-cookie mutations.
	recorder = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, path+"/checkout", strings.NewReader(`{"planId":"pro"}`))
	req.AddCookie(cookie)
	req.Header.Set("Origin", "https://evil.example")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != 403 {
		t.Fatal("cross-origin checkout accepted")
	}
	// A failed Checkout must retain its customer association. Otherwise a
	// later retry could create a second customer with an untracked subscription.
	f.failCheckout = true
	failed := requestJSON(t, handler, http.MethodPost, path+"/checkout", map[string]string{"planId": "pro"}, cookie)
	if failed.Code < 400 {
		t.Fatalf("expected provider failure: %d", failed.Code)
	}
	var linkedCustomer string
	if err := store.pool.QueryRow(ctx, `SELECT customer_id FROM workspace_billing WHERE workspace_id=$1`, workspace.ID).Scan(&linkedCustomer); err != nil || linkedCustomer != "cus_test" {
		t.Fatalf("customer link lost after checkout failure: %q %v", linkedCustomer, err)
	}
	f.failCheckout = false
	for i := 0; i < 2; i++ {
		response := requestJSON(t, handler, http.MethodPost, path+"/checkout", map[string]string{"planId": "pro"}, cookie)
		if response.Code != 200 {
			t.Fatalf("checkout: %d %s", response.Code, response.Body.String())
		}
	}
	if f.sessions != 1 {
		t.Fatalf("duplicate checkout sessions: %d", f.sessions)
	}
	if f.customers != 1 || len(f.checkoutKeys) != 2 || f.checkoutKeys[0] == "" || f.checkoutKeys[0] != f.checkoutKeys[1] {
		t.Fatal("checkout retry created a different customer or idempotency key")
	}
	state, err := store.billingState(ctx, workspace.ID)
	if err != nil || state.Status != "not_started" {
		t.Fatal("checkout creation activated a plan")
	}
	webhook := func(id string) int {
		body := fmt.Sprintf(`{"id":%q,"type":"invoice.paid","data":{"object":{"customer":"cus_test"}}}`, id)
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", strings.NewReader(body))
		req.Header.Set("Stripe-Signature", billingSignature(body, b.Config.WebhookSecret, time.Now()))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Log(rec.Body.String())
		}
		return rec.Code
	}
	f.active = true
	f.paid = false
	if got := webhook("evt_unpaid"); got != 200 {
		t.Fatal(got)
	}
	state, _ = store.billingState(ctx, workspace.ID)
	if state.Status == "active" {
		t.Fatal("unpaid Pro became active")
	}
	f.paid = true
	for i := 0; i < 2; i++ {
		if got := webhook("evt_paid"); got != 200 {
			t.Fatal(got)
		}
	}
	state, err = store.billingState(ctx, workspace.ID)
	if err != nil || state.Status != "active" || state.PlanID != "pro" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	denied = requestJSON(t, handler, http.MethodPost, path+"/checkout", map[string]string{"planId": "pro"}, cookie)
	if denied.Code != 409 {
		t.Fatal("second subscription was allowed")
	}
	event := BillingUsageEvent{ID: "usage-test-0001", WorkspaceID: workspace.ID, AmountCents: 2100, Resource: "app/example", RateVersion: "reviewed-quote-test", OccurredAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Second)}
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- store.RecordBillingUsage(ctx, event) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	changed := event
	changed.AmountCents++
	if err := store.RecordBillingUsage(ctx, changed); err == nil {
		t.Fatal("conflicting retry accepted")
	}
	invalid := event
	invalid.ID = "usage-negative"
	invalid.AmountCents = -1
	if err := store.RecordBillingUsage(ctx, invalid); err == nil {
		t.Fatal("negative usage accepted")
	}
	invalid = event
	invalid.ID = "usage-future"
	invalid.OccurredAt = time.Now().Add(time.Hour)
	if err := store.RecordBillingUsage(ctx, invalid); err == nil {
		t.Fatal("future usage accepted")
	}
	state, err = store.billingState(ctx, workspace.ID)
	if err != nil || state.Bill.TotalCents != 2100 || state.Bill.AdditionalUsageCents != 100 || state.PendingEvents != 1 {
		t.Fatalf("incorrect credit or duplicate usage: %+v %v", state, err)
	}
	f.failMeter = true
	if err := b.DispatchUsage(ctx, store); err == nil {
		t.Fatal("expected provider failure")
	}
	state, _ = store.billingState(ctx, workspace.ID)
	if state.PendingEvents != 1 {
		t.Fatal("failed usage disappeared")
	}
	f.failMeter = false
	if err := b.DispatchUsage(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := b.DispatchUsage(ctx, store); err != nil {
		t.Fatal(err)
	}
	if len(f.identifiers) != 1 || f.meterCalls != 2 {
		t.Fatalf("duplicate provider usage: %+v", f)
	}
	// Renewal starts a new credit allowance, while historical events remain.
	f.periodStart = time.Now().Unix()
	f.periodEnd = time.Now().Add(30 * 24 * time.Hour).Unix()
	if got := webhook("evt_renewed"); got != 200 {
		t.Fatal(got)
	}
	state, _ = store.billingState(ctx, workspace.ID)
	if state.Bill.UsageCents != 0 || state.Bill.CreditRemainingCents != 2000 {
		t.Fatal("prior cycle usage leaked into renewal")
	}
	if err := store.RecordBillingUsage(ctx, event); err != nil {
		t.Fatal("accepted retry failed after renewal", err)
	}
	f.canceled = true
	f.active = false
	if got := webhook("evt_deleted"); got != 200 {
		t.Fatal(got)
	}
	if got := webhook("evt_old_delivered_late"); got != 200 {
		t.Fatal(got)
	}
	state, _ = store.billingState(ctx, workspace.ID)
	if state.Status != "canceled" {
		t.Fatal("stale webhook resurrected subscription")
	}
}

func TestBillingUsageIngestRequiresPrivateToken(t *testing.T) {
	b, _ := billingTestGateway(t)
	handler := NewHTTPServer(&Service{}, HTTPConfig{Billing: b})
	req := httptest.NewRequest(http.MethodPost, "/v1/billing/usage", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer agent-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("untrusted usage ingestion: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal("unsigned webhook accepted")
	}
}
