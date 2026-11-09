package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestWrenExtensionReviewsExactRequesterOnceAndLifecycle(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"changed":true}`))
	}))
	defer server.Close()
	h, private, shared, peer, install := extensionFixture(t, server.URL, "none")
	s := h.service.Store
	ctx := context.Background()
	input := extensionInstallEdit(install)
	input.AllowWrite = true
	var err error
	install, err = s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.EnqueueOperator(ctx, shared, "review-request", "Prepare the requested update", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	name := extensionDynamicName(install.ID, install.Version, "update")
	prepare := func(value, key string) string {
		t.Helper()
		out, e := h.InvokeWrenExtensionTool(ctx, shared, run, name, json.RawMessage(`{"query":"`+value+`"}`), key)
		if e != nil {
			t.Fatal(e)
		}
		call := out.(WrenExtensionCall)
		if call.Status != "review_pending" || call.ReviewID == "" {
			t.Fatalf("not a proposal: %+v", call)
		}
		return call.ReviewID
	}
	id := prepare("reviewed value", "first")
	if same := prepare("reviewed value", "different-model-id"); same != id {
		t.Fatal("model call id created duplicate review")
	}
	if hits.Load() != 0 {
		t.Fatal("write executed before review")
	}
	ownerReview, err := h.extensionReview(ctx, private.WorkspaceID, private.AccountID, id)
	if err != nil || !strings.Contains(string(ownerReview.Arguments), "reviewed value") || ownerReview.Endpoint != server.URL+"/write" || ownerReview.Account != "No sign-in required" {
		t.Fatalf("wrong exact review: %+v %v", ownerReview, err)
	}
	if _, err = h.extensionReview(ctx, private.WorkspaceID, peer, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("peer read arguments: %v", err)
	}
	if _, err = h.decideExtensionReview(ctx, private.WorkspaceID, peer, id, "approve"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("peer approved: %v", err)
	}
	_, _, peerToken, err := s.Signin(ctx, "knowledge-member@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/v1/workspaces/"+private.WorkspaceID+"/wren-extensions/reviews/"+id, nil)
	request.AddCookie(&http.Cookie{Name: "canter_session", Value: peerToken})
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"status":"private"`) || strings.Contains(response.Body.String(), "reviewed value") || strings.Contains(response.Body.String(), server.URL) {
		t.Fatalf("peer presentation leaked details or reported a broken card: %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest("POST", "/v1/workspaces/"+private.WorkspaceID+"/wren-extensions/reviews/"+id, strings.NewReader(`{"decision":"approve"}`))
	request.AddCookie(&http.Cookie{Name: "canter_session", Value: peerToken})
	request.Header.Set("Origin", "http://canter.test")
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 404 || hits.Load() != 0 {
		t.Fatalf("peer HTTP approval accepted: %d", response.Code)
	}
	var leaked bool
	if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_events WHERE run_id=$1 AND kind='extension_review' AND data::text LIKE '%reviewed value%')`, run.ID).Scan(&leaked); err != nil || leaked {
		t.Fatalf("private args in room event: %v", err)
	}
	// Approving concurrently must result in one outbound operation with one stable receipt.
	var wg sync.WaitGroup
	fail := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := h.decideExtensionReview(ctx, private.WorkspaceID, private.AccountID, id, "approve")
			fail <- e
		}()
	}
	wg.Wait()
	close(fail)
	for e := range fail {
		if e != nil {
			t.Fatal(e)
		}
	}
	result, err := h.extensionReview(ctx, private.WorkspaceID, private.AccountID, id)
	if err != nil || hits.Load() != 1 || result.Receipt == nil || result.Receipt.Status != "completed" {
		t.Fatalf("not exactly once: %d %+v %v", hits.Load(), result, err)
	}
	outcomes, err := h.listExtensionReviewOutcomes(ctx, shared, run)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(outcomes)
	if !strings.Contains(string(encoded), `"executionStatus":"completed"`) || strings.Contains(string(encoded), "reviewed value") || strings.Contains(string(encoded), "arguments") {
		t.Fatalf("outcome metadata incomplete or disclosed private arguments: %s", encoded)
	}
	forged := shared
	forged.AccountID = peer
	if _, err = h.listExtensionReviewOutcomes(ctx, forged, run); !errors.Is(err, ErrForbidden) {
		t.Fatalf("peer forged outcome reader: %v", err)
	}
	declined := prepare("declined value", "decline")
	if _, err = h.decideExtensionReview(ctx, private.WorkspaceID, private.AccountID, declined, "decline"); err != nil {
		t.Fatal(err)
	}
	if _, err = h.decideExtensionReview(ctx, private.WorkspaceID, private.AccountID, declined, "approve"); !errors.Is(err, ErrConflict) {
		t.Fatalf("declined approved: %v", err)
	}
	expired := prepare("expired value", "expire")
	s.pool.Exec(ctx, `UPDATE wren_extension_reviews SET expires_at=now()-interval '1 second' WHERE id=$1`, expired)
	if _, err = h.decideExtensionReview(ctx, private.WorkspaceID, private.AccountID, expired, "approve"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired approved: %v", err)
	}
	stopped := prepare("stopped value", "stop")
	if err = s.cancelOperatorRequest(ctx, shared.WorkspaceID, shared.AccountID, shared.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.decideExtensionReview(ctx, private.WorkspaceID, private.AccountID, stopped, "approve"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stopped request approved: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("unapproved writes escaped: %d", hits.Load())
	}
}

func TestWrenExtensionReviewChangedCredentialAndUncertainReceipt(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		w.Write([]byte(`{"error":"uncertain"}`))
	}))
	defer server.Close()
	h, private, shared, _, install := extensionFixture(t, server.URL, "bearer")
	s := h.service.Store
	ctx := context.Background()
	input := extensionInstallEdit(install)
	input.AllowWrite = true
	install, err := s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.ConfigureWrenExtensionCredential(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionCredentialInput{Value: "test_original_bearer"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.EnqueueOperator(ctx, shared, "credential-review", "Prepare update", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	prepare := func(q string) string {
		t.Helper()
		out, e := h.InvokeWrenExtensionTool(ctx, shared, run, extensionDynamicName(install.ID, install.Version, "update"), json.RawMessage(`{"query":"`+q+`"}`), q)
		if e != nil {
			t.Fatal(e)
		}
		return out.(WrenExtensionCall).ReviewID
	}
	id := prepare("changed_account")
	view, err := s.WrenExtensionInstallation(ctx, private.WorkspaceID, private.AccountID, install.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.ConfigureWrenExtensionCredential(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionCredentialInput{ExpectedRevision: view.CredentialRevision, Value: "test_replacement_bearer"}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.decideExtensionReview(ctx, private.WorkspaceID, private.AccountID, id, "approve"); !errors.Is(err, ErrConflict) {
		t.Fatalf("account change accepted: %v", err)
	}
	unknown := prepare("uncertain_write")
	for i := 0; i < 2; i++ {
		out, e := h.decideExtensionReview(ctx, private.WorkspaceID, private.AccountID, unknown, "approve")
		if e != nil || out.Receipt == nil || out.Receipt.Status != "unknown" {
			t.Fatalf("uncertain result: %+v %v", out, e)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("uncertain write resent: %d", hits.Load())
	}
	changed := prepare("changed_installation")
	input = extensionInstallEdit(install)
	input.Enabled = false
	if _, err = s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, input); err != nil {
		t.Fatal(err)
	}
	if _, err = h.decideExtensionReview(ctx, private.WorkspaceID, private.AccountID, changed, "approve"); err == nil {
		t.Fatal("disabled installation approved")
	}
	if hits.Load() != 1 {
		t.Fatal("changed installation called remote")
	}
}
