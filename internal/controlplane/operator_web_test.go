package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

type webTestTransport func(*http.Request) (*http.Response, error)

func (f webTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func webTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestOperatorWebURLAndUnicode(t *testing.T) {
	for _, raw := range []string{"file:///tmp/a", "https://user:password@example.com", "http://localhost", "http://127.0.0.1", "http://[::1]", "http://169.254.169.254/latest", "http://2130706433", "http://0x7f000001", "http://service.internal", "http://site.local.", "https://example.com:8080", "https://exa.ai\\@localhost"} {
		if _, err := publicWebURL(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	got, err := publicWebURL("https://EXA.AI:443/docs#section")
	if err != nil || got != "https://exa.ai:443/docs" {
		t.Fatalf("canonical URL %q %v", got, err)
	}
	offsets := webMatchOffsets("xKelvin你好 kelvin", "kelvin", 8)
	if len(offsets) != 2 || offsets[0] != 1 || offsets[1] != 16 {
		t.Fatalf("UTF-8 offsets: %v", offsets)
	}
}
func TestOperatorExaProtocolAndFailures(t *testing.T) {
	count := 0
	cfg := OperatorConfig{ExaAPIKey: "test-secret", exaTransport: webTestTransport(func(req *http.Request) (*http.Response, error) {
		count++
		if req.URL.String() != "https://api.exa.ai/search" || req.Method != "POST" || req.Header.Get("x-api-key") != "test-secret" {
			t.Fatalf("wrong provider request")
		}
		return webTestResponse(429, `{"error":"test-secret echoed by upstream"}`), nil
	})}
	_, err := cfg.exaRequest(context.Background(), "search", map[string]string{"query": "public docs"})
	if err == nil || strings.Contains(err.Error(), "test-secret") || count != 1 {
		t.Fatalf("unsafe error or retry: %v %d", err, count)
	}
	cfg.exaTransport = webTestTransport(func(req *http.Request) (*http.Response, error) {
		r := webTestResponse(307, "")
		r.Header.Set("Location", "https://other.example/steal")
		return r, nil
	})
	if _, err = cfg.exaRequest(context.Background(), "search", nil); err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect accepted: %v", err)
	}
	cfg.exaTransport = webTestTransport(func(req *http.Request) (*http.Response, error) {
		return webTestResponse(200, strings.Repeat("x", (2<<20)+1)), nil
	})
	if _, err = cfg.exaRequest(context.Background(), "search", nil); err == nil || !strings.Contains(err.Error(), "2 MiB") {
		t.Fatalf("oversize accepted: %v", err)
	}
	cfg.exaTransport = webTestTransport(func(req *http.Request) (*http.Response, error) { return webTestResponse(200, `{"results":[]}`), nil })
	result, err := cfg.exaRequest(context.Background(), "search", nil)
	if err != nil || result.CostDollars != nil {
		t.Fatal("unknown cost must not become zero")
	}
	for _, tool := range (&OperatorRuntime{Config: cfg}).tools() {
		if _, ok := operatorPolicy(tool.Name); !ok {
			t.Fatalf("missing policy: %s", tool.Name)
		}
	}
	for _, tool := range (&OperatorRuntime{}).webTools() {
		if tool.Name != "canter_read_web" {
			t.Fatal("network search advertised without credential")
		}
	}
}

func TestOperatorWebEvidenceRecoveryIsolationAndBudget(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	_, err := s.EnqueueOperator(ctx, c, "web_request", "Research public documentation", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	principal, err := s.operatorPrincipal(ctx, c, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	freshRequests := 0
	document := "Authoritative paragraph. " + strings.Repeat("你好 evidence passage. ", 9000)
	cfg := OperatorConfig{ExaAPIKey: "test-secret", exaTransport: webTestTransport(func(req *http.Request) (*http.Response, error) {
		requests++
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		text := ""
		highlights := []string{"A compact preview"}
		if req.URL.Path == "/contents" {
			text = document
			if body["maxAgeHours"] == float64(0) {
				freshRequests++
			}
		}
		if req.URL.Path == "/search" {
			if body["numResults"] != float64(5) || body["type"] != "auto" {
				t.Fatalf("unbounded search payload: %v", body)
			}
		}
		raw, _ := json.Marshal(map[string]any{"requestId": "exa-test", "results": []any{map[string]any{"url": "https://example.com/paper", "title": "Paper", "publishedDate": "2026-06-01", "text": text, "highlights": highlights}}, "costDollars": map[string]any{"total": 0.007}})
		return webTestResponse(200, string(raw)), nil
	})}
	runtime := &OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{}).(*HTTPServer), Config: cfg}
	call := func(id, name, args string) []byte {
		t.Helper()
		toolCall := operatorTestCall(name, args)
		toolCall.ID = id
		raw, e := runtime.executeTool(ctx, run, c, principal, toolCall)
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	// Use the ordinary dispatcher, so reservations, replay and cost evidence are tested.
	first := call("s1", "canter_search_web", `{"query":"public docs","domains":["example.com"],"publishedAfter":"2026-01-01"}`)
	var search operatorWebResult
	if err = json.Unmarshal(first, &search); err != nil || len(search.Sources) != 1 || search.Sources[0].Kind != "preview" {
		t.Fatalf("search %s %v", first, err)
	}
	call("s1", "canter_search_web", `{"query":"public docs"}`)
	if requests != 1 {
		t.Fatal("replay billed twice")
	}
	cached := call("s2", "canter_search_web", `{"query":"public docs","domains":["example.com"],"publishedAfter":"2026-01-01"}`)
	var cache operatorWebResult
	_ = json.Unmarshal(cached, &cache)
	if requests != 1 || !cache.Cached || cache.CostDollars == nil || *cache.CostDollars != 0 || cache.Sources[0].ID != search.Sources[0].ID {
		t.Fatalf("cache failed: %s", cached)
	}
	opened := call("o1", "canter_open_web", `{"url":"https://example.com/paper"}`)
	var page operatorWebResult
	_ = json.Unmarshal(opened, &page)
	if len(opened) > 3000 || len(page.Sources) != 1 || !page.Sources[0].Truncated || page.Sources[0].Bytes > webDocumentBytes || requests != 2 {
		t.Fatalf("unbounded open: %d bytes, %+v", len(opened), page)
	}
	id := page.Sources[0].ID
	read := call("read", "canter_read_web", fmt.Sprintf(`{"action":"read","sourceId":%q,"offset":0}`, id))
	var segment struct {
		Content  string `json:"content"`
		Next     int    `json:"nextOffset"`
		Complete bool   `json:"complete"`
	}
	_ = json.Unmarshal(read, &segment)
	if len(segment.Content) > 6000 || !utf8.ValidString(segment.Content) || segment.Complete || segment.Next != len(segment.Content) {
		t.Fatalf("bad page: %s", read)
	}
	// Recover every byte after restarting the runtime; no provider request needed.
	runtime = &OperatorRuntime{Server: runtime.Server, Config: cfg}
	var restored strings.Builder
	restored.WriteString(segment.Content)
	for !segment.Complete {
		value, e := s.readOperatorWeb(ctx, c, json.RawMessage(fmt.Sprintf(`{"action":"read","sourceId":%q,"offset":%d}`, id, segment.Next)))
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(value)
		_ = json.Unmarshal(raw, &segment)
		restored.WriteString(segment.Content)
	}
	stored, err := s.operatorWebSource(ctx, c.ID, id)
	if err != nil || restored.String() != stored.Content {
		t.Fatal("snapshot paging lost content")
	}
	found := call("find", "canter_read_web", `{"action":"find","query":"evidence passage"}`)
	if !strings.Contains(string(found), id) || requests != 2 {
		t.Fatalf("find missed evidence or billed: %s", found)
	}
	var passages struct {
		Matches []struct {
			Offset  int    `json:"offset"`
			Content string `json:"content"`
		} `json:"matches"`
	}
	if err = json.Unmarshal(found, &passages); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(passages.Matches); i++ {
		previous := passages.Matches[i-1]
		if passages.Matches[i].Offset < previous.Offset+len(previous.Content) {
			t.Fatal("overlapping find passages waste model context")
		}
	}
	other, err := s.CreateConversation(ctx, c.WorkspaceID, c.AccountID, "conv_web_other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.operatorWebSource(ctx, other.ID, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-conversation disclosure: %v", err)
	}
	foreign, err := s.readOperatorWeb(ctx, other, json.RawMessage(`{"action":"find","query":"evidence passage"}`))
	if err != nil {
		t.Fatal(err)
	}
	foreignJSON, _ := json.Marshal(foreign)
	if strings.Contains(string(foreignJSON), id) {
		t.Fatal("find leaked another conversation")
	}
	call("o2", "canter_open_web", `{"url":"https://example.com/paper"}`)
	if requests != 2 {
		t.Fatal("page cache missed")
	}
	call("o3", "canter_open_web", `{"url":"https://example.com/paper","refresh":true}`)
	if requests != 3 || freshRequests != 1 {
		t.Fatal("freshness bypass failed")
	}
	old, err := s.operatorWebSource(ctx, c.ID, id)
	if err != nil || old.RetrievedAt != stored.RetrievedAt {
		t.Fatal("refresh mutated earlier evidence")
	}
	for n := 3; n <= 8; n++ {
		call(fmt.Sprintf("s%d", n), "canter_search_web", fmt.Sprintf(`{"query":"public query %d"}`, n))
	}
	before := requests
	blocked := call("s9", "canter_search_web", `{"query":"one more"}`)
	if requests != before || !strings.Contains(string(blocked), "limit reached") {
		t.Fatalf("budget bypass: %s", blocked)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO operator_tool_calls(run_id,call_id,name,arguments) VALUES($1,'ambiguous','canter_open_web','{}')`, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := call("ambiguous", "canter_open_web", `{"url":"https://example.com/paper"}`)
	if requests != before || !strings.Contains(string(ambiguous), "was not repeated") {
		t.Fatal("ambiguous paid operation retried")
	}
	if err = s.cancelOperator(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.webTool(ctx, run, c, "canter_open_web", json.RawMessage(`{"url":"https://example.com/new"}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled evidence persisted: %v", err)
	}
	var leaked int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_web_sources WHERE conversation_id=$1 AND url='https://example.com/new'`, c.ID).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatal("cancelled source persisted")
	}
	if requests != before {
		t.Fatal("cancelled run made a paid request")
	}
	// Identical queries in another conversation must not reuse private handles.
	if _, err = s.EnqueueOperator(ctx, other, "other_request", "Search public docs", "test", nil); err != nil {
		t.Fatal(err)
	}
	otherRun, claimed, err := s.claimOperator(ctx)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	otherPrincipal, err := s.operatorPrincipal(ctx, other, otherRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherResult, err := runtime.executeTool(ctx, otherRun, other, otherPrincipal, operatorTestCall("canter_search_web", `{"query":"public docs","domains":["example.com"],"publishedAfter":"2026-01-01"}`))
	if err != nil || requests != before+1 || strings.Contains(string(otherResult), search.Sources[0].ID) {
		t.Fatal("cache crossed conversation scope")
	}
	if err = s.DeleteConversation(ctx, c.WorkspaceID, c.AccountID, c.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM operator_web_sources WHERE conversation_id=$1)+(SELECT count(*) FROM operator_web_cache WHERE conversation_id=$1)`, c.ID).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatal("conversation deletion left web evidence")
	}
}
