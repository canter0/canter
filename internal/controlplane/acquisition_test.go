package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func acquisitionRequest(h http.Handler, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/acquisition", strings.NewReader(body))
	r.Header.Set("Origin", "http://canter.test")
	r.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func captureAcquisition(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	w := acquisitionRequest(h, `{"source":"google","landingPath":"/pricing"}`)
	cookies := w.Result().Cookies()
	if w.Code != 204 || len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].MaxAge != 30*24*60*60 {
		t.Fatalf("capture: %d %s cookies=%d", w.Code, w.Body.String(), len(cookies))
	}
	return cookies[0]
}

func acquisitionTestServer(t *testing.T) (*Store, *HTTPServer) {
	t.Helper()
	s := integrationStore(t)
	if _, err := s.pool.Exec(context.Background(), `TRUNCATE acquisition_visits,acquisition_daily,account_acquisition,billing_paid_invoices,billing_webhook_events`); err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	return s, h
}

func TestAcquisitionFirstTouchBoundariesAndExpiry(t *testing.T) {
	s, h := acquisitionTestServer(t)
	ctx := context.Background()
	cookie := captureAcquisition(t, h)
	if w := acquisitionRequest(h, `{"source":"direct","landingPath":"/"}`, cookie); w.Code != 204 || len(w.Result().Cookies()) != 0 {
		t.Fatal("repeat visit changed first touch")
	}
	var source, path string
	var visitors int
	if err := s.pool.QueryRow(ctx, `SELECT source,landing_path,visitors FROM acquisition_daily`).Scan(&source, &path, &visitors); err != nil || source != "google" || path != "/pricing" || visitors != 1 {
		t.Fatal(source, path, visitors, err)
	}
	for _, body := range []string{`{"source":"google","landingPath":"/approve/secret"}`, `{"source":"https://google.com/?secret=yes","landingPath":"/"}`, `{"source":"google","landingPath":"/?token=secret"}`, `{"source":"direct","landingPath":"/","token":"secret"}`} {
		if w := acquisitionRequest(h, body); w.Code != 400 {
			t.Fatalf("accepted private or unknown input: %d", w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/acquisition", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://attacker.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin capture accepted")
	}
	for _, header := range []string{"DNT", "Sec-GPC"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/acquisition", strings.NewReader(`{}`))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set(header, "1")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 204 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge != -1 {
			t.Fatal("privacy opt-out ignored")
		}
	}
	before := s.now()
	s.now = func() time.Time { return before.Add(31 * 24 * time.Hour) }
	w = acquisitionRequest(h, `{"source":"bing","landingPath":"/"}`, cookie)
	if w.Code != 204 || len(w.Result().Cookies()) != 1 {
		t.Fatal("expired visit not replaced")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM acquisition_visits`).Scan(&count); err != nil || count != 1 {
		t.Fatal("expired identifiers retained", count, err)
	}
}

func TestAcquisitionSignupFunnelAndSettledPaymentDeduplication(t *testing.T) {
	s, h := acquisitionTestServer(t)
	ctx := context.Background()
	cookie := captureAcquisition(t, h)
	signup := httptest.NewRequest(http.MethodPost, "/v1/auth/signup/finish", nil)
	signup.AddCookie(cookie)
	w := signupHTTP(t, h, "seo-test@example.com", cookie)
	if w.Code != 201 {
		t.Fatalf("signup %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Account   Account   `json:"account"`
		Workspace Workspace `json:"workspace"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	session := w.Result().Cookies()[0]
	h.claimAcquisition(signup, session.Value)
	var account, workspace string
	if err := s.pool.QueryRow(ctx, `SELECT account_id,workspace_id FROM account_acquisition`).Scan(&account, &workspace); err != nil || account != out.Account.ID || workspace != out.Workspace.ID {
		t.Fatal("signup attribution missing", err)
	}
	assertFunnel := func(deployed, usage, paid, gross int) {
		t.Helper()
		var visitors, signups, gotDeployed, gotUsage, gotPaid, gotGross int
		err := s.pool.QueryRow(ctx, `SELECT visitors,signups,deployed_workspaces,usage_workspaces,paid_workspaces,gross_paid_usd_cents FROM acquisition_funnel_daily WHERE source='google' AND landing_path='/pricing'`).Scan(&visitors, &signups, &gotDeployed, &gotUsage, &gotPaid, &gotGross)
		if err != nil || visitors != 1 || signups != 1 || gotDeployed != deployed || gotUsage != usage || gotPaid != paid || gotGross != gross {
			t.Fatalf("funnel %d %d %d %d %d %d: %v", visitors, signups, gotDeployed, gotUsage, gotPaid, gotGross, err)
		}
	}
	assertFunnel(0, 0, 0, 0)
	if _, err := s.pool.Exec(ctx, `INSERT INTO initial_deployments(id,workspace_id,system_name,phase,summary,digest,document,created_at,updated_at) VALUES('seo-deploy',$1,'seo','failed','','','{}',now(),now())`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO initial_deployment_executions(id,workspace_id,deployment_id,system_name,phase,requested_by_kind,requested_by_id) VALUES('seo-failed',$1,'seo-deploy','seo','failed','human',$2)`, workspace, account); err != nil {
		t.Fatal(err)
	}
	assertFunnel(0, 0, 0, 0)
	if _, err := s.pool.Exec(ctx, `INSERT INTO initial_deployment_executions(id,workspace_id,deployment_id,system_name,phase,requested_by_kind,requested_by_id) VALUES('seo-success',$1,'seo-deploy','seo','succeeded','human',$2),('seo-success-2',$1,'seo-deploy','seo','succeeded','human',$2)`, workspace, account); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id,status) VALUES($1,'cus_test','sub_test','active')`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO billing_usage_events(id,workspace_id,customer_id,subscription_id,amount_cents,resource,rate_version,occurred_at) VALUES('seo-usage',$1,'cus_test','sub_test',10,'compute','test',now())`, workspace); err != nil {
		t.Fatal(err)
	}
	assertFunnel(1, 1, 0, 0)
	gateway, fixture := billingTestGateway(t)
	fixture.active = true
	h.config.Billing = gateway
	webhook := func(event, invoice, kind, customer, status string, amount int) {
		t.Helper()
		body := fmt.Sprintf(`{"id":%q,"type":%q,"data":{"object":{"id":%q,"customer":%q,"status":%q,"amount_paid":%d,"currency":"usd","status_transitions":{"paid_at":%d}}}}`, event, kind, invoice, customer, status, amount, time.Now().Unix())
		r := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", strings.NewReader(body))
		r.Header.Set("Stripe-Signature", billingSignature(body, gateway.Config.WebhookSecret, time.Now()))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("webhook: %d %s", w.Code, w.Body.String())
		}
	}
	webhook("evt_zero", "in_zero", "invoice.paid", "cus_test", "paid", 0)
	webhook("evt_checkout", "cs_one", "checkout.session.completed", "cus_test", "paid", 2000)
	webhook("evt_other_customer", "in_other", "invoice.paid", "cus_other_app", "paid", 2000)
	assertFunnel(1, 1, 0, 0)
	webhook("evt_paid", "in_paid", "invoice.paid", "cus_test", "paid", 2000)
	webhook("evt_paid", "in_paid", "invoice.paid", "cus_test", "paid", 2000)
	webhook("evt_paid_duplicate", "in_paid", "invoice.paid", "cus_test", "paid", 2000)
	assertFunnel(1, 1, 1, 2000)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("repeat migration", err)
	}
	assertFunnel(1, 1, 1, 2000)
}

func TestAcquisitionDoesNotClaimExistingAccountsOrExpiredVisits(t *testing.T) {
	s, h := acquisitionTestServer(t)
	ctx := context.Background()
	_, _, session, err := s.Signup(ctx, "existing@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	cookie := captureAcquisition(t, h)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(cookie)
	h.claimAcquisition(r, session)
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM account_acquisition`).Scan(&count); err != nil || count != 0 {
		t.Fatal("existing account counted as signup", count, err)
	}
	now := s.now()
	s.now = func() time.Time { return now.Add(31 * 24 * time.Hour) }
	_, _, session, err = s.Signup(ctx, "later@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	h.claimAcquisition(r, session)
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM account_acquisition`).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired visit claimed", count, err)
	}
}
