package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWrenAIInitialAllowanceIsOperatorOnlyOncePerNewWorkspace(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	_, zero, _, e := s.Signup(ctx, "no-ai-credit@example.test", "correct horse battery staple", "", false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.WrenAIAllowance(ctx, zero.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal("defaultmintedcredit")
	}
	if e = s.SetInitialWrenAIAllowanceMicroUSD(250000); e != nil {
		t.Fatal(e)
	}
	a, w, _, e := s.Signup(ctx, "initial-ai@example.test", "correct horse battery staple", "", false)
	if e != nil {
		t.Fatal(e)
	}
	v, e := s.WrenAIAllowance(ctx, w.ID)
	if e != nil || v.LimitMicroUSD != 250000 || v.SpentMicroUSD != 0 || v.HeldMicroUSD != 0 {
		t.Fatalf("initial %+v %v", v, e)
	}
	c, e := s.CreateConversation(ctx, w.ID, a.ID, "conv_initial_policy", "hello")
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.ReserveWrenAIUsage(ctx, aiTestInput(c, "initial-spend", "operator", 100))
	if e != nil {
		t.Fatal(e)
	}
	s.DispatchWrenAIUsage(ctx, w.ID, r.ID)
	s.SettleWrenAIUsage(ctx, w.ID, r.ID, 20, "real-receipt", nil)
	s.SetInitialWrenAIAllowanceMicroUSD(500000)
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.initializeWrenAIAllowanceTx(ctx, tx, w.ID); e != nil {
		t.Fatal(e)
	}
	tx.Commit(ctx)
	v, _ = s.WrenAIAllowance(ctx, w.ID)
	if v.LimitMicroUSD != 250000 || v.SpentMicroUSD != 20 {
		t.Fatalf("policychange/refill %+v", v)
	}
	_, fresh, _, e := s.Signup(ctx, "fresh-policy@example.test", "correct horse battery staple", "", false)
	if e != nil {
		t.Fatal(e)
	}
	v, _ = s.WrenAIAllowance(ctx, fresh.ID)
	if v.LimitMicroUSD != 500000 {
		t.Fatal("newworkspaceignoredoperatorpolicy")
	}
	for _, n := range []int64{-1, maxInitialWrenAIAllowanceMicroUSD + 1} {
		if e = s.SetInitialWrenAIAllowanceMicroUSD(n); e == nil {
			t.Fatal("invalidoperatorpolicyaccepted")
		}
	}
}

func TestWrenAIInitialAllowanceFailureRollsBackSignup(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	s.SetInitialWrenAIAllowanceMicroUSD(123456)
	if _, e := s.pool.Exec(ctx, `ALTER TABLE wren_ai_allowances ADD CONSTRAINT test_initial_ai_atomicity CHECK(limit_micro_usd<>123456)`); e != nil {
		t.Fatal(e)
	}
	defer s.pool.Exec(ctx, `ALTER TABLE wren_ai_allowances DROP CONSTRAINT test_initial_ai_atomicity`)
	if _, _, _, e := s.Signup(ctx, "atomic-ai@example.test", "correct horse battery staple", "", false); e == nil {
		t.Fatal("injectedbootstrapfailuresucceeded")
	}
	for _, table := range []string{"accounts", "workspaces", "wren_ai_allowances"} {
		var n int
		if e := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != 0 {
			t.Fatalf("partialcreation %s count%d %v", table, n, e)
		}
	}
}

func TestWrenAIOAuthInitialAllowanceAndRepeatLoginNoRefill(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	s.SetInitialWrenAIAllowanceMicroUSD(250000)
	id := oauthIdentity{Provider: "google", Subject: "initial-ai-oauth", Email: "initial-oauth@example.test"}
	token, e := s.signinOAuth(ctx, id, oauthLoginState{Provider: "google"}, false, nil)
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.ResolveHuman(ctx, token)
	if e != nil {
		t.Fatal(e)
	}
	workspaces, e := s.WorkspacesForAccount(ctx, p.Account.ID)
	if e != nil || len(workspaces) != 1 {
		t.Fatal(workspaces, e)
	}
	v, e := s.WrenAIAllowance(ctx, workspaces[0].ID)
	if e != nil || v.LimitMicroUSD != 250000 {
		t.Fatalf("OAuthinitial %+v %v", v, e)
	}
	s.SetInitialWrenAIAllowanceMicroUSD(500000)
	if _, e = s.signinOAuth(ctx, id, oauthLoginState{Provider: "google"}, false, nil); e != nil {
		t.Fatal(e)
	}
	v, _ = s.WrenAIAllowance(ctx, workspaces[0].ID)
	if v.LimitMicroUSD != 250000 {
		t.Fatal("OAuthloginrefilledallowance")
	}
}

func TestWrenAIUsageReadOnlySnapshotScopesAndEstimatedHolds(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 1000)
	r, _ := s.ReserveWrenAIUsage(ctx, aiTestInput(c, "uncertain-view", "operator", 200))
	s.DispatchWrenAIUsage(ctx, c.WorkspaceID, r.ID)
	s.MarkWrenAIUsageUncertain(ctx, c.WorkspaceID, r.ID)
	s.ReserveWrenAIUsage(ctx, aiTestInput(c, "reserved-view", "media", 100))
	spent, _ := s.ReserveWrenAIUsage(ctx, aiTestInput(c, "spent-view", "dictation", 100))
	s.DispatchWrenAIUsage(ctx, c.WorkspaceID, spent.ID)
	s.SettleWrenAIUsage(ctx, c.WorkspaceID, spent.ID, 50, "provider", nil)
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "https://canter.test", AIUsage: WrenAIUsageConfig{Enforce: true}})
	call := func(tok, method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/workspaces/"+c.WorkspaceID+"/ai-usage", nil)
		if tok != "" {
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: tok})
		}
		r.Header.Set("Origin", "https://canter.test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call("", "GET"); w.Code != 401 {
		t.Fatalf("anonymouscostread %d", w.Code)
	}
	w := call(token, "GET")
	var v WrenAIUsageSnapshot
	json.Unmarshal(w.Body.Bytes(), &v)
	if w.Code != 200 || !v.Configured || !v.Enforced || v.LimitMicroUSD != 1000 || v.HeldMicroUSD != 300 || v.SpentMicroUSD != 50 || v.RemainingMicroUSD != 650 || v.UncertainEstimateMicroUSD != 200 || v.ReservedEstimateMicroUSD != 100 || v.ComputeIncluded {
		t.Fatalf("snapshot %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "request_key") || strings.Contains(w.Body.String(), "modelTariffs") || strings.Contains(w.Body.String(), "provider") {
		t.Fatal("financialsnapshotleakedprivatemetadata")
	}
	if w := call(token, "POST"); w.Code != 405 {
		t.Fatal("customerwrotecap")
	}
	other, _, foreign, e := s.Signup(ctx, "usage-outsider@example.test", "correct horse battery staple", "", false)
	if e != nil {
		t.Fatal(e)
	}
	if w := call(foreign, "GET"); w.Code != 403 && w.Code != 404 {
		t.Fatalf("foreigntenantcostread %d", w.Code)
	}
	s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role)VALUES($1,$2,'viewer')`, other.ID, c.WorkspaceID)
	if w := call(foreign, "GET"); w.Code != http.StatusOK {
		t.Fatalf("viewerread %d", w.Code)
	}
}
func TestWrenAIInitialPolicyEnvironmentValidation(t *testing.T) {
	t.Setenv("CANTER_AI_USAGE_POLICY_JSON", "")
	t.Setenv("CANTER_AI_INITIAL_ALLOWANCE_MICRO_USD", "")
	if c := WrenAIUsageConfigFromEnv(); c.InitialAllowanceMicroUSD != 0 {
		t.Fatal("defaultcreditinvented")
	}
	t.Setenv("CANTER_AI_INITIAL_ALLOWANCE_MICRO_USD", "250000")
	if c := WrenAIUsageConfigFromEnv(); c.InitialAllowanceMicroUSD != 250000 {
		t.Fatal("operatorinitialpolicyignored")
	}
	for _, raw := range []string{"garbage", "-1", "1000000000001"} {
		t.Setenv("CANTER_AI_INITIAL_ALLOWANCE_MICRO_USD", raw)
		c := WrenAIUsageConfigFromEnv()
		if c.InitialAllowanceMicroUSD != -1 || !c.Enforce {
			t.Fatal("invalidpolicyfailedopen")
		}
	}
}
