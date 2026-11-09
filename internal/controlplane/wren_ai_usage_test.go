package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func aiTestInput(c Conversation, key, kind string, n int64) WrenAIReservationInput {
	return WrenAIReservationInput{WorkspaceID: c.WorkspaceID, AccountID: c.AccountID, ConversationID: c.ID, RequestKey: key, Kind: kind, Model: "test-model", AmountMicroUSD: n}
}
func TestWrenAISharedAllowanceSerializesConcurrentFanout(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	if e := s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 100); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted, denied := 0, 0
	kinds := []string{"operator", "acp", "dictation", "media"}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := s.ReserveWrenAIUsage(ctx, aiTestInput(c, fmt.Sprintf("physical-%d", i), kinds[i%4], 10))
			mu.Lock()
			defer mu.Unlock()
			if e == nil {
				accepted++
			} else if errors.Is(e, ErrCapacity) {
				denied++
			} else {
				t.Errorf("reserve %v", e)
			}
		}(i)
	}
	wg.Wait()
	v, e := s.WrenAIAllowance(ctx, c.WorkspaceID)
	if e != nil || accepted != 10 || denied != 6 || v.HeldMicroUSD != 100 || v.SpentMicroUSD != 0 {
		t.Fatalf("sharedcap %+v accepted%d denied%d %v", v, accepted, denied, e)
	}
	// A child conversation is not a fresh budget.
	child, e := s.CreateConversation(ctx, c.WorkspaceID, c.AccountID, "conv_budget_child", "Child")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ReserveWrenAIUsage(ctx, aiTestInput(child, "new-child", "acp", 1)); !errors.Is(e, ErrCapacity) {
		t.Fatalf("child reset shared allowance %v", e)
	}
	reopened, e := Open(ctx, os.Getenv("CANTER_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	v, e = reopened.WrenAIAllowance(ctx, c.WorkspaceID)
	if e != nil || v.HeldMicroUSD != 100 {
		t.Fatalf("restartlostholds %+v %v", v, e)
	}
}
func TestWrenAIUncertainCostsRetainedAndSettlementExactlyOnce(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 100)
	in := aiTestInput(c, "once", "operator", 50)
	r, e := s.ReserveWrenAIUsage(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	same, e := s.ReserveWrenAIUsage(ctx, in)
	if e != nil || same.ID != r.ID {
		t.Fatalf("reservation duplicated %v", e)
	}
	changed := in
	changed.Kind = "acp"
	if _, e = s.ReserveWrenAIUsage(ctx, changed); !errors.Is(e, ErrConflict) {
		t.Fatal("request identity changed")
	}
	if e = s.DispatchWrenAIUsage(ctx, c.WorkspaceID, r.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.DispatchWrenAIUsage(ctx, c.WorkspaceID, r.ID); !errors.Is(e, ErrConflict) {
		t.Fatal("duplicate dispatch allowed")
	}
	if e = s.MarkWrenAIUsageUncertain(ctx, c.WorkspaceID, r.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.ReleaseWrenAIUsage(ctx, c.WorkspaceID, r.ID); !errors.Is(e, ErrConflict) {
		t.Fatal("timedout paid request refunded")
	}
	v, _ := s.WrenAIAllowance(ctx, c.WorkspaceID)
	if v.HeldMicroUSD != 50 || v.SpentMicroUSD != 0 {
		t.Fatalf("unknowncost vanished %+v", v)
	}
	receipt := json.RawMessage(`{"cost":0.00002}`)
	for i := 0; i < 2; i++ {
		if e = s.SettleWrenAIUsage(ctx, c.WorkspaceID, r.ID, 20, "provider-request", receipt); e != nil {
			t.Fatal(e)
		}
	}
	v, _ = s.WrenAIAllowance(ctx, c.WorkspaceID)
	if v.HeldMicroUSD != 0 || v.SpentMicroUSD != 20 {
		t.Fatalf("double settled %+v", v)
	}
	if e = s.SettleWrenAIUsage(ctx, c.WorkspaceID, r.ID, 19, "provider-request", receipt); !errors.Is(e, ErrConflict) {
		t.Fatal("rewrote actual receipt")
	}
	unused, e := s.ReserveWrenAIUsage(ctx, aiTestInput(c, "never-sent", "media", 20))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ReleaseWrenAIUsage(ctx, c.WorkspaceID, unused.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.ReleaseWrenAIUsage(ctx, c.WorkspaceID, unused.ID); e != nil {
		t.Fatal(e)
	}
	v, _ = s.WrenAIAllowance(ctx, c.WorkspaceID)
	if v.SpentMicroUSD != 20 || v.HeldMicroUSD != 0 {
		t.Fatalf("preflight refund wrong %+v", v)
	}
	over, e := s.ReserveWrenAIUsage(ctx, aiTestInput(c, "overrun", "acp", 10))
	if e != nil {
		t.Fatal(e)
	}
	s.DispatchWrenAIUsage(ctx, c.WorkspaceID, over.ID)
	if e = s.SettleWrenAIUsage(ctx, c.WorkspaceID, over.ID, 120, "overrun", nil); e != nil {
		t.Fatal(e)
	}
	v, _ = s.WrenAIAllowance(ctx, c.WorkspaceID)
	if v.SpentMicroUSD != 140 {
		t.Fatalf("actual cost clamped %+v", v)
	}
	if _, e = s.ReserveWrenAIUsage(ctx, aiTestInput(c, "future", "dictation", 1)); !errors.Is(e, ErrCapacity) {
		t.Fatal("overrun failed to stop future costs")
	}
	s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 100)
	v, _ = s.WrenAIAllowance(ctx, c.WorkspaceID)
	if v.SpentMicroUSD != 140 {
		t.Fatal("configure reset spent money")
	}
}
func TestWrenAIReservationACLAndUnconfiguredPolicy(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	if _, e := s.ReserveWrenAIUsage(ctx, aiTestInput(c, "notconfigured", "operator", 1)); !errors.Is(e, ErrCapacity) {
		t.Fatalf("missingpolicy guessed %v", e)
	}
	s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 100)
	in := aiTestInput(c, "wrongscope", "operator", 1)
	in.ConversationID = "foreign"
	if _, e := s.ReserveWrenAIUsage(ctx, in); !errors.Is(e, ErrNotFound) {
		t.Fatal("foreign conversation allowed")
	}
	s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, c.WorkspaceID, c.AccountID)
	if _, e := s.ReserveWrenAIUsage(ctx, aiTestInput(c, "viewer", "dictation", 1)); !errors.Is(e, ErrForbidden) {
		t.Fatal("viewer paid use allowed")
	}
}
func TestWrenAIActualCostRounding(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), -1} {
		if _, e := WrenAICostMicroUSD(v); e == nil {
			t.Fatal("invalid cost")
		}
	}
	if n, e := WrenAICostMicroUSD(.00001569762); e != nil || n != 16 {
		t.Fatalf("subcent provider charge lost %d %v", n, e)
	}
}

