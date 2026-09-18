package controlplane

import (
	"context"
	"encoding/json"
	"github.com/canter0/canter/internal/envfile"
	"github.com/canter0/canter/sdk"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Explicitly opt in with a disposable DB, sandbox env file and a provider
// observation file. Replays allocations over a simulated four-hour interval.
func TestSandboxResourceMeterToInvoice(t *testing.T) {
	path := os.Getenv("CANTER_TEST_OBSERVED_RESOURCES")
	if path == "" {
		t.Skip("real sandbox integration is opt-in")
	}
	if _, err := envfile.Load(); err != nil {
		t.Fatal(err)
	}
	key := os.Getenv("CANTER_STRIPE_SECRET_KEY")
	if !strings.HasPrefix(key, "sk_test_") {
		t.Fatal("requires sandbox secret")
	}
	b := NewBillingGateway(BillingConfig{Enabled: true, SecretKey: key, WebhookSecret: "test", IngestToken: strings.Repeat("x", 32), PaygPriceID: os.Getenv("CANTER_STRIPE_PAYG_PRICE_ID"), ProPriceID: os.Getenv("CANTER_STRIPE_PRO_PRICE_ID"), ProUsagePriceID: os.Getenv("CANTER_STRIPE_PRO_USAGE_PRICE_ID"), MeterID: os.Getenv("CANTER_STRIPE_METER_ID"), MeterEventName: os.Getenv("CANTER_STRIPE_METER_EVENT_NAME"), PortalConfigurationID: os.Getenv("CANTER_STRIPE_PORTAL_CONFIGURATION_ID")})
	ctx := context.Background()
	if err := b.ValidatePrices(ctx); err != nil {
		t.Fatal(err)
	}
	var resources []sdk.BillingResource
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &resources); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Second).Add(-4 * time.Hour)
	var clock, customer struct {
		ID string `json:"id"`
	}
	call := func(method, path string, v url.Values, out any) {
		t.Helper()
		if err := b.request(ctx, method, path, v, "", out); err != nil {
			t.Fatal(path, err)
		}
	}
	call(http.MethodPost, "/v1/test_helpers/test_clocks", url.Values{"frozen_time": {strconv.FormatInt(start.Unix(), 10)}, "name": {"Canter resource metering verification"}}, &clock)
	defer b.request(ctx, http.MethodDelete, "/v1/test_helpers/test_clocks/"+clock.ID, nil, "", nil)
	call(http.MethodPost, "/v1/customers", url.Values{"test_clock": {clock.ID}, "name": {"Canter metering verification"}, "payment_method": {"pm_card_visa"}, "invoice_settings[default_payment_method]": {"pm_card_visa"}}, &customer)
	var sub stripeSubscription
	call(http.MethodPost, "/v1/subscriptions", url.Values{"customer": {customer.ID}, "items[0][price]": {b.Config.PaygPriceID}}, &sub)
	s := integrationStore(t)
	_, w, _, err := s.Signup(ctx, "sandbox-meter@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id,status,period_start,period_end) VALUES($1,$2,$3,'active',$4,$5)`, w.ID, customer.ID, sub.ID, start, start.AddDate(0, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= 240; i++ {
		if err = s.recordResourceSnapshot(ctx, w.ID, resources, start.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	var expected int64
	if err = s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_cents),0) FROM billing_usage_events`).Scan(&expected); err != nil || expected < 1 {
		t.Fatalf("no rated cents %d %v", expected, err)
	}
	call(http.MethodPost, "/v1/test_helpers/test_clocks/"+clock.ID+"/advance", url.Values{"frozen_time": {strconv.FormatInt(start.Add(4*time.Hour).Unix(), 10)}}, nil)
	for i := 0; i < 20; i++ {
		var state struct {
			Status string `json:"status"`
		}
		call(http.MethodGet, "/v1/test_helpers/test_clocks/"+clock.ID, nil, &state)
		if state.Status == "ready" {
			break
		}
		time.Sleep(time.Second)
	}
	if err = b.DispatchUsage(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err = b.DispatchUsage(ctx, s); err != nil {
		t.Fatal(err)
	}
	var invoice struct {
		AmountDue int64 `json:"amount_due"`
	}
	for i := 0; i < 20; i++ {
		call(http.MethodPost, "/v1/invoices/create_preview", url.Values{"customer": {customer.ID}, "subscription": {sub.ID}}, &invoice)
		if invoice.AmountDue == expected {
			t.Logf("Provider-observed allocation replay: ledger=%d cents, Stripe invoice=%d cents; duplicate dispatch unchanged", expected, invoice.AmountDue)
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("invoice=%d ledger=%d", invoice.AmountDue, expected)
}
