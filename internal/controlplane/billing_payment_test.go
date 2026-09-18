package controlplane

import (
	"context"
	"encoding/json"
	"github.com/canter0/canter/sdk"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBillingPaygNeedsNoProPrices(t *testing.T) {
	b, _ := billingTestGateway(t)
	b.Config.ProPriceID, b.Config.ProUsagePriceID = "", ""
	if !b.Ready() {
		t.Fatal("payg-only configuration is not ready")
	}
	if err := b.ValidatePrices(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBillingProvisioningAndSharedAccountIsolation(t *testing.T) {
	s := integrationStore(t)
	b, _ := billingTestGateway(t)
	s.ConfigureBilling(b)
	_, workspace, token, err := s.Signup(context.Background(), "card-gate@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueInitialDeployment(context.Background(), workspace.ID, "unpaid", sdk.ActorRef{}); err == nil || !strings.Contains(err.Error(), "payment method") {
		t.Fatalf("missing card passed provisioning gate: %v", err)
	}
	d := InitialDeploymentDispatcher{Store: s}
	if err = d.execute(context.Background(), InitialDeploymentExecution{WorkspaceID: workspace.ID}); err == nil || !strings.Contains(err.Error(), "payment method") {
		t.Fatalf("worker bypassed payment gate: %v", err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Billing: b})
	resp := requestJSON(t, h, http.MethodPost, "/v1/workspaces/"+workspace.ID+"/billing/checkout", map[string]string{"planId": "unknown"}, &http.Cookie{Name: "canter_session", Value: token})
	if resp.Code != 400 {
		t.Fatalf("unknown subscription allowed: %d", resp.Code)
	}
	body := `{"id":"evt_autodisc_unrelated","type":"invoice.paid","data":{"object":{"customer":"cus_autodisc"}}}`
	r := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", strings.NewReader(body))
	r.Header.Set("Stripe-Signature", billingSignature(body, b.Config.WebhookSecret, time.Now()))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("unrelated customer caused webhook retry: %d %s", w.Code, w.Body.String())
	}
}

func TestBillingCardReadiness(t *testing.T) {
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, status, customer, methodCustomer string
		month, year                            int
		fallback, missing, want                bool
	}{
		{"active card", "active", "cus_one", "cus_one", 9, 2026, false, false, true},
		{"customer fallback", "active", "cus_one", "cus_one", 10, 2026, true, false, true},
		{"missing card", "active", "cus_one", "cus_one", 9, 2026, true, true, false},
		{"expired card", "active", "cus_one", "cus_one", 8, 2026, false, false, false},
		{"past due", "past_due", "cus_one", "cus_one", 9, 2027, false, false, false},
		{"wrong customer", "active", "cus_other", "cus_one", 9, 2027, false, false, false},
		{"detached card", "active", "cus_one", "", 9, 2027, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pm := any(map[string]any{"id": "pm_one", "type": "card", "customer": tc.methodCustomer, "card": map[string]any{"brand": "visa", "last4": "4242", "exp_month": tc.month, "exp_year": tc.year}})
			if tc.missing {
				pm = nil
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/customers/cus_one" {
					json.NewEncoder(w).Encode(map[string]any{"invoice_settings": map[string]any{"default_payment_method": pm}})
					return
				}
				if r.URL.Path != "/v1/subscriptions/sub_one" {
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				subPM := pm
				if tc.fallback {
					subPM = nil
				}
				json.NewEncoder(w).Encode(map[string]any{"id": "sub_one", "customer": tc.customer, "status": tc.status, "collection_method": "charge_automatically", "default_payment_method": subPM, "items": map[string]any{"data": []any{map[string]any{"price": map[string]any{"id": "price_payg"}}}}})
			}))
			defer server.Close()
			b := NewBillingGateway(BillingConfig{PaygPriceID: "price_payg"})
			b.baseURL = server.URL
			_, ready, err := b.paymentReadiness(context.Background(), "cus_one", "sub_one", now)
			if ready != tc.want || tc.want && err != nil {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
}
