package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
)

func ciAgentToken(t *testing.T, s *Store, ctx context.Context, accountID, workspaceID string, auth Authority) (string, string) {
	t.Helper()
	pairing, err := s.CreateAgentPairing(ctx, accountID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	device, err := s.ClaimAgentPairing(ctx, pairing.Token, "ci-agent", "test", "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApproveAgentPairing(ctx, pairing.ID, accountID, false, auth); err != nil {
		t.Fatal(err)
	}
	pair, err := s.ExchangeDevice(ctx, device.DeviceCode, "ci-agent-session")
	if err != nil {
		t.Fatal(err)
	}
	return pair.AccessToken, pair.Installation.ID
}

func ciBearer(t *testing.T, h *HTTPServer, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCIBindFirstUseRequiresOwner(t *testing.T) {
	for _, role := range []string{"operator", "viewer"} {
		t.Run(role, func(t *testing.T) {
			s, c, _ := operatorFixture(t)
			ctx := context.Background()
			hash := strings.Repeat("a", 64)
			if err := s.CreateCIConnectState(ctx, c.WorkspaceID, c.AccountID, hash, time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(ctx, `UPDATE memberships SET role=$3 WHERE workspace_id=$1 AND account_id=$2`, c.WorkspaceID, c.AccountID, role); err != nil {
				t.Fatal(err)
			}
			if _, err := s.BindCIInstallation(ctx, c.WorkspaceID, c.AccountID, hash, 7001, 4242, "octo", "User", []ciGitHubRepo{{ID: 8001, FullName: "octo/repo"}}); !errors.Is(err, errCINotWorkspaceOwner) {
				t.Fatalf("non-owner binding: %v", err)
			}
			assertFirstUseProduct(t, s, c.WorkspaceID, "ci", false, 0)
			if _, _, _, err := s.GetCIConnectState(ctx, hash); err != nil {
				t.Fatalf("rejected binding consumed state: %v", err)
			}
		})
	}
}

func TestCIGuardsAgentVisibilityAndCancel(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	mock := newCIMockGitHub(t)
	h := ciHardeningServer(s, ciHardeningConfig(t, mock), true)
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	instID := ciSeedInstallation(t, s, ctx, c.WorkspaceID, 7001)
	ciSeedRepo(t, s, ctx, c.WorkspaceID, instID, 9601, "octo/agentpub", false)
	ciSeedRepo(t, s, ctx, c.WorkspaceID, instID, 9602, "octo/agentpriv", true)
	mock.repoInfo["octo/agentpub"] = ciRepoInfo{ID: 9601, Private: boolPtr(false)}
	mock.repoInfo["octo/agentpriv"] = ciRepoInfo{ID: 9602, Private: boolPtr(true)}
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-agent-1"), "workflow_job", ciJobWebhookBody(7001, 9601, 5961, 6961, "queued", []string{"canter"}, nil)); rec.Code != http.StatusOK {
		t.Fatalf("public job: %d", rec.Code)
	}
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-agent-2"), "workflow_job", ciJobWebhookBody(7001, 9602, 5962, 6962, "queued", []string{"canter"}, nil)); rec.Code != http.StatusOK {
		t.Fatalf("private job: %d", rec.Code)
	}
	var privJobID string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM ci_jobs WHERE github_job_id=5962`).Scan(&privJobID); err != nil {
		t.Fatal(err)
	}
	var pubJobID string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM ci_jobs WHERE github_job_id=5961`).Scan(&pubJobID); err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + c.WorkspaceID + "/ci"
	noInspect, noInspectID := ciAgentToken(t, s, ctx, c.AccountID, c.WorkspaceID, Authority{Inspect: true, Draft: true, ApplyMode: "human-approval-required"})
	if _, err := s.pool.Exec(ctx, `UPDATE agent_installations SET inspect_allowed=false WHERE id=$1`, noInspectID); err != nil {
		t.Fatal(err)
	}
	if rec := ciBearer(t, h, http.MethodGet, base, noInspect); rec.Code != http.StatusForbidden {
		t.Fatalf("agent without Inspect overview: got %d want 403", rec.Code)
	}
	if rec := ciBearer(t, h, http.MethodGet, base+"/jobs/"+pubJobID, noInspect); rec.Code != http.StatusForbidden {
		t.Fatalf("agent without Inspect detail: got %d want 403", rec.Code)
	}
	inspectOnly, _ := ciAgentToken(t, s, ctx, c.AccountID, c.WorkspaceID, Authority{Inspect: true, ApplyMode: "never"})
	rec := ciBearer(t, h, http.MethodGet, base, inspectOnly)
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect agent overview: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "octo/agentpub") {
		t.Fatal("inspect agent must see fresh-public job")
	}
	if strings.Contains(rec.Body.String(), "octo/agentpriv") {
		t.Fatal("inspect agent must not see private job")
	}
	if rec := ciBearer(t, h, http.MethodGet, base+"/jobs/"+privJobID, inspectOnly); rec.Code != http.StatusNotFound {
		t.Fatalf("inspect agent private detail: got %d want 404", rec.Code)
	}
	if rec := ciBearer(t, h, http.MethodPost, base+"/jobs/"+pubJobID+"/cancel", inspectOnly); rec.Code != http.StatusForbidden {
		t.Fatalf("inspect-only cancel: got %d want 403", rec.Code)
	}
	draftAgent, _ := ciAgentToken(t, s, ctx, c.AccountID, c.WorkspaceID, Authority{Inspect: true, Draft: true, ApplyMode: "human-approval-required"})
	if rec := ciBearer(t, h, http.MethodPost, base+"/jobs/"+pubJobID+"/cancel", draftAgent); rec.Code != http.StatusConflict {
		t.Fatalf("draft agent cancel nonterminal: got %d want 409", rec.Code)
	}
	operatorAccount, _, operatorToken, err := s.Signup(ctx, "ci-operator@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, operatorAccount.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	viewerAccount, _, viewerToken, err := s.Signup(ctx, "ci-viewer@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, viewerAccount.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	ciCookie := func(method, path, session string) *httptest.ResponseRecorder {
		t.Helper()
		req := ciAuthedRequest(t, method, path, session, "")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := ciCookie(http.MethodPost, base+"/jobs/"+pubJobID+"/cancel", operatorToken); rec.Code != http.StatusConflict {
		t.Fatalf("operator cancel nonterminal: got %d want 409", rec.Code)
	}
	if rec := ciCookie(http.MethodPost, base+"/jobs/"+pubJobID+"/cancel", viewerToken); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer cancel: got %d want 403", rec.Code)
	}
	_, strangerWS, strangerToken, err := s.Signup(ctx, "ci-stranger@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	strangerBase := "/v1/workspaces/" + strangerWS.ID + "/ci"
	if rec := ciCookie(http.MethodGet, strangerBase+"/jobs/"+pubJobID, strangerToken); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace job detail: got %d want 404", rec.Code)
	}
	var jobStatus string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM ci_jobs WHERE id=$1`, pubJobID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "queued" {
		t.Fatalf("cancel attempts must not change job status, got %s", jobStatus)
	}
	var logs, audits int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_job_logs WHERE job_id=$1`, pubJobID).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action='ci.job.cancelled'`, c.WorkspaceID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if logs != 0 || audits != 0 {
		t.Fatalf("cancel attempts must not mutate logs/audit: logs=%d audits=%d", logs, audits)
	}
	mock.mu.Lock()
	mock.repoInfo["octo/agentpub"] = ciRepoInfo{ID: 9601, Private: boolPtr(true)}
	mock.mu.Unlock()
	rec = ciBearer(t, h, http.MethodGet, base, inspectOnly)
	if strings.Contains(rec.Body.String(), "octo/agentpub") {
		t.Fatal("source private flip must hide job for agent too")
	}
}

func ciSeedFinancialWorkspace(t *testing.T, s *Store, h *HTTPServer, email string) (*http.Cookie, Principal) {
	t.Helper()
	cookie, p := deletionAccount(t, h, email)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_github_installations(id,workspace_id,github_installation_id,github_account_id,owner,account_type,created_by) VALUES('ci_financial_installation',$1,9100,4242,'ci-tests','User',$2)`, p.WorkspaceID, p.Account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_repositories(workspace_id,installation_id,github_repository_id,full_name,private,enabled) VALUES($1,'ci_financial_installation',789,'ci-tests/repo',true,true)`, p.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableWorkspaceProduct(ctx, p.WorkspaceID, "ci", sdk.ActorRef{Kind: "human", ID: p.Account.ID}); err != nil {
		t.Fatal(err)
	}
	return cookie, p
}

func TestCIGuardsAccountDeletionAndDisable(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	cookie, p := ciSeedFinancialWorkspace(t, s, h, "ci-delete@example.com")
	otherCookie, otherP := deletionAccount(t, h, "ci-other@example.com")
	_ = otherCookie
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_github_installations(id,workspace_id,github_installation_id,github_account_id,owner,account_type,created_by) VALUES('ci_other_installation',$1,9200,4242,'ci-tests','User',$2)`, otherP.WorkspaceID, otherP.Account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_repositories(workspace_id,installation_id,github_repository_id,full_name,private,enabled) VALUES($1,'ci_other_installation',790,'ci-other/repo',true,true)`, otherP.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	runner := ciFinancialRunner(t, s, p.WorkspaceID, "linux-x64-2", 1, false, time.Now())
	if err := s.DisableWorkspaceProduct(ctx, p.WorkspaceID, "ci"); !errors.Is(err, ErrConflict) {
		t.Fatalf("disable with live runner: got %v want conflict", err)
	}
	blockedStart := authRequest(t, h, "account/delete/start", map[string]string{"password": "correct horse battery staple"}, cookie)
	if blockedStart.Code != http.StatusConflict {
		t.Fatalf("deletion start with live CI runner: got %d want 409: %s", blockedStart.Code, blockedStart.Body.String())
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET phase='deleted' WHERE id=$1`, runner); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableWorkspaceProduct(ctx, p.WorkspaceID, "ci"); err != nil {
		t.Fatalf("disable after runner deleted: %v", err)
	}
	if err := s.EnableWorkspaceProduct(ctx, p.WorkspaceID, "ci", sdk.ActorRef{Kind: "human", ID: p.Account.ID}); err != nil {
		t.Fatal(err)
	}
	deletion := startDeletion(t, h, cookie)
	code := lastEmailCode(t, h)
	finish := authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, cookie, deletion)
	if finish.Code != http.StatusOK {
		t.Fatalf("deletion after CI cleanup: %d %s", finish.Code, finish.Body.String())
	}
	for _, table := range []string{"ci_github_installations", "ci_repositories", "ci_jobs", "ci_runners", "ci_connect_states", "ci_settings"} {
		var count int
		if err := s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE workspace_id=$1`, table), p.WorkspaceID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows survived account deletion: %d", table, count)
		}
	}
	var otherInstallations, otherRepos int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_github_installations WHERE workspace_id=$1`, otherP.WorkspaceID).Scan(&otherInstallations); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_repositories WHERE workspace_id=$1`, otherP.WorkspaceID).Scan(&otherRepos); err != nil {
		t.Fatal(err)
	}
	if otherInstallations != 1 || otherRepos != 1 {
		t.Fatalf("other workspace CI metadata must be untouched: installations=%d repos=%d", otherInstallations, otherRepos)
	}
}

func TestCIGuardsSharedWorkspaceSurvives(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	creatorCookie, creator := ciSeedFinancialWorkspace(t, s, h, "ci-creator@example.com")
	memberAccount, _, _, err := s.Signup(ctx, "ci-member@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'owner')`, memberAccount.ID, creator.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO ci_jobs(id,workspace_id,installation_id,github_repository_id,github_job_id,github_run_id,status,document) VALUES('ci_shared_job',$1,'ci_financial_installation',789,995,995,'completed','{"job":{"id":"ci_shared_job","status":"completed"},"steps":[]}'::jsonb)`, creator.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	runner := ciFinancialRunner(t, s, creator.WorkspaceID, "linux-x64-2", 2, false, time.Now())
	deletion := startDeletion(t, h, creatorCookie)
	code := lastEmailCode(t, h)
	finish := authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, creatorCookie, deletion)
	if finish.Code != http.StatusOK {
		t.Fatalf("shared workspace deletion: %d %s", finish.Code, finish.Body.String())
	}
	var workspaceCount int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM workspaces WHERE id=$1`, creator.WorkspaceID).Scan(&workspaceCount); err != nil {
		t.Fatal(err)
	}
	if workspaceCount != 1 {
		t.Fatal("shared workspace must survive creator deletion")
	}
	var createdBy *string
	var jobs int
	if err = s.pool.QueryRow(ctx, `SELECT created_by FROM ci_github_installations WHERE id='ci_financial_installation'`).Scan(&createdBy); err != nil {
		t.Fatal(err)
	}
	if createdBy != nil {
		t.Fatalf("created_by must be NULL after account deletion, got %v", *createdBy)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_jobs WHERE workspace_id=$1 AND id='ci_shared_job'`, creator.WorkspaceID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatal("CI team history must stay on surviving workspace")
	}
	var runnerPhase string
	if err = s.pool.QueryRow(ctx, `SELECT phase FROM ci_runners WHERE id=$1`, runner).Scan(&runnerPhase); err != nil {
		t.Fatal(err)
	}
	if runnerPhase != "queued" {
		t.Fatalf("shared workspace live runner must survive creator deletion, phase=%s", runnerPhase)
	}
}

func assertFirstUseProduct(t *testing.T, s *Store, workspace, product string, wantEnabled bool, wantAudits int) {
	t.Helper()
	ctx := context.Background()
	enabled, err := s.productEnabled(ctx, s.pool, workspace, product)
	if err != nil || enabled != wantEnabled {
		t.Fatalf("%s enabled=%v want=%v: %v", product, enabled, wantEnabled, err)
	}
	var audits int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action='product.enabled' AND subject=$2 AND metadata->>'implicit'='true'`, workspace, product).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != wantAudits {
		t.Fatalf("%s implicit activation audits=%d want=%d", product, audits, wantAudits)
	}
}