func TestWrenAIModelHTTPHooksReserveEveryPhysicalRetryAndSettleActual(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 1_000_000)
	calls := 0
	fail := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if fail {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"generation-test\",\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"cost\":0.00001569762}}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	policy := WrenAIUsageConfig{Enforce: true, ModelTariffs: map[string]WrenAIModelTariff{"test-model": {InputMicroUSDPerMillionTokens: 100000, OutputMicroUSDPerMillionTokens: 500000, MaxInputBytes: 1 << 20, MaxOutputTokens: 16384, Source: "testverifiedtariff", ObservedAt: "2026-10-26T04:27:23Z"}}}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{AIUsage: policy}).(*HTTPServer)
	s.EnqueueOperator(ctx, c, "ai-model-http-parent", "hello", "test-model", nil)
	actualRun, ok, claimErr := s.claimOperator(ctx)
	if claimErr != nil || !ok {
		t.Fatal("actualrunclaim", claimErr)
	}
	config := h.withWrenAIModelUsage(OperatorConfig{APIKey: "test", BaseURL: provider.URL, Model: "test-model", modelRetryDelay: time.Millisecond}, c, actualRun)
	if _, e := config.completeWithLimit(ctx, []modelMessage{{Role: "user", Content: "hello"}}, nil, 64, func(string) error { return nil }); e != nil {
		t.Fatal(e)
	}
	a, _ := s.WrenAIAllowance(ctx, c.WorkspaceID)
	if a.SpentMicroUSD != 16 || a.HeldMicroUSD != 0 || calls != 1 {
		t.Fatalf("actual settlement %+v calls%d", a, calls)
	}
	fail = true
	if _, e := config.complete(ctx, []modelMessage{{Role: "user", Content: "retry"}}, nil, func(string) error { return nil }); e == nil {
		t.Fatal("acceptedfailedprovider")
	}
	a, _ = s.WrenAIAllowance(ctx, c.WorkspaceID)
	var unknown int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_ai_reservations WHERE workspace_id=$1 AND state='uncertain'`, c.WorkspaceID).Scan(&unknown)
	if calls != 4 || unknown != 3 || a.HeldMicroUSD <= 0 || a.SpentMicroUSD != 16 {
		t.Fatalf("physicalretrybudget %+v calls%d unknown%d", a, calls, unknown)
	}
	config.Model = "unpriced-model"
	if _, e := config.completeWithLimit(ctx, []modelMessage{{Role: "user", Content: "cannotbill"}}, nil, 64, func(string) error { return nil }); !errors.Is(e, ErrCapacity) || calls != 4 {
		t.Fatalf("unconfiguredcostreachedprovider %v calls%d", e, calls)
	}
}
func TestWrenAIEstimateRequiresExplicitMediaAndImagePolicies(t *testing.T) {
	p := WrenAIUsageConfig{Enforce: true, ModelTariffs: map[string]WrenAIModelTariff{"m": {InputMicroUSDPerMillionTokens: 100000, OutputMicroUSDPerMillionTokens: 500000, MaxInputBytes: 1024, MaxOutputTokens: 4096, Source: "officialcatalog", ObservedAt: "2026-10-26T04:27:23Z"}}}
	for _, in := range []WrenAIRequestEstimate{{Kind: "media"}, {Kind: "dictation"}, {Kind: "operator", Model: "m", InputBytes: 50, MaxOutputTokens: 64, ImageCount: 1}, {Kind: "operator", Model: "m", InputBytes: 2048, MaxOutputTokens: 64}} {
		if _, _, e := p.estimate(in); !errors.Is(e, ErrCapacity) {
			t.Fatalf("missingcostpolicyallowed %+v %v", in, e)
		}
	}
	if _, e := wrenAIModelImageCount([]byte(`{"messages":[{"content":[{"type":"file","file":{"url":"https://example.com/private.pdf"}}]}]}`)); !errors.Is(e, ErrCapacity) {
		t.Fatal("unpricedfilemediaallowed")
	}
	if n, e := wrenAIModelImageCount([]byte(`{"messages":[{"content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]}`)); e != nil || n != 1 {
		t.Fatalf("imageclassifier %d %v", n, e)
	}
}
func TestWrenAIAudioSettlesSharedAllowanceAndBlocksMissingPolicy(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 1000)
	calls := 0
	config := HTTPConfig{PublicURL: "https://canter.test", AIUsage: WrenAIUsageConfig{Enforce: true, KindEstimatesMicroUSD: map[string]int64{"dictation": 100}}, Audio: WrenAudioConfig{APIKey: "test", transport: audioRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"text":"draft","usage":{"cost":0.00001569762}}`))}, nil
	})}}
	call := func(cfg HTTPConfig) *httptest.ResponseRecorder {
		h := NewHTTPServer(&Service{Store: s}, cfg)
		r := httptest.NewRequest("POST", "/v1/workspaces/"+c.WorkspaceID+"/conversations/"+c.ID+"/dictation", strings.NewReader(string(audioTestWAV(1))))
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		r.Header.Set("Origin", "https://canter.test")
		r.Header.Set("Content-Type", "audio/wav")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call(config); w.Code != 200 {
		t.Fatalf("audiotranscript %d %s", w.Code, w.Body.String())
	}
	a, _ := s.WrenAIAllowance(ctx, c.WorkspaceID)
	if a.SpentMicroUSD != 16 || a.HeldMicroUSD != 0 {
		t.Fatalf("STTnotshared %+v", a)
	}
	config.AIUsage.KindEstimatesMicroUSD = nil
	if w := call(config); w.Code != 429 || calls != 1 {
		t.Fatalf("unpricedSTTbilled %d calls%d", w.Code, calls)
	}
}

func TestWrenAIClosedAllowancePreservesUnknownAndAllowsReconciliation(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 100)
	r, e := s.ReserveWrenAIUsage(ctx, aiTestInput(c, "closing-paid", "dictation", 50))
	if e != nil {
		t.Fatal(e)
	}
	s.DispatchWrenAIUsage(ctx, c.WorkspaceID, r.ID)
	s.MarkWrenAIUsageUncertain(ctx, c.WorkspaceID, r.ID)
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CloseWrenAIAllowanceTx(ctx, tx, c.WorkspaceID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	a, _ := s.WrenAIAllowance(ctx, c.WorkspaceID)
	if a.ClosedAt == nil || a.HeldMicroUSD != 50 {
		t.Fatalf("closureerasedholds %+v", a)
	}
	if e = s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 200); !errors.Is(e, ErrForbidden) {
		t.Fatal("closedcapreopened")
	}
	if _, e = s.ReserveWrenAIUsage(ctx, aiTestInput(c, "closed-new", "media", 1)); !errors.Is(e, ErrForbidden) {
		t.Fatal("closedworkspacebilled")
	}
	if e = s.SettleWrenAIUsage(ctx, c.WorkspaceID, r.ID, 16, "known-request", json.RawMessage(`{"cost":0.00001569762,"prompt":"private user audio","input_audio":{"data":"secret"}}`)); e != nil {
		t.Fatal(e)
	}
	a, _ = s.WrenAIAllowance(ctx, c.WorkspaceID)
	if a.HeldMicroUSD != 0 || a.SpentMicroUSD != 16 || a.ClosedAt == nil {
		t.Fatalf("closereconcile %+v", a)
	}
	var receipt string
	s.pool.QueryRow(ctx, `SELECT usage::text FROM wren_ai_reservations WHERE id=$1`, r.ID).Scan(&receipt)
	if strings.Contains(receipt, "private") || strings.Contains(receipt, "secret") || strings.Contains(receipt, "prompt") {
		t.Fatal("financialreceiptkeptprivatecontent")
	}
}

func TestWrenAIACPBrokerUsesSameWorkspaceAllowance(t *testing.T) {
	h, c, _, _, _, token := harnessFixture(t)
	s := h.service.Store
	ctx := context.Background()
	s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 1000)
	h.config.AIUsage = WrenAIUsageConfig{Enforce: true, ModelTariffs: map[string]WrenAIModelTariff{"test-worker": {InputMicroUSDPerMillionTokens: 1000000, OutputMicroUSDPerMillionTokens: 1000000, MaxInputBytes: 4096, MaxOutputTokens: 128, Source: "fixturetariff", ObservedAt: "2026-10-26T04:27:23Z"}}}
	lead, e := s.ReserveWrenAIUsage(ctx, aiTestInput(c, "held-lead", "operator", 900))
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	h.config.Harnesses.transport = brokerTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"acp-generation","usage":{"cost":0.00002},"choices":[]}`))}, nil
	})
	body := `{"model":"test-worker","messages":[{"role":"user","content":"hello"}]}`
	if w := brokerRequest(h, token, body); w.Code != 429 || calls != 0 {
		t.Fatalf("ACPignoredleadreserve HTTP%d calls%d %s", w.Code, calls, w.Body.String())
	}
	if e = s.ReleaseWrenAIUsage(ctx, c.WorkspaceID, lead.ID); e != nil {
		t.Fatal(e)
	}
	if w := brokerRequest(h, token, body); w.Code != 200 || calls != 1 {
		t.Fatalf("ACPbudgetedcall HTTP%d %s", w.Code, w.Body.String())
	}
	a, _ := s.WrenAIAllowance(ctx, c.WorkspaceID)
	if a.SpentMicroUSD != 20 || a.HeldMicroUSD != 0 {
		t.Fatalf("ACPseparatebudget %+v", a)
	}
}

