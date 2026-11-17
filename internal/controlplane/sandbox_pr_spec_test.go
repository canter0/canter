package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
)

func TestSandboxTariffCutoverKeepsResourceAndRemainder(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	_, workspace, _ := workspaceSession(t, s, "tariff-cutover@example.com")
	start := time.Now().UTC().Truncate(time.Second)
	_, err := s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id,status,period_start,period_end)
		VALUES($1,'customer','subscription','active',$2,$3)`, workspace.ID, start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	resources := []sdk.BillingResource{
		{ID: "compute/sandbox-shape", Kind: "compute", Units: 4},
		{ID: "compute/computer-shape", Kind: "compute", Units: 4},
	}
	if err = s.recordResourceSnapshot(ctx, workspace.ID, resources, start); err != nil {
		t.Fatal(err)
	}
	resources[0].Kind = "sandbox_compute"
	cutover := start.Add(3 * time.Minute)
	for _, at := range []time.Time{cutover, cutover, cutover.Add(3 * time.Minute), cutover} {
		if err = s.recordResourceSnapshot(ctx, workspace.ID, resources, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, expected := range []struct {
		id, tariff string
		remainder  int64
	}{
		{resources[0].ID, sandboxComputeRateVersion, 396000},
		{resources[1].ID, resourceRateVersion, 432000},
	} {
		var remainder int64
		var tariff string
		if err := s.pool.QueryRow(ctx, `SELECT remainder::bigint,rate_version FROM billing_resource_samples WHERE workspace_id=$1 AND resource_id=$2`,
			workspace.ID, expected.id).Scan(&remainder, &tariff); err != nil || remainder != expected.remainder || tariff != expected.tariff {
			t.Fatalf("%s: remainder=%d tariff=%s error=%v", expected.id, remainder, tariff, err)
		}
	}
}

func TestSandboxRetainedJobHistoryEncryptionSleepAndExpiry(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	f.h.config.Sandboxes.CollectJobHistory = true
	v := f.ready(t, f.create(t, "history", "free-1x1", http.StatusAccepted))
	ctx := context.Background()
	actor := sdk.ActorRef{Kind: "human", ID: f.account.ID}
	id := "job_retention_scope"
	if err := f.s.recordSandboxCommand(ctx, v.ID, id, v.Generation, actor, true); err != nil {
		t.Fatal(err)
	}
	claim, err := f.s.claimSandboxHistory(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	value := []byte("retained diagnostic text")
	exit := 17
	chunk := wrencomputer.JobLogChunk{JobID: id, Stream: "combined", Data: value, NextOffset: int64(len(value)), EOF: true, Status: "completed", ExitCode: &exit}
	if err = f.h.publishSandboxHistory(ctx, v, claim, chunk); err != nil {
		t.Fatal(err)
	}
	if err = f.h.publishSandboxHistory(ctx, v, claim, chunk); err == nil {
		t.Fatal("duplicate lease publication succeeded")
	}
	var encrypted []byte
	if err = f.s.pool.QueryRow(ctx, `SELECT ciphertext FROM sandbox_job_history_chunks WHERE sandbox_id=$1 AND job_id=$2`, v.ID, id).Scan(&encrypted); err != nil || strings.Contains(string(encrypted), string(value)) {
		t.Fatalf("ciphertext missing or contained plaintext: %v", err)
	}
	if _, err = f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep',generation=generation+1,address=NULL WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) {
		t.Fatal("history attempted to dial sleeping sandbox")
		return nil, errors.New("unexpected dial")
	}
	base := "/v1/workspaces/" + v.WorkspaceID + "/sandboxes/" + v.ID + "/history"
	list := requestJSON(t, f.h, http.MethodGet, base, nil, f.cookie)
	requireStatus(t, list, http.StatusOK)
	var jobs struct{ Jobs []sandboxHistoryJob }
	if err = json.Unmarshal(list.Body.Bytes(), &jobs); err != nil || len(jobs.Jobs) != 1 || jobs.Jobs[0].ID != id {
		t.Fatalf("list=%s %v", list.Body, err)
	}
	read := requestJSON(t, f.h, http.MethodGet, base+"/"+id, nil, f.cookie)
	requireStatus(t, read, http.StatusOK)
	var out struct {
		Job  sandboxHistoryJob
		Data []byte `json:"dataBase64"`
	}
	if err = json.Unmarshal(read.Body.Bytes(), &out); err != nil || string(out.Data) != string(value) || out.Job.ExitCode == nil || *out.Job.ExitCode != exit {
		t.Fatalf("history receipt: %+v %v", out, err)
	}
	_, _, foreignCookie := workspaceSession(t, f.s, "sandbox-outsider@example.com")
	foreign := requestJSON(t, f.h, http.MethodGet, base+"/"+id, nil, foreignCookie)
	if foreign.Code == http.StatusOK {
		t.Fatal("foreign account read retained output")
	}
	if _, err = f.s.pool.Exec(ctx, `UPDATE sandbox_jobs SET history_terminal_at=now()-interval '8 days' WHERE sandbox_id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	read = requestJSON(t, f.h, http.MethodGet, base+"/"+id, nil, f.cookie)
	requireStatus(t, read, http.StatusOK)
	if err = json.Unmarshal(read.Body.Bytes(), &out); err != nil || out.Job.Output != "expired" || len(out.Data) != 0 {
		t.Fatalf("expiry receipt: %+v %v", out, err)
	}
}

