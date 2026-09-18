package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBillingPlanSwitchPreservesPeriodAndCredit(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	_, ws, token, err := s.Signup(ctx, "plans@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	start, end := time.Now().Add(-time.Hour).Unix(), time.Now().Add(29*24*time.Hour).Unix()
	_, err = s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id,plan_id,status,period_start,period_end) VALUES($1,'cus_plans','sub_plans','payg','active',$2,$3)`, ws.ID, time.Unix(start, 0), time.Unix(end, 0))
	if err != nil {
		t.Fatal(err)
	}
	scheduled, updates, releases := false, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		schedule := map[string]any{"id": "sub_sched_one", "status": "active", "metadata": map[string]string{"canter_workspace_id": ws.ID}, "current_phase": map[string]int64{"start_date": start, "end_date": end}, "phases": []any{map[string]any{"start_date": end, "metadata": map[string]string{"canter_plan": "pro"}}}}
		switch r.URL.Path {
		case "/v1/subscriptions/sub_plans":
			var active any
			if scheduled {
				active = schedule
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "sub_plans", "customer": "cus_plans", "status": "active", "schedule": active, "items": map[string]any{"data": []any{map[string]any{"price": map[string]string{"id": "price_payg"}, "current_period_start": start, "current_period_end": end}}}})
		case "/v1/subscription_schedules":
			r.ParseForm()
			if r.Form.Get("from_subscription") != "sub_plans" {
				t.Error("created a second subscription")
			}
			scheduled = true
			json.NewEncoder(w).Encode(schedule)
		case "/v1/subscription_schedules/sub_sched_one":
			r.ParseForm()
			updates++
			want := map[string]string{"phases[0][start_date]": strconv.FormatInt(start, 10), "phases[0][end_date]": strconv.FormatInt(end, 10), "phases[0][items][0][price]": "price_payg", "phases[1][start_date]": strconv.FormatInt(end, 10), "phases[1][items][0][price]": "price_pro", "phases[1][items][0][quantity]": "1", "phases[1][items][1][price]": "price_overage", "proration_behavior": "none", "phases[1][proration_behavior]": "none", "end_behavior": "release"}
			for k, v := range want {
				if r.Form.Get(k) != v {
					t.Errorf("%s=%q, want %q", k, r.Form.Get(k), v)
				}
			}
			json.NewEncoder(w).Encode(schedule)
		case "/v1/subscription_schedules/sub_sched_one/release":
			releases++
			scheduled = false
			json.NewEncoder(w).Encode(map[string]string{"status": "released"})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	b, _ := billingTestGateway(t)
	b.baseURL = server.URL
	b.validated = true
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Billing: b}).(*HTTPServer)
	if err = h.changeBillingPlan(ctx, ws.ID, "pro", end-1); err == nil {
		t.Fatal("stale period accepted")
	}
	if scheduled {
		t.Fatal("stale request mutated Stripe")
	}
	path := "/v1/workspaces/" + ws.ID + "/billing/plan-change"
	denied := requestJSON(t, h, http.MethodPost, path, map[string]any{"planId": "pro", "periodEnd": end}, nil)
	if denied.Code != 401 {
		t.Fatal("anonymous plan change accepted")
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"planId":"pro"}`))
	r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin plan change accepted")
	}
	resp := requestJSON(t, h, http.MethodPost, path, map[string]any{"planId": "pro", "periodEnd": end}, &http.Cookie{Name: "canter_session", Value: token})
	if resp.Code != 200 {
		t.Fatalf("%d %s", resp.Code, resp.Body.String())
	}
	plan, at, err := h.pendingBillingPlan(ctx, ws.ID)
	if err != nil || plan != "pro" || at.Unix() != end {
		t.Fatalf("pending=%s %v %v", plan, at, err)
	}
	state, err := s.billingState(ctx, ws.ID)
	if err != nil || state.PlanID != "payg" {
		t.Fatal("scheduled change immediately changed plan")
	}
	if err = h.changeBillingPlan(ctx, ws.ID, "payg", end); err != nil {
		t.Fatal(err)
	}
	if updates != 1 || releases != 1 {
		t.Fatalf("updates=%d releases=%d", updates, releases)
	}
}