func TestWrenAIWelcomeWorkspaceScopeDoesNotInventConversation(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 100)
	var before, after int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_conversations WHERE workspace_id=$1`, c.WorkspaceID).Scan(&before)
	in := aiTestInput(c, "welcome-physical", "operator", 20)
	in.ConversationID = ""
	if _, e := s.ReserveWrenAIUsage(ctx, in); !errors.Is(e, ErrForbidden) {
		t.Fatal("genericpaidrequestescapedconversation")
	}
	in.Purpose = "welcome"
	r, e := s.ReserveWrenAIUsage(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.DispatchWrenAIUsage(ctx, c.WorkspaceID, r.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.SettleWrenAIUsage(ctx, c.WorkspaceID, r.ID, 5, "welcome-provider", nil); e != nil {
		t.Fatal(e)
	}
	s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_conversations WHERE workspace_id=$1`, c.WorkspaceID).Scan(&after)
	a, _ := s.WrenAIAllowance(ctx, c.WorkspaceID)
	if before != after || a.SpentMicroUSD != 5 {
		t.Fatalf("welcomefabricatedhistory orresetcap before%d after%d %+v", before, after, a)
	}
	in.Kind = "acp"
	in.RequestKey = "worker-escape"
	if _, e = s.ReserveWrenAIUsage(ctx, in); !errors.Is(e, ErrForbidden) {
		t.Fatal("ACPescapedoriginscope")
	}
}