func TestCILogReaderSearchesBeyondTailAndChecksRepository(t *testing.T) {
	s, h, mock, conversation, token, private, public := ciMCPSession(t)
	s.SetCILogReaderEnabled(true)
	ctx := context.Background()
	for _, id := range []string{private, public} {
		_, err := s.pool.Exec(ctx, `INSERT INTO ci_job_logs(workspace_id,job_id,sequence,stream,content)
			SELECT $1,$2,seq,'stdout',CASE seq WHEN 1 THEN 'start cross' WHEN 2 THEN 'chunk match'||chr(10) ELSE 'later'||chr(10) END
			FROM generate_series(1,550) seq`, conversation.WorkspaceID, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	base := "/v1/workspaces/" + conversation.WorkspaceID + "/ci/jobs/"
	response := requestJSON(t, h, http.MethodGet, base+public+"/logs?q=crosschunk", nil, &http.Cookie{Name: "canter_session", Value: token})
	requireStatus(t, response, http.StatusOK)
	var page ciLogPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || page.Matches != 1 || page.MatchOffset == nil || *page.MatchOffset != 0 ||
		len(page.Lines) == 0 || page.Lines[0].Text != "start crosschunk match\n" || page.Next == nil {
		t.Fatalf("page: %+v %v", page, err)
	}
	// Switch this session to a GitHub identity without repository access.
	before := requestJSON(t, h, http.MethodGet, base+private+"/logs", nil, &http.Cookie{Name: "canter_session", Value: token})
	requireStatus(t, before, http.StatusOK)
	mock.mu.Lock()
	mock.userDenied["octo/priv"] = true
	mock.mu.Unlock()
	denied := requestJSON(t, h, http.MethodGet, base+private+"/logs?q=crosschunk", nil, &http.Cookie{Name: "canter_session", Value: token})
	if denied.Code == http.StatusOK {
		t.Fatal("revoked repository identity retained log access")
	}
}

type transferTestGuest struct {
	sandboxFakeClient
	staged wrencomputer.FileUpload
}

func (c *transferTestGuest) SandboxTransfer(_ context.Context, op string, input, out any) error {
	switch op {
	case "stat":
		*out.(*[]string) = []string{"missing"}
		return nil
	case "commit":
		if c.staged.Offset != c.staged.Size {
			return ErrConflict
		}
		c.staged.Complete = true
		*out.(*wrencomputer.FileUpload) = c.staged
		return nil
	default:
		return ErrConflict
	}
}

func (c *transferTestGuest) BeginUpload(_ context.Context, id, path string, size int64, sha string) (wrencomputer.FileUpload, error) {
	if c.staged.ID == "" {
		c.staged = wrencomputer.FileUpload{ID: id, Path: path, Size: size, SHA256: sha}
	}
	return c.staged, nil
}

func (c *transferTestGuest) UploadChunk(_ context.Context, id string, offset int64, data []byte) (wrencomputer.FileUpload, error) {
	if id != c.staged.ID || offset != c.staged.Offset {
		return wrencomputer.FileUpload{}, ErrConflict
	}
	c.staged.Offset += int64(len(data))
	return c.staged, nil
}

func TestSandboxTransferRoutesResumeAndFenceGeneration(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	f.h.config.Sandboxes.DirectoryTransfers = true
	v := f.ready(t, f.create(t, "transfer", "free-1x1", http.StatusAccepted))
	fake := &transferTestGuest{sandboxFakeClient: sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1, TransferProtocol: 1}}}
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) { return fake, nil }
	ctx := context.Background()
	base := "/v1/workspaces/" + v.WorkspaceID + "/sandboxes/" + v.ID + "/transfers"
	manifest := sdk.SandboxTransferRequest{Direction: "push", Path: "/workspace/project", SandboxTransferManifest: sdk.SandboxTransferManifest{Entries: []sdk.SandboxTransferEntry{
		{Path: "é file.sh", Size: 4, SHA256: strings.Repeat("a", 64)},
	}}}
	response := requestJSON(t, f.h, http.MethodPost, base+"?dryRun=true", manifest, f.cookie)
	requireStatus(t, response, http.StatusOK)
	var dry sdk.SandboxTransfer
	if err := json.Unmarshal(response.Body.Bytes(), &dry); err != nil || dry.ID != "" || dry.State != "preview" {
		t.Fatalf("dry plan: %+v %v", dry, err)
	}
	newPlan := func() sdk.SandboxTransfer {
		t.Helper()
		response := requestJSON(t, f.h, http.MethodPost, base, manifest, f.cookie)
		requireStatus(t, response, http.StatusOK)
		var plan sdk.SandboxTransfer
		if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil || plan.ID == "" {
			t.Fatalf("plan: %+v %v", plan, err)
		}
		return plan
	}
	plan := newPlan()
	entry := base + "/" + plan.ID + "/0"
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, entry+"/begin", nil, f.cookie), http.StatusOK)
	put := func(offset int, data string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("%s/chunk?offset=%d", entry, offset), strings.NewReader(data))
		req.AddCookie(f.cookie)
		req.Header.Set("Origin", "http://canter.test")
		req.Header.Set("Content-Type", "application/octet-stream")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		requireStatus(t, rec, http.StatusOK)
	}
	put(0, "ab")
	response = requestJSON(t, f.h, http.MethodPost, entry+"/begin", nil, f.cookie)
	requireStatus(t, response, http.StatusOK)
	var resumed sdk.SandboxTransferFileReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &resumed); err != nil || resumed.Offset != 2 {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	put(2, "cd")
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, entry+"/complete", nil, f.cookie), http.StatusOK)
	response = requestJSON(t, f.h, http.MethodGet, base+"/"+plan.ID, nil, f.cookie)
	requireStatus(t, response, http.StatusOK)
	if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil || plan.State != "complete" || plan.Receipts[0].Offset != 4 {
		t.Fatalf("transfer receipt: %+v %v", plan, err)
	}
	stale := newPlan()
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET generation=generation+1 WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, base+"/"+stale.ID+"/0/begin", nil, f.cookie), http.StatusConflict)
	_, _, outsider := workspaceSession(t, f.s, "transfer-outsider@example.com")
	if response = requestJSON(t, f.h, http.MethodGet, base+"/"+plan.ID, nil, outsider); response.Code == http.StatusOK {
		t.Fatal("foreign account read a transfer receipt")
	}
}
