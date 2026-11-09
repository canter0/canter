package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type mediaTransport func(*http.Request) (*http.Response, error)

func (f mediaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func mediaPNG(t *testing.T) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	im.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func mediaServer(t *testing.T, s *Store, fn mediaTransport) *HTTPServer {
	t.Helper()
	return NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Operator: OperatorConfig{APIKey: "owned-test-key"}, Media: WrenMediaConfig{transport: fn}}).(*HTTPServer)
}
func mediaResponse(data []byte) *http.Response {
	b, _ := json.Marshal(map[string]any{"data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(data), "media_type": "image/png"}}, "usage": map[string]any{"cost": 0.001}})
	return &http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": []string{"owned-provider-receipt"}}, Body: io.NopCloser(bytes.NewReader(b))}
}
func TestWrenMediaActualBytesReplayPrivacyAndBudget(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	data := mediaPNG(t)
	var calls atomic.Int32
	h := mediaServer(t, s, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.String() != "https://openrouter.ai/api/v1/images" || r.Header.Get("Authorization") != "Bearer owned-test-key" {
			t.Fatal("wrong provider boundary")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["n"] != float64(1) || payload["quality"] != "low" || payload["model"] != defaultWrenImageModel {
			t.Fatal(payload)
		}
		return mediaResponse(data), nil
	})
	input := WrenMediaInput{OperationKey: "owned-media-request-1", Prompt: "Create a tiny test icon", Name: "test.png"}
	op, err := h.GenerateWrenMedia(ctx, c, OperatorRun{}, input)
	if err != nil || op.State != "completed" || op.ArtifactID == "" {
		t.Fatalf("generation: %+v %v", op, err)
	}
	a, err := s.GetWrenArtifact(ctx, c.WorkspaceID, c.AccountID, op.ArtifactID, 0)
	if err != nil || !bytes.Equal(a.Content, data) || a.Provenance.Summary != "Image generation operation "+op.ID {
		t.Fatalf("actual artifact: %+v %v", a, err)
	}
	again, err := h.GenerateWrenMedia(ctx, c, OperatorRun{}, input)
	if err != nil || again.ID != op.ID || calls.Load() != 1 {
		t.Fatalf("replay resent: %+v %v %d", again, err, calls.Load())
	}
	input.Prompt = "different"
	if _, err = h.GenerateWrenMedia(ctx, c, OperatorRun{}, input); !errors.Is(err, ErrConflict) {
		t.Fatal("changed replay", err)
	}
	other, _, _, err := s.Signup(ctx, "media-other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.WrenMediaOperation(ctx, c.WorkspaceID, other.ID, op.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("private receipt leaked", err)
	}
	for i := 0; i < 3; i++ {
		input.OperationKey += "x"
		if _, err = h.GenerateWrenMedia(ctx, c, OperatorRun{}, input); err != nil {
			t.Fatal(err)
		}
	}
	input.OperationKey += "x"
	if _, err = h.GenerateWrenMedia(ctx, c, OperatorRun{}, input); !errors.Is(err, ErrCapacity) {
		t.Fatal("budget not enforced", err)
	}
}
func TestWrenMediaEditConflictRecoveryNoRepayment(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	data := mediaPNG(t)
	original, err := s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, WrenArtifactInput{ConversationID: c.ID, Name: "base.png", MediaType: "image/png", Content: data})
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	h := mediaServer(t, s, func(r *http.Request) (*http.Response, error) {
		calls++
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["input_references"] == nil {
			t.Fatal("edit omitted actual source")
		}
		_, err = s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, WrenArtifactInput{ID: original.ID, ConversationID: c.ID, ExpectedRevision: 1, Name: original.Name, MediaType: original.MediaType, Content: data})
		if err != nil {
			t.Fatal(err)
		}
		return mediaResponse(data), nil
	})
	op, err := h.GenerateWrenMedia(ctx, c, OperatorRun{}, WrenMediaInput{OperationKey: "owned-edit-key-1", Mode: "edit", Prompt: "Make this red", Name: "edited.png", SourceArtifactID: original.ID, ExpectedRevision: 1})
	if err != nil || op.State != "conflict" || !op.Recoverable || op.ArtifactID != "" {
		t.Fatalf("conflict bytes not retained: %+v %v", op, err)
	}
	op, err = h.publishWrenMedia(ctx, c.WorkspaceID, c.AccountID, op.ID, true)
	if err != nil || op.State != "completed" || op.ArtifactID == original.ID {
		t.Fatalf("recover: %+v %v", op, err)
	}
	again, err := h.publishWrenMedia(ctx, c.WorkspaceID, c.AccountID, op.ID, true)
	if err != nil || again.ArtifactID != op.ArtifactID || calls != 1 {
		t.Fatal("recovery retried provider", err, calls)
	}
	a, err := s.GetWrenArtifact(ctx, c.WorkspaceID, c.AccountID, original.ID, 0)
	if err != nil || a.Revision != 2 {
		t.Fatal("overwrote human edit", err)
	}
}
func TestWrenMediaUnknownAndInvalidAreNotSuccess(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	var calls int
	h := mediaServer(t, s, func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("owned connection loss") })
	input := WrenMediaInput{OperationKey: "owned-uncertain-key", Prompt: "Create a picture", Name: "image.png"}
	op, err := h.GenerateWrenMedia(ctx, c, OperatorRun{}, input)
	if err != nil || op.State != "unknown" || op.ArtifactID != "" {
		t.Fatalf("unknown misreported %+v %v", op, err)
	}
	_, err = h.GenerateWrenMedia(ctx, c, OperatorRun{}, input)
	if err != nil || calls != 1 {
		t.Fatal("uncertain retry", err, calls)
	}
	h.config.Media.transport = mediaTransport(func(*http.Request) (*http.Response, error) { return mediaResponse([]byte("not an image")), nil })
	input.OperationKey += "2"
	op, err = h.GenerateWrenMedia(ctx, c, OperatorRun{}, input)
	if err != nil || op.State != "failed" || op.ArtifactID != "" {
		t.Fatalf("invalid marked success %+v %v", op, err)
	}
	h.config.Operator.APIKey = ""
	input.OperationKey += "3"
	if _, err = h.GenerateWrenMedia(ctx, c, OperatorRun{}, input); err == nil {
		t.Fatal("unconfigured provider succeeded")
	}
}
func TestWrenMediaHTTPAndRoleRevocation(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	calls := 0
	h := mediaServer(t, s, func(*http.Request) (*http.Response, error) {
		calls++
		_, err := s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, c.WorkspaceID, c.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		return mediaResponse(mediaPNG(t)), nil
	})
	payload := map[string]any{"conversationId": c.ID, "operationKey": "owned-http-image-key", "prompt": "Create an icon", "name": "icon.png"}
	b, _ := json.Marshal(payload)
	r := httptest.NewRequest("POST", "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/wren-media", bytes.NewReader(b))
	r.Header.Set("Origin", "http://canter.test")
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || strings.Contains(w.Body.String(), "owned-test-key") {
		t.Fatalf("revoked publication HTTP %d %s", w.Code, w.Body)
	}
	ops, err := s.WrenMediaOperations(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if err != nil || len(ops) != 1 || !ops[0].Recoverable || ops[0].ArtifactID != "" {
		t.Fatalf("revoked bytes lost %+v %v", ops, err)
	}
	if _, err = h.publishWrenMedia(ctx, c.WorkspaceID, c.AccountID, ops[0].ID, true); !errors.Is(err, ErrForbidden) {
		t.Fatal("viewer recovered", err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestWrenMediaAcceptedIntentAndEnforcedAllowance(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	calls := 0
	h := mediaServer(t, s, func(*http.Request) (*http.Response, error) { calls++; return mediaResponse(mediaPNG(t)), nil })
	_, err := s.EnqueueOperator(ctx, c, "image-intent", "Create a simple bird logo image", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	input := WrenMediaInput{OperationKey: "owned-authority-key", Prompt: "A bird silhouette", Name: "bird.png"}
	forged := run
	forged.Lease = "forged"
	if _, err = h.GenerateWrenMedia(ctx, c, forged, input); !errors.Is(err, ErrForbidden) {
		t.Fatal("forged lease accepted", err)
	}
	h.config.AIUsage = WrenAIUsageConfig{Enforce: true}
	op, err := h.GenerateWrenMedia(ctx, c, run, input)
	if err != nil || op.State != "failed" || calls != 0 {
		t.Fatalf("missing cost policy sent request %+v %v %d", op, err, calls)
	}
	h.config.AIUsage.KindEstimatesMicroUSD = map[string]int64{"media": 10000}
	if err = s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 20000); err != nil {
		t.Fatal(err)
	}
	input.OperationKey += "2"
	input.Prompt += " dark blue"
	op, err = h.GenerateWrenMedia(ctx, c, run, input)
	if err != nil || op.State != "completed" || calls != 1 {
		t.Fatalf("allowed image %+v %v %d", op, err, calls)
	}
	budget, err := s.WrenAIAllowance(ctx, c.WorkspaceID)
	if err != nil || budget.SpentMicroUSD != 1000 || budget.HeldMicroUSD != 0 {
		t.Fatalf("actual media cost not settled %+v %v", budget, err)
	}
	// A different tool-call ID in the same accepted root task does not repay for
	// exactly the same prompt/source even when the filename is changed.
	input.OperationKey += "3"
	input.Name = "renamed.png"
	repeated, err := h.GenerateWrenMedia(ctx, c, run, input)
	if err != nil || repeated.ID != op.ID || calls != 1 {
		t.Fatalf("duplicate root request resent %+v %v", repeated, err)
	}
}

func TestWrenMediaRuntimeToolPublishesRealSurface(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	calls := 0
	h := mediaServer(t, s, func(*http.Request) (*http.Response, error) { calls++; return mediaResponse(mediaPNG(t)), nil })
	if _, err := s.EnqueueOperator(ctx, c, "owned-runtime-image", "Please create a bird logo image", "test", nil); err != nil {
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
	runtime := OperatorRuntime{Server: h}
	call := operatorTestCall("canter_generate_image", `{"prompt":"A blue wren silhouette","name":"bird.png"}`)
	result, err := runtime.executeTool(ctx, run, c, principal, call)
	if err != nil || !strings.Contains(string(result), "completed") {
		t.Fatalf("runtime media %s %v", result, err)
	}
	again, err := runtime.executeTool(ctx, run, c, principal, call)
	if err != nil || len(again) == 0 || calls != 1 {
		t.Fatal("runtime replay paid again", err, calls)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_events WHERE run_id=$1 AND kind='surface' AND data->>'kind'='wren-artifact'`, run.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("real artifact surface missing", count, err)
	}
}

func TestWrenMediaConcurrentReplayMakesOnePhysicalRequest(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	calls := atomic.Int32{}
	data := mediaPNG(t)
	h := mediaServer(t, s, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		close(entered)
		<-release
		return mediaResponse(data), nil
	})
	input := WrenMediaInput{OperationKey: "owned-concurrent-key", Prompt: "Create an icon", Name: "icon.png"}
	done := make(chan error, 1)
	go func() {
		op, err := h.GenerateWrenMedia(ctx, c, OperatorRun{}, input)
		if err == nil && op.State != "completed" {
			err = errors.New("first request did not complete")
		}
		done <- err
	}()
	<-entered
	op, err := h.GenerateWrenMedia(ctx, c, OperatorRun{}, input)
	close(release)
	if err != nil || op.State != "running" || op.ArtifactID != "" {
		t.Fatalf("inflight replay claimed success %+v %v", op, err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent replay paid twice", calls.Load())
	}
}

func TestWrenMediaRecoveryReconcilesCommittedEditBeforeFork(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	data := mediaPNG(t)
	source, err := s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, WrenArtifactInput{ConversationID: c.ID, Name: "source.png", MediaType: "image/png", Content: data})
	if err != nil {
		t.Fatal(err)
	}
	// Fail the final receipt write. The new transaction must roll back the
	// artifact edit with it; then emulate an older split-commit receipt for
	// backwards-compatible reconciliation. No real provider is contacted.
	_, err = s.pool.Exec(ctx, `CREATE FUNCTION wren_media_test_receipt_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='completed' THEN RAISE EXCEPTION 'owned completed receipt failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER wren_media_test_receipt_failure BEFORE UPDATE ON wren_media_operations FOR EACH ROW EXECUTE FUNCTION wren_media_test_receipt_failure()`)
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS wren_media_test_receipt_failure ON wren_media_operations; DROP FUNCTION IF EXISTS wren_media_test_receipt_failure()`)
	calls := 0
	h := mediaServer(t, s, func(*http.Request) (*http.Response, error) { calls++; return mediaResponse(data), nil })
	_, err = h.GenerateWrenMedia(ctx, c, OperatorRun{}, WrenMediaInput{OperationKey: "crash-after-edit-commit", Mode: "edit", Name: "edited.png", Prompt: "Make the bird blue", SourceArtifactID: source.ID, ExpectedRevision: 1})
	if err == nil {
		t.Fatal("receipt failure seam did not execute")
	}
	committed, err := s.GetWrenArtifact(ctx, c.WorkspaceID, c.AccountID, source.ID, 0)
	if err != nil || committed.Revision != 1 {
		t.Fatal("receipt failure left a half-published edit", err)
	}
	ops, err := s.WrenMediaOperations(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if err != nil || len(ops) != 1 || ops[0].State != "ready" {
		t.Fatal("retained operation missing", err)
	}
	if _, err = s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, WrenArtifactInput{ID: source.ID, ConversationID: c.ID, ExpectedRevision: 1, Name: "edited.png", MediaType: "image/png", Content: data, Provenance: WrenArtifactProvenance{Summary: "Image generation operation " + ops[0].ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `DROP TRIGGER wren_media_test_receipt_failure ON wren_media_operations; DROP FUNCTION wren_media_test_receipt_failure()`); err != nil {
		t.Fatal(err)
	}
	recovered, err := h.publishWrenMedia(ctx, c.WorkspaceID, c.AccountID, ops[0].ID, true)
	if err != nil || recovered.State != "completed" || recovered.ArtifactID != source.ID || recovered.ArtifactRevision != 2 {
		t.Fatalf("recovery forked committed edit: %+v %v", recovered, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_artifacts WHERE conversation_id=$1`, c.ID).Scan(&count); err != nil || count != 1 || calls != 1 {
		t.Fatal("duplicate artifact/payment", count, calls, err)
	}
}

func TestWrenMediaHumanArchiveAfterClaimStopsBeforeNetwork(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	// The AFTER INSERT transition deterministically places archive after the
	// claim's lifecycle check and before dispatch. Equivalent interleaving: an
	// archive transaction commits between request claim and paid network send.
	_, err := s.pool.Exec(ctx, `CREATE FUNCTION wren_media_test_archive_claim() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO wren_lifecycle(conversation_id,archived_at,archived_by) VALUES(NEW.conversation_id,now(),NEW.account_id) ON CONFLICT(conversation_id) DO UPDATE SET archived_at=now(); RETURN NEW; END $$; CREATE TRIGGER wren_media_test_archive_claim AFTER INSERT ON wren_media_operations FOR EACH ROW EXECUTE FUNCTION wren_media_test_archive_claim()`)
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS wren_media_test_archive_claim ON wren_media_operations; DROP FUNCTION IF EXISTS wren_media_test_archive_claim()`)
	calls := 0
	h := mediaServer(t, s, func(*http.Request) (*http.Response, error) { calls++; return mediaResponse(mediaPNG(t)), nil })
	op, err := h.GenerateWrenMedia(ctx, c, OperatorRun{}, WrenMediaInput{OperationKey: "archive-at-dispatch-boundary", Name: "image.png", Prompt: "Create an image"})
	if err != nil || op.State != "failed" || calls != 0 || !strings.Contains(op.Failure, "archived") {
		t.Fatalf("archived human request dispatched %+v %v %d", op, err, calls)
	}
}

func TestWrenMediaIntentRecognizesOnlyDirectVisualRequests(t *testing.T) {
	cases := []struct {
		text    string
		allowed bool
	}{
		{"Please create an image with no text", true},
		{"Could you generate a polished orange bird logo?", true},
		{"Edit the source image to make the bird orange", true},
		{"Change the existing logo to orange", true},
		{"Create an image; no text or shadow", true},
		{"Don't make an image; edit the README to describe the existing logo.", false},
		{"Never generate any icons. Write docs describing our current logo.", false},
		{"Do not create an image", false},
		{"Don't draw a picture", false},
		{"Update the README describing the existing logo image", false},
		{"Edit the code to change the logo", false},
		{"Create documentation about image generation", false},
		{"I edited the existing image yesterday", false},
		{"How do I create an image?", false},
		{"Explain how to generate a logo", false},
		{"If I create an image, what happens?", false},
		{"Create an image caption", false},
		{"Create an image prompt", false},
		{"Generate an image tag for this existing logo", false},
		{"Change the logo CSS", false},
		{"Make the README image description clearer", false},
		{"Create an article with an existing logo image", false},
	}
	for _, test := range cases {
		if actual := wrenMediaInstructionAllows(test.text); actual != test.allowed {
			t.Errorf("%q: allowed=%v want=%v", test.text, actual, test.allowed)
		}
	}
}
func TestWrenMediaAmbiguousAcceptedInstructionNeverCallsProvider(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	calls := 0
	h := mediaServer(t, s, func(*http.Request) (*http.Response, error) { calls++; return mediaResponse(mediaPNG(t)), nil })
	if _, err := s.EnqueueOperator(ctx, c, "not-an-image-request", "Don't make an image; edit the README to describe the existing logo.", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	_, err = h.GenerateWrenMedia(ctx, c, run, WrenMediaInput{OperationKey: "ambiguous-media-authority", Name: "image.png", Prompt: "Create an orange bird"})
	if err == nil || !strings.Contains(err.Error(), "needs-image-request") || calls != 0 {
		t.Fatal("ambiguous source spent money", err, calls)
	}
}

func TestWrenMediaConcurrentInitialPublishAndRecoveryCommitOnce(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	source, err := s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, WrenArtifactInput{ConversationID: c.ID, Name: "source.png", MediaType: "image/png", Content: mediaPNG(t)})
	if err != nil {
		t.Fatal(err)
	}
	id := "media_concurrent_publish_recovery"
	_, err = s.pool.Exec(ctx, `INSERT INTO wren_media_operations(id,workspace_id,conversation_id,account_id,operation_key,request_sha256,model,mode,name,prompt,source_artifact_id,source_revision,state,width,height,sha256,output_bytes) VALUES($1,$2,$3,$4,'concurrent-publish-fixture','fixture',$5,'edit','edited.png','Create an edited image',$6,1,'ready',4,4,$7,$8)`, id, c.WorkspaceID, c.ID, c.AccountID, defaultWrenImageModel, source.ID, source.SHA256, source.Content)
	if err != nil {
		t.Fatal(err)
	}
	h := mediaServer(t, s, func(*http.Request) (*http.Response, error) {
		t.Error("publication contacted provider")
		return nil, errors.New("unexpected provider request")
	})
	barrier, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(context.Background())
	var locked string
	if err = barrier.QueryRow(ctx, `SELECT id FROM operator_conversations WHERE id=$1 FOR UPDATE`, c.ID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		op  WrenMediaOperation
		err error
	}
	first := make(chan outcome, 1)
	second := make(chan outcome, 1)
	go func() {
		op, e := h.publishWrenMedia(ctx, c.WorkspaceID, c.AccountID, id, false)
		first <- outcome{op, e}
	}()
	waitLocks := func(want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			if e := s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FOR UPDATE OF c FOR KEY SHARE OF m%'`).Scan(&n); e != nil {
				t.Fatal(e)
			}
			if n >= want {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatal("publication did not wait behind origin authority lock", want)
	}
	waitLocks(1)
	go func() {
		op, e := h.publishWrenMedia(ctx, c.WorkspaceID, c.AccountID, id, true)
		second <- outcome{op, e}
	}()
	waitLocks(2)
	if err = barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	a, b := <-first, <-second
	if a.err != nil || b.err != nil || a.op.State != "completed" || b.op.State != "completed" || a.op.ArtifactID != b.op.ArtifactID || a.op.ArtifactRevision != b.op.ArtifactRevision {
		t.Fatalf("publication diverged first=%+v/%v second=%+v/%v", a.op, a.err, b.op, b.err)
	}
	var revisions, artifacts int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_artifact_revisions v JOIN wren_artifacts a ON a.id=v.artifact_id WHERE a.conversation_id=$1`, c.ID).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_artifacts WHERE conversation_id=$1`, c.ID).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if revisions != 2 || artifacts != 1 || a.op.ArtifactID != source.ID || a.op.ArtifactRevision != 2 {
		t.Fatalf("duplicate publication revisions=%d artifacts=%d", revisions, artifacts)
	}
}
