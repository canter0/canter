package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

func ciTestKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

const ciTestWebhookSecret = "hardening-webhook-secret-0123456789"

type ciMockGitHub struct {
	mu               sync.Mutex
	server           *httptest.Server
	appID            int64
	installationID   int64
	accountType      string
	accountID        int64
	accountLogin     string
	userID           int64
	userLogin        string
	membershipRole   string
	membershipState  string
	suspended        bool
	repos            []ciGitHubRepo
	repoInfo         map[string]ciRepoInfo
	repoRaw          map[string]string
	userDenied       map[string]bool
	tokenGrants      map[string]map[string]bool
	repoBearer       map[string]string
	onRepos          func()
	redirectHits     atomic.Int32
	authOnRedirect   atomic.Int32
	largeBody        bool
	installationHits atomic.Int32
}

func newCIMockGitHub(t *testing.T) *ciMockGitHub {
	t.Helper()
	m := &ciMockGitHub{
		appID:           1234,
		installationID:  7001,
		accountType:     "User",
		accountID:       4242,
		accountLogin:    "octo",
		userID:          4242,
		userLogin:       "octo",
		membershipRole:  "admin",
		membershipState: "active",
		repoInfo:        map[string]ciRepoInfo{},
		repoRaw:         map[string]string{},
		userDenied:      map[string]bool{},
		tokenGrants:     map[string]map[string]bool{},
		repoBearer:      map[string]string{},
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

func (m *ciMockGitHub) handle(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	path := r.URL.Path
	w.Header().Set("Content-Type", "application/json")
	switch {
	case path == "/redirect-target":
		m.redirectHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			m.authOnRedirect.Add(1)
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	case strings.HasPrefix(path, "/app/installations/") && strings.HasSuffix(path, "/access_tokens"):
		fmt.Fprint(w, `{"token":"inst-token-1","expires_at":"2099-01-01T00:00:00Z"}`)
	case strings.HasPrefix(path, "/app/installations/"):
		m.installationHits.Add(1)
		var suspended any
		if m.suspended {
			suspended = "2024-01-01T00:00:00Z"
		}
		fmt.Fprintf(w, `{"id":%d,"app_id":%d,"account":{"id":%d,"login":%q,"type":%q},"suspended_at":%s}`, m.installationID, m.appID, m.accountID, m.accountLogin, m.accountType, suspendedJSON(suspended))
	case path == "/user":
		if r.Header.Get("Authorization") != "Bearer user-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"bad credentials"}`)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"login":%q}`, m.userID, m.userLogin)
	case strings.HasPrefix(path, "/orgs/") && strings.Contains(path, "/memberships/"):
		fmt.Fprintf(w, `{"role":%q,"state":%q}`, m.membershipRole, m.membershipState)
	case path == "/installation/repositories":
		if m.onRepos != nil {
			m.onRepos()
		}
		if m.largeBody {
			w.Write(make([]byte, 9<<20))
			return
		}
		fmt.Fprintf(w, `{"total_count":%d,"repositories":[`, len(m.repos))
		for i, repo := range m.repos {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"id":%d,"full_name":%q,"private":%t,"default_branch":"main"}`, repo.ID, repo.FullName, repo.Private)
		}
		fmt.Fprint(w, `]}`)
	case strings.HasPrefix(path, "/repos/"):
		if m.largeBody {
			w.Write(make([]byte, 9<<20))
			return
		}
		name := strings.TrimPrefix(path, "/repos/")
		name = strings.ReplaceAll(name, "%2F", "/")
		m.repoBearer[name] = r.Header.Get("Authorization")
		if raw, ok := m.repoRaw[name]; ok {
			fmt.Fprint(w, raw)
			return
		}
		info, ok := m.repoInfo[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		private := info.Private != nil && *info.Private
		if private {
			auth := r.Header.Get("Authorization")
			token := strings.TrimPrefix(auth, "Bearer ")
			granted := auth == "Bearer user-tok" && !m.userDenied[name]
			if grants, ok := m.tokenGrants[name]; ok {
				granted = grants[token]
			}
			if !granted {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
		}
		if info.Private == nil {
			fmt.Fprintf(w, `{"id":%d}`, info.ID)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"private":%t}`, info.ID, *info.Private)
	default:
		if strings.HasPrefix(path, "/app/installations/") && r.Method == http.MethodPost && strings.HasSuffix(path, "/access_tokens") {
			fmt.Fprint(w, `{"token":"inst-token-1"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"unmocked `+path+`"}`)
	}
}

func suspendedJSON(v any) string {
	if v == nil {
		return "null"
	}
	return `"` + v.(string) + `"`
}

func ciHardeningConfig(t *testing.T, mock *ciMockGitHub) CIConfig {
	t.Helper()
	return CIConfig{
		AppID:         1234,
		AppSlug:       "canter-ci",
		PrivateKeyPEM: ciTestKey(t),
		WebhookSecret: []byte(ciTestWebhookSecret),
		APIBaseURL:    mock.server.URL,
	}
}

func ciHardeningServer(s *Store, cfg CIConfig, withGitHubOAuth bool) *HTTPServer {
	config := HTTPConfig{PublicURL: "http://canter.test", CI: cfg}
	if withGitHubOAuth {
		config.GitHubOAuth = OAuthCredentials{ClientID: "gh-id", ClientSecret: "gh-secret"}
	}
	return NewHTTPServer(&Service{Store: s}, config).(*HTTPServer)
}

func ciConnectState(t *testing.T, h *HTTPServer, token, workspaceID string) string {
	t.Helper()
	req := ciAuthedRequest(t, http.MethodPost, "/v1/workspaces/"+workspaceID+"/ci/github/connect", token, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(out.URL)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatalf("connect URL missing state: %s", out.URL)
	}
	return state
}

func ciCallback(t *testing.T, h *HTTPServer, installationID int64, state string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/ci/github/callback?installation_id=%d&state=%s", installationID, url.QueryEscape(state)), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ciSaveGitHub(t *testing.T, h *HTTPServer, ctx context.Context, account, workspace string) {
	t.Helper()
	token := (&oauth2.Token{AccessToken: "user-tok", Expiry: time.Now().Add(time.Hour)})
	if err := h.saveGitHubConnection(ctx, account, workspace, "123", "octo", "repo", token); err != nil {
		t.Fatal(err)
	}
}

func ciEnableProduct(t *testing.T, s *Store, ctx context.Context, workspaceID, accountID string) {
	t.Helper()
	if err := s.EnableWorkspaceProduct(ctx, workspaceID, "ci", sdk.ActorRef{Kind: "human", ID: accountID}); err != nil {
		t.Fatal(err)
	}
}

func ciSeedInstallation(t *testing.T, s *Store, ctx context.Context, workspaceID string, ghInstallationID int64) string {
	t.Helper()
	id, err := newID("ciinst_")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO ci_github_installations(id,workspace_id,github_installation_id,github_account_id,owner,account_type,status) VALUES($1,$2,$3,4242,'octo','User','active')`, id, workspaceID, ghInstallationID); err != nil {
		t.Fatal(err)
	}
	return id
}

func ciSeedRepo(t *testing.T, s *Store, ctx context.Context, workspaceID, installationID string, repoID int64, fullName string, private bool) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_repositories(workspace_id,installation_id,github_repository_id,full_name,private,default_branch,enabled) VALUES($1,$2,$3,$4,$5,'main',true)`, workspaceID, installationID, repoID, fullName, private); err != nil {
		t.Fatal(err)
	}
}

func ciJobWebhookBody(installationID, repoID, jobID, runID int64, status string, labels []string, conclusion *string) string {
	labelJSON := "["
	for i, l := range labels {
		if i > 0 {
			labelJSON += ","
		}
		labelJSON += fmt.Sprintf("%q", l)
	}
	labelJSON += "]"
	conclusionField := ""
	if conclusion != nil {
		conclusionField = fmt.Sprintf(`,"conclusion":%q`, *conclusion)
	}
	started := `"2024-01-01T00:00:00Z"`
	completed := `,"completed_at":"2024-01-01T00:01:00Z"`
	if status != "completed" {
		completed = ""
	}
	return fmt.Sprintf(`{"action":%q,"installation":{"id":%d},"repository":{"id":%d},"workflow_job":{"id":%d,"run_id":%d,"name":"build","workflow_name":"ci","head_sha":"abc123def","head_branch":"main","status":%q%s,"started_at":%s%s,"labels":%s,"steps":[{"name":"step1","number":1,"status":"completed","conclusion":"success"}]}}`, map[string]string{"queued": "queued", "in_progress": "in_progress", "completed": "completed"}[status], installationID, repoID, jobID, runID, status, conclusionField, started, completed, labelJSON)
}

var ciDeliverySeq atomic.Int64

func ciDeliveryID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), ciDeliverySeq.Add(1))
}

func ciPostWebhook(t *testing.T, h *HTTPServer, delivery, event, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := ciWebhookRequest(t, ciTestWebhookSecret, delivery, event, body, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ciJobRowCount(t *testing.T, s *Store, ctx context.Context, githubJobID int64) (count int, status string) {
	t.Helper()
	err := s.pool.QueryRow(ctx, `SELECT count(*),COALESCE(max(status),'') FROM ci_jobs WHERE github_job_id=$1`, githubJobID).Scan(&count, &status)
	if err != nil {
		t.Fatal(err)
	}
	return count, status
}

func TestCIHardeningCallbackUserSuccessAndReplay(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	mock := newCIMockGitHub(t)
	mock.repos = []ciGitHubRepo{{ID: 8001, FullName: "octo/app", Private: true, DefaultBranch: "main"}, {ID: 8002, FullName: "octo/lib", Private: false, DefaultBranch: "main"}}
	mock.repoInfo["octo/app"] = ciRepoInfo{ID: 8001, Private: boolPtr(true)}
	mock.repoInfo["octo/lib"] = ciRepoInfo{ID: 8002, Private: boolPtr(false)}
	h := ciHardeningServer(s, ciHardeningConfig(t, mock), true)
	ciSaveGitHub(t, h, ctx, c.AccountID, c.WorkspaceID)
	state := ciConnectState(t, h, token, c.WorkspaceID)
	// Starting a connection does not activate CI; the verified binding does.
	assertFirstUseProduct(t, s, c.WorkspaceID, "ci", false, 0)
	rec := ciCallback(t, h, 7001, state)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasSuffix(loc, "/app/ci") {
		t.Fatalf("callback redirect: %s", loc)
	}
	var installations, repos int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_github_installations WHERE workspace_id=$1 AND github_installation_id=7001`, c.WorkspaceID).Scan(&installations); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_repositories r JOIN ci_github_installations i ON i.id=r.installation_id WHERE i.workspace_id=$1`, c.WorkspaceID).Scan(&repos); err != nil {
		t.Fatal(err)
	}
	if installations != 1 || repos != 2 {
		t.Fatalf("binding: installations=%d repos=%d", installations, repos)
	}
	assertFirstUseProduct(t, s, c.WorkspaceID, "ci", true, 1)
	rec = ciCallback(t, h, 7001, state)
	if rec.Code == http.StatusSeeOther {
		t.Fatal("replayed state must not bind again")
	}
	assertFirstUseProduct(t, s, c.WorkspaceID, "ci", true, 1)
}

func TestCIHardeningCallbackOrgAdminAndReconnect(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	mock := newCIMockGitHub(t)
	mock.accountType = "Organization"
	mock.accountLogin = "myorg"
	mock.accountID = 9001
	mock.repos = []ciGitHubRepo{{ID: 8101, FullName: "myorg/a", Private: true}, {ID: 8102, FullName: "myorg/b", Private: false}}
	mock.repoInfo["myorg/a"] = ciRepoInfo{ID: 8101, Private: boolPtr(true)}
	mock.repoInfo["myorg/b"] = ciRepoInfo{ID: 8102, Private: boolPtr(false)}
	h := ciHardeningServer(s, ciHardeningConfig(t, mock), true)
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	ciSaveGitHub(t, h, ctx, c.AccountID, c.WorkspaceID)
	state := ciConnectState(t, h, token, c.WorkspaceID)
	if rec := ciCallback(t, h, 7001, state); rec.Code != http.StatusSeeOther {
		t.Fatalf("org admin callback: %d %s", rec.Code, rec.Body.String())
	}
	mock.mu.Lock()
	mock.repos = []ciGitHubRepo{{ID: 8101, FullName: "myorg/a", Private: true}}
	mock.mu.Unlock()
	state2 := ciConnectState(t, h, token, c.WorkspaceID)
	if rec := ciCallback(t, h, 7001, state2); rec.Code != http.StatusSeeOther {
		t.Fatalf("reconnect: %d %s", rec.Code, rec.Body.String())
	}
	var enabled, disabled int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE enabled),count(*) FILTER (WHERE NOT enabled) FROM ci_repositories`).Scan(&enabled, &disabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || disabled != 1 {
		t.Fatalf("reconnect selection: enabled=%d disabled=%d", enabled, disabled)
	}
}

func TestCIHardeningCallbackProofRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ciMockGitHub)
		want   int
	}{
		{"user id mismatch", func(m *ciMockGitHub) { m.userID = 9999 }, http.StatusForbidden},
		{"suspended", func(m *ciMockGitHub) { m.suspended = true }, http.StatusConflict},
		{"org nonadmin", func(m *ciMockGitHub) {
			m.accountType = "Organization"
			m.accountID = 9001
			m.accountLogin = "myorg"
			m.membershipRole = "member"
		}, http.StatusForbidden},
		{"org inactive", func(m *ciMockGitHub) {
			m.accountType = "Organization"
			m.accountID = 9001
			m.accountLogin = "myorg"
			m.membershipState = "pending"
		}, http.StatusForbidden},
		{"app id mismatch", func(m *ciMockGitHub) { m.appID = 9999 }, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c, token := operatorFixture(t)
			ctx := context.Background()
			mock := newCIMockGitHub(t)
			tc.mutate(mock)
			mock.repos = []ciGitHubRepo{{ID: 8201, FullName: "octo/x", Private: true}}
			mock.repoInfo["octo/x"] = ciRepoInfo{ID: 8201, Private: boolPtr(true)}
			h := ciHardeningServer(s, ciHardeningConfig(t, mock), true)
			ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
			ciSaveGitHub(t, h, ctx, c.AccountID, c.WorkspaceID)
			state := ciConnectState(t, h, token, c.WorkspaceID)
			rec := ciCallback(t, h, 7001, state)
			if rec.Code != tc.want {
				t.Fatalf("got %d want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			var count int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_github_installations`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("failed proof must not bind")
			}
			sum := sha256.Sum256([]byte(state))
			ws2, acct2, _, err := s.GetCIConnectState(ctx, hex.EncodeToString(sum[:]))
			if err != nil || ws2 != c.WorkspaceID || acct2 != c.AccountID {
				t.Fatalf("failed proof must leave state retryable: %v", err)
			}
		})
	}
}

func TestCIHardeningCallbackCrossWorkspaceAndTOCTOU(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	mock := newCIMockGitHub(t)
	mock.repos = []ciGitHubRepo{{ID: 8301, FullName: "octo/x", Private: true}}
	mock.repoInfo["octo/x"] = ciRepoInfo{ID: 8301, Private: boolPtr(true)}
	h := ciHardeningServer(s, ciHardeningConfig(t, mock), true)
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	ciSaveGitHub(t, h, ctx, c.AccountID, c.WorkspaceID)
	state := ciConnectState(t, h, token, c.WorkspaceID)
	if rec := ciCallback(t, h, 7001, state); rec.Code != http.StatusSeeOther {
		t.Fatalf("first bind: %d", rec.Code)
	}
	other, otherWorkspace, otherToken, err := s.Signup(ctx, "second@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	ciSaveGitHub(t, h, ctx, other.ID, otherWorkspace.ID)
	state2 := ciConnectState(t, h, otherToken, otherWorkspace.ID)
	rec := ciCallback(t, h, 7001, state2)
	if rec.Code != http.StatusConflict {
		t.Fatalf("cross-workspace bind: got %d want 409: %s", rec.Code, rec.Body.String())
	}
	assertFirstUseProduct(t, s, otherWorkspace.ID, "ci", false, 0)
	var owner string
	if err = s.pool.QueryRow(ctx, `SELECT workspace_id FROM ci_github_installations WHERE github_installation_id=7001`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != c.WorkspaceID {
		t.Fatal("cross-workspace bind must not reassign")
	}
	_ = other

	toctou := []struct {
		name   string
		mutate func(t *testing.T, s *Store, ctx context.Context, ws, acct, stateHash string)
		want   int
	}{
		{"state expired during fetch", func(t *testing.T, s *Store, ctx context.Context, ws, acct, hash string) {
			if _, err := s.pool.Exec(ctx, `UPDATE ci_connect_states SET expires_at=now()-interval '1 second' WHERE state_hash=$1`, hash); err != nil {
				t.Fatal(err)
			}
		}, http.StatusBadRequest},
		{"owner demoted during fetch", func(t *testing.T, s *Store, ctx context.Context, ws, acct, hash string) {
			if _, err := s.pool.Exec(ctx, `UPDATE memberships SET role='operator' WHERE workspace_id=$1 AND account_id=$2`, ws, acct); err != nil {
				t.Fatal(err)
			}
		}, http.StatusForbidden},
		{"account disabled during fetch", func(t *testing.T, s *Store, ctx context.Context, ws, acct, hash string) {
			if _, err := s.pool.Exec(ctx, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, acct); err != nil {
				t.Fatal(err)
			}
		}, http.StatusForbidden},
		{"ci disabled during fetch", func(t *testing.T, s *Store, ctx context.Context, ws, acct, hash string) {
			if _, err := s.pool.Exec(ctx, `DELETE FROM workspace_products WHERE workspace_id=$1 AND product='ci'`, ws); err != nil {
				t.Fatal(err)
			}
		}, http.StatusSeeOther},
	}
	for _, tc := range toctou {
		t.Run(tc.name, func(t *testing.T) {
			s2, c2, token2 := operatorFixture(t)
			ctx2 := context.Background()
			mock2 := newCIMockGitHub(t)
			mock2.installationID = 7100
			mock2.repos = []ciGitHubRepo{{ID: 8401, FullName: "octo/y", Private: true}}
			mock2.repoInfo["octo/y"] = ciRepoInfo{ID: 8401, Private: boolPtr(true)}
			state3 := ""
			mock2.onRepos = func() {
				sum := sha256.Sum256([]byte(state3))
				tc.mutate(t, s2, ctx2, c2.WorkspaceID, c2.AccountID, hex.EncodeToString(sum[:]))
			}
			h2 := ciHardeningServer(s2, ciHardeningConfig(t, mock2), true)
			ciEnableProduct(t, s2, ctx2, c2.WorkspaceID, c2.AccountID)
			ciSaveGitHub(t, h2, ctx2, c2.AccountID, c2.WorkspaceID)
			state3 = ciConnectState(t, h2, token2, c2.WorkspaceID)
			rec := ciCallback(t, h2, 7100, state3)
			if rec.Code != tc.want {
				t.Fatalf("got %d want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			var count int
			if err := s2.pool.QueryRow(ctx2, `SELECT count(*) FROM ci_github_installations WHERE github_installation_id=7100`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if tc.want == http.StatusSeeOther {
				if count != 1 {
					t.Fatal("authorized binding must activate and bind CI")
				}
				assertFirstUseProduct(t, s2, c2.WorkspaceID, "ci", true, 1)
			} else if count != 0 {
				t.Fatal("TOCTOU mutation must not bind")
			}
		})
	}
}

func TestCIHardeningVisibilityPerUser(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	mock := newCIMockGitHub(t)
	h := ciHardeningServer(s, ciHardeningConfig(t, mock), true)
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	instID := ciSeedInstallation(t, s, ctx, c.WorkspaceID, 7001)
	ciSeedRepo(t, s, ctx, c.WorkspaceID, instID, 9001, "octo/priv", true)
	ciSeedRepo(t, s, ctx, c.WorkspaceID, instID, 9002, "octo/pub", false)
	mock.repoInfo["octo/priv"] = ciRepoInfo{ID: 9001, Private: boolPtr(true)}
	mock.repoInfo["octo/pub"] = ciRepoInfo{ID: 9002, Private: boolPtr(false)}
	body := ciJobWebhookBody(7001, 9001, 5001, 6001, "completed", []string{"canter"}, strPtr("success"))
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-vis-1"), "workflow_job", body); rec.Code != http.StatusOK {
		t.Fatalf("seed private job: %d %s", rec.Code, rec.Body.String())
	}
	body = ciJobWebhookBody(7001, 9002, 5002, 6002, "completed", []string{"canter"}, strPtr("success"))
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-vis-2"), "workflow_job", body); rec.Code != http.StatusOK {
		t.Fatalf("seed public job: %d %s", rec.Code, rec.Body.String())
	}
	member, _, memberToken, err := s.Signup(ctx, "member@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, member.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	ownerWithToken := func(path string) *httptest.ResponseRecorder {
		req := ciAuthedRequest(t, http.MethodGet, path, token, "")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	memberReq := func(path string) *httptest.ResponseRecorder {
		req := ciAuthedRequest(t, http.MethodGet, path, memberToken, "")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	ciSaveGitHub(t, h, ctx, c.AccountID, c.WorkspaceID)
	rec := ownerWithToken("/v1/workspaces/" + c.WorkspaceID + "/ci")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner overview: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "octo/priv") || !strings.Contains(rec.Body.String(), "octo/pub") {
		t.Fatalf("owner with token should see both repos: %s", rec.Body.String())
	}
	rec = memberReq("/v1/workspaces/" + c.WorkspaceID + "/ci")
	if rec.Code != http.StatusOK {
		t.Fatalf("member overview: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "octo/priv") {
		t.Fatal("member without GitHub grant must not see private job")
	}
	if !strings.Contains(rec.Body.String(), "octo/pub") {
		t.Fatal("member should see public job anonymously")
	}
	privID := ""
	var jobID string
	if err = s.pool.QueryRow(ctx, `SELECT id FROM ci_jobs WHERE github_job_id=5001`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	privID = jobID
	rec = memberReq("/v1/workspaces/" + c.WorkspaceID + "/ci/jobs/" + privID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("member private detail: got %d want 404", rec.Code)
	}
	rec = ownerWithToken("/v1/workspaces/" + c.WorkspaceID + "/ci/jobs/" + privID)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner private detail: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "user-tok") || strings.Contains(rec.Body.String(), "encrypted") {
		t.Fatal("response must not contain credentials")
	}
	mock.mu.Lock()
	mock.repoInfo["octo/pub"] = ciRepoInfo{ID: 99999, Private: boolPtr(false)}
	mock.mu.Unlock()
	rec = memberReq("/v1/workspaces/" + c.WorkspaceID + "/ci")
	if strings.Contains(rec.Body.String(), "octo/pub") {
		t.Fatal("repo id mismatch must hide job (name reuse protection)")
	}
	mock.mu.Lock()
	mock.repoInfo["octo/pub"] = ciRepoInfo{ID: 9002, Private: boolPtr(true)}
	mock.mu.Unlock()
	rec = memberReq("/v1/workspaces/" + c.WorkspaceID + "/ci")
	if strings.Contains(rec.Body.String(), "octo/pub") {
		t.Fatal("public-to-private transition must hide job for anonymous viewers")
	}
}

func TestCIHardeningGitHubClientSafety(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	mock := newCIMockGitHub(t)
	h := ciHardeningServer(s, ciHardeningConfig(t, mock), true)
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	instID := ciSeedInstallation(t, s, ctx, c.WorkspaceID, 7001)
	ciSeedRepo(t, s, ctx, c.WorkspaceID, instID, 9101, "octo/down", false)
	mock.repoInfo["octo/down"] = ciRepoInfo{ID: 9101, Private: boolPtr(false)}
	body := ciJobWebhookBody(7001, 9101, 5101, 6101, "completed", []string{"canter"}, strPtr("success"))
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-safe-1"), "workflow_job", body); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d", rec.Code)
	}
	mock.mu.Lock()
	mock.largeBody = true
	mock.mu.Unlock()
	req := ciAuthedRequest(t, http.MethodGet, "/v1/workspaces/"+c.WorkspaceID+"/ci", token, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("oversized API response must fail closed 503: got %d", rec.Code)
	}
	mock.mu.Lock()
	mock.largeBody = false
	mock.onRepos = nil
	mock.mu.Unlock()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	down.Close()
	cfg := ciHardeningConfig(t, mock)
	cfg.APIBaseURL = down.URL
	h2 := ciHardeningServer(s, cfg, true)
	req = ciAuthedRequest(t, http.MethodGet, "/v1/workspaces/"+c.WorkspaceID+"/ci", token, "")
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable source must fail closed 503: got %d", rec.Code)
	}
	var redirectTargetHits, redirectAuthHits atomic.Int32
	redirectMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r" {
			http.Redirect(w, r, "/redirect-target", http.StatusFound)
			return
		}
		if r.URL.Path == "/redirect-target" {
			redirectTargetHits.Add(1)
			if r.Header.Get("Authorization") != "" {
				redirectAuthHits.Add(1)
			}
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	defer redirectMock.Close()
	cfg2 := ciHardeningConfig(t, mock)
	cfg2.APIBaseURL = redirectMock.URL
	cfg2.HTTPClient = &http.Client{}
	h3 := ciHardeningServer(s, cfg2, true)
	client := h3.ciGitHubTransport()
	_, err := client.repoInfo(ctx, "o/r", "user-tok")
	if err == nil {
		t.Fatal("redirected response must be rejected")
	}
	if redirectTargetHits.Load() != 0 || redirectAuthHits.Load() != 0 {
		t.Fatal("credentials must never reach redirect target")
	}
}

func TestCIHardeningWebhookEndToEnd(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	h := ciTestServer(s, CIConfig{WebhookSecret: []byte(ciTestWebhookSecret), APIBaseURL: "http://127.0.0.1:1"})
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	instID := ciSeedInstallation(t, s, ctx, c.WorkspaceID, 7001)
	ciSeedRepo(t, s, ctx, c.WorkspaceID, instID, 9201, "octo/e2e", true)
	post := func(delivery, body string) *httptest.ResponseRecorder {
		return ciPostWebhook(t, h, delivery, "workflow_job", body)
	}
	d1 := ciDeliveryID("d-e2e-1")
	dBad := ciDeliveryID("d-bad-json")
	if rec := post(d1, ciJobWebhookBody(7001, 9201, 5201, 6201, "queued", []string{"canter"}, nil)); rec.Code != http.StatusOK {
		t.Fatalf("queued: %d %s", rec.Code, rec.Body.String())
	}
	var createdAt1, jobID string
	var status1 string
	if err := s.pool.QueryRow(ctx, `SELECT id,status,created_at::text FROM ci_jobs WHERE github_job_id=5201`).Scan(&jobID, &status1, &createdAt1); err != nil {
		t.Fatal(err)
	}
	if status1 != "queued" {
		t.Fatalf("status after queued: %s", status1)
	}
	if rec := post(ciDeliveryID("d-e2e-2"), ciJobWebhookBody(7001, 9201, 5201, 6201, "in_progress", []string{"canter"}, nil)); rec.Code != http.StatusOK {
		t.Fatalf("running: %d", rec.Code)
	}
	if rec := post(ciDeliveryID("d-e2e-3"), ciJobWebhookBody(7001, 9201, 5201, 6201, "completed", []string{"canter"}, strPtr("success"))); rec.Code != http.StatusOK {
		t.Fatalf("completed: %d", rec.Code)
	}
	var status2, createdAt2 string
	if err := s.pool.QueryRow(ctx, `SELECT status,created_at::text FROM ci_jobs WHERE github_job_id=5201`).Scan(&status2, &createdAt2); err != nil {
		t.Fatal(err)
	}
	if status2 != "completed" || createdAt1 != createdAt2 {
		t.Fatalf("completion: status=%s created changed %q->%q", status2, createdAt1, createdAt2)
	}
	if rec := post(ciDeliveryID("d-e2e-4"), ciJobWebhookBody(7001, 9201, 5201, 6201, "queued", []string{"canter"}, nil)); rec.Code != http.StatusOK {
		t.Fatalf("late queued: %d", rec.Code)
	}
	if _, status := ciJobRowCount(t, s, ctx, 5201); status != "completed" {
		t.Fatalf("late queued rewound terminal job: %s", status)
	}
	bodyNC := ciJobWebhookBody(7001, 9201, 5202, 6202, "queued", []string{"ubuntu-latest"}, nil)
	if rec := post(ciDeliveryID("d-e2e-5"), bodyNC); rec.Code != http.StatusOK {
		t.Fatalf("non-canter: %d", rec.Code)
	}
	if count, _ := ciJobRowCount(t, s, ctx, 5202); count != 0 {
		t.Fatal("non-Canter job must be ignored")
	}
	bodyBad := ciJobWebhookBody(7001, 9201, 5203, 6203, "completed", []string{"canter-9vcpu"}, strPtr("success"))
	if rec := post(ciDeliveryID("d-e2e-6"), bodyBad); rec.Code != http.StatusOK {
		t.Fatalf("unsupported completed: %d", rec.Code)
	}
	var badStatus, badReason string
	if err := s.pool.QueryRow(ctx, `SELECT status,document->'job'->>'queueReason' FROM ci_jobs WHERE github_job_id=5203`).Scan(&badStatus, &badReason); err != nil {
		t.Fatal(err)
	}
	if badStatus != "completed" || !strings.Contains(badReason, "Unsupported") {
		t.Fatalf("unsupported completed must stay terminal with diagnostic: %s %q", badStatus, badReason)
	}
	bodyConflict := ciJobWebhookBody(7001, 9201, 5204, 6204, "queued", []string{"canter", "canter-2vcpu-ubuntu-2404"}, nil)
	if rec := post(ciDeliveryID("d-e2e-7"), bodyConflict); rec.Code != http.StatusOK {
		t.Fatalf("conflict: %d", rec.Code)
	}
	var conflictStatus string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM ci_jobs WHERE github_job_id=5204`).Scan(&conflictStatus); err != nil {
		t.Fatal(err)
	}
	if conflictStatus != "blocked" {
		t.Fatalf("conflicting labels must block: %s", conflictStatus)
	}
	var deliveries int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_webhook_deliveries WHERE delivery_id=$1`, d1).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 {
		t.Fatal("delivery must persist once")
	}
	rec := ciWebhookRequest(t, ciTestWebhookSecret, d1, "check_run", ciJobWebhookBody(7001, 9201, 5201, 6201, "queued", []string{"canter"}, nil), true)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, rec)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("changed-event replay: got %d want 409", rec2.Code)
	}
	rec = ciWebhookRequest(t, ciTestWebhookSecret, dBad, "workflow_job", `{"action":`, true)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, rec)
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("malformed payload: got %d want 400", rec3.Code)
	}
	var badDeliveries int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_webhook_deliveries WHERE delivery_id=$1`, dBad).Scan(&badDeliveries); err != nil {
		t.Fatal(err)
	}
	if badDeliveries != 0 {
		t.Fatal("malformed payload must not persist delivery")
	}
}

func TestCIHardeningWebhookConcurrentAndLifecycle(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	h := ciTestServer(s, CIConfig{WebhookSecret: []byte(ciTestWebhookSecret), APIBaseURL: "http://127.0.0.1:1"})
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	instID := ciSeedInstallation(t, s, ctx, c.WorkspaceID, 7001)
	ciSeedRepo(t, s, ctx, c.WorkspaceID, instID, 9301, "octo/race", true)
	var wg sync.WaitGroup
	for i, status := range []string{"queued", "completed"} {
		wg.Add(1)
		go func(i int, status string) {
			defer wg.Done()
			conclusion := ""
			var cp *string
			if status == "completed" {
				cp = &conclusion
				*cp = "success"
			}
			body := ciJobWebhookBody(7001, 9301, 5301, 6301, status, []string{"canter"}, cp)
			req := ciWebhookRequest(t, ciTestWebhookSecret, ciDeliveryID(fmt.Sprintf("d-race-%d", i)), "workflow_job", body, true)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("concurrent %s: %d", status, rec.Code)
			}
		}(i, status)
	}
	wg.Wait()
	count, status := ciJobRowCount(t, s, ctx, 5301)
	if count != 1 || status != "completed" {
		t.Fatalf("concurrent deliveries: count=%d status=%s", count, status)
	}
	instBody := `{"action":"deleted","installation":{"id":7001}}`
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-life-1"), "installation", instBody); rec.Code != http.StatusOK {
		t.Fatalf("delete installation: %d", rec.Code)
	}
	var instStatus string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM ci_github_installations WHERE id=$1`, instID).Scan(&instStatus); err != nil {
		t.Fatal(err)
	}
	if instStatus != "removed" {
		t.Fatalf("installation status: %s", instStatus)
	}
	reactivate := `{"action":"unsuspend","installation":{"id":7001}}`
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-life-2"), "installation", reactivate); rec.Code != http.StatusOK {
		t.Fatalf("stale unsuspend: %d", rec.Code)
	}
	if err := s.pool.QueryRow(ctx, `SELECT status FROM ci_github_installations WHERE id=$1`, instID).Scan(&instStatus); err != nil {
		t.Fatal(err)
	}
	if instStatus != "removed" {
		t.Fatal("removed installation must not reactivate via webhook")
	}
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-life-3"), "workflow_job", ciJobWebhookBody(7001, 9301, 5302, 6302, "queued", []string{"canter"}, nil)); rec.Code != http.StatusOK {
		t.Fatalf("job on removed installation: %d", rec.Code)
	}
	if count, _ := ciJobRowCount(t, s, ctx, 5302); count != 0 {
		t.Fatal("removed installation must not import jobs")
	}
}

func TestCIHardeningDisabledProductAndCancellation(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	mock := newCIMockGitHub(t)
	mock.repoInfo["octo/off"] = ciRepoInfo{ID: 9401, Private: boolPtr(false)}
	h := ciHardeningServer(s, ciHardeningConfig(t, mock), false)
	instID := ciSeedInstallation(t, s, ctx, c.WorkspaceID, 7001)
	ciSeedRepo(t, s, ctx, c.WorkspaceID, instID, 9401, "octo/off", false)
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-off-1"), "workflow_job", ciJobWebhookBody(7001, 9401, 5401, 6401, "queued", []string{"canter"}, nil)); rec.Code != http.StatusOK {
		t.Fatalf("disabled product webhook: %d", rec.Code)
	}
	if count, _ := ciJobRowCount(t, s, ctx, 5401); count != 0 {
		t.Fatal("disabled product must not import new jobs")
	}
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-off-2"), "workflow_job", ciJobWebhookBody(7001, 9401, 5401, 6401, "queued", []string{"canter"}, nil)); rec.Code != http.StatusOK {
		t.Fatalf("enabled webhook: %d", rec.Code)
	}
	if count, _ := ciJobRowCount(t, s, ctx, 5401); count != 1 {
		t.Fatal("enabled product must import job")
	}
	var jobID string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM ci_jobs WHERE github_job_id=5401`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	cancel := func(tok string) *httptest.ResponseRecorder {
		req := ciAuthedRequest(t, http.MethodPost, "/v1/workspaces/"+c.WorkspaceID+"/ci/jobs/"+jobID+"/cancel", tok, "")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	viewer, _, viewerToken, err := s.Signup(ctx, "viewer@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, viewer.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if rec := cancel(viewerToken); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer cancel: got %d want 403", rec.Code)
	}
	if rec := cancel(token); rec.Code != http.StatusConflict {
		t.Fatalf("owner cancel nonterminal: got %d want 409", rec.Code)
	}
	var status string
	var logs int
	if err = s.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM ci_job_logs WHERE job_id=$1) FROM ci_jobs WHERE id=$1`, jobID).Scan(&status, &logs); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || logs != 0 {
		t.Fatalf("nonterminal cancel must not mutate: status=%s logs=%d", status, logs)
	}
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-off-3"), "workflow_job", ciJobWebhookBody(7001, 9401, 5401, 6401, "completed", []string{"canter"}, strPtr("success"))); rec.Code != http.StatusOK {
		t.Fatalf("complete: %d", rec.Code)
	}
	rec := cancel(token)
	if rec.Code != http.StatusOK {
		t.Fatalf("terminal cancel idempotent: %d %s", rec.Code, rec.Body.String())
	}
	var audits int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action='ci.job.cancelled'`, c.WorkspaceID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 0 {
		t.Fatal("no false cancellation audit")
	}
}

func TestCIHardeningCascadeAndSettings(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	account, workspace, _, err := s.Signup(ctx, "cascade@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, accountID := workspace.ID, account.ID
	h := ciTestServer(s, CIConfig{WebhookSecret: []byte(ciTestWebhookSecret), APIBaseURL: "http://127.0.0.1:1"})
	ciEnableProduct(t, s, ctx, workspaceID, accountID)
	instID := ciSeedInstallation(t, s, ctx, workspaceID, 7001)
	ciSeedRepo(t, s, ctx, workspaceID, instID, 9501, "octo/cascade", true)
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-cas-1"), "workflow_job", ciJobWebhookBody(7001, 9501, 5501, 6501, "completed", []string{"canter"}, strPtr("success"))); rec.Code != http.StatusOK {
		t.Fatalf("seed job: %d", rec.Code)
	}
	var jobPK string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM ci_jobs WHERE github_job_id=5501`).Scan(&jobPK); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_job_logs(workspace_id,job_id,sequence,stream,content) VALUES($1,$2,0,'system','x')`, workspaceID, jobPK); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_job_metrics(workspace_id,job_id,sampled_at,document) VALUES($1,$2,now(),'{}')`, workspaceID, jobPK); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_job_tests(workspace_id,job_id,id,document) VALUES($1,$2,'t1','{}')`, workspaceID, jobPK); err != nil {
		t.Fatal(err)
	}
	raw := strings.Repeat("b", 64)
	sum := sha256.Sum256([]byte(raw))
	if err := s.CreateCIConnectState(ctx, workspaceID, accountID, hex.EncodeToString(sum[:]), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM memberships WHERE workspace_id=$1`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM audit_events WHERE workspace_id=$1`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM workspace_usage_caps WHERE workspace_id=$1`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM wren_ai_allowances WHERE workspace_id=$1`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, workspaceID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"ci_settings", "ci_github_installations", "ci_repositories", "ci_jobs", "ci_job_logs", "ci_job_metrics", "ci_job_tests", "ci_connect_states"} {
		var count int
		query := fmt.Sprintf(`SELECT count(*) FROM %s WHERE workspace_id=$1`, table)
		if err := s.pool.QueryRow(ctx, query, workspaceID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows survived workspace deletion: %d", table, count)
		}
	}
	settingsBody := `{"paidOverrunEnabled":false,"monthlySpendLimitCents":1000,"cacheEnabled":true,"retainFailedMinutes":0,"defaultSize":"linux-x64-4"} {"extra":true}`
	s3, c3, token3 := operatorFixture(t)
	h3 := ciTestServer(s3, CIConfig{})
	req := ciAuthedRequest(t, http.MethodPatch, "/v1/workspaces/"+c3.WorkspaceID+"/ci/settings", token3, settingsBody)
	rec := httptest.NewRecorder()
	h3.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON document: got %d want 400", rec.Code)
	}
	operator, _, operatorToken, err := s3.Signup(ctx, "op@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s3.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, operator.ID, c3.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	{
		req = ciAuthedRequest(t, http.MethodPatch, "/v1/workspaces/"+c3.WorkspaceID+"/ci/settings", operatorToken, `{"paidOverrunEnabled":false,"monthlySpendLimitCents":1000,"cacheEnabled":true,"retainFailedMinutes":0,"defaultSize":"linux-x64-4"}`)
		rec = httptest.NewRecorder()
		h3.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("operator settings: got %d want 403", rec.Code)
		}
	}
}

func TestCIHardeningConfig(t *testing.T) {
	s, c, token := operatorFixture(t)
	key := ciTestKey(t)
	shortKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	shortDER, _ := x509.MarshalPKCS8PrivateKey(shortKey)
	shortPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: shortDER})
	valid := CIConfig{AppID: 1, AppSlug: "canter-ci", PrivateKeyPEM: key, WebhookSecret: []byte("0123456789abcdef0123456789abcdef"), APIBaseURL: "https://api.github.com"}
	if err := ValidateCIConfig(CIConfig{}); err != nil {
		t.Fatalf("zero config must be valid: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*CIConfig)
		ok   bool
	}{
		{"valid", func(c *CIConfig) {}, true},
		{"missing app id", func(c *CIConfig) { c.AppID = 0 }, false},
		{"missing slug", func(c *CIConfig) { c.AppSlug = "" }, false},
		{"missing key", func(c *CIConfig) { c.PrivateKeyPEM = nil }, false},
		{"missing secret", func(c *CIConfig) { c.WebhookSecret = nil }, false},
		{"bad slug", func(c *CIConfig) { c.AppSlug = "Bad Slug!" }, false},
		{"short secret", func(c *CIConfig) { c.WebhookSecret = []byte("short") }, false},
		{"1024-bit key", func(c *CIConfig) { c.PrivateKeyPEM = shortPEM }, false},
		{"garbage key", func(c *CIConfig) { c.PrivateKeyPEM = []byte("not a key") }, false},
	} {
		cfg := valid
		tc.mut(&cfg)
		err := ValidateCIConfig(cfg)
		if tc.ok && err != nil {
			t.Fatalf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		srv := ciHardeningServer(s, CIConfig{}, false)
		srv.config.CI = cfg
		if got := srv.ciConfigured(); got != tc.ok {
			t.Fatalf("%s: ciConfigured=%v want %v", tc.name, got, tc.ok)
		}
	}
	h := ciHardeningServer(s, CIConfig{}, false)
	ciEnableProduct(t, s, context.Background(), c.WorkspaceID, c.AccountID)
	req := ciAuthedRequest(t, http.MethodGet, "/v1/workspaces/"+c.WorkspaceID+"/ci", token, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"github-app","name":"GitHub App","status":"not_configured"`) {
		t.Fatalf("unconfigured overview must mark github-app not_configured: %s", body)
	}
	if !strings.Contains(body, `"id":"job-ingest","name":"Job ingest","status":"not_configured"`) {
		t.Fatalf("unconfigured overview must mark job-ingest not_configured")
	}
	mock := newCIMockGitHub(t)
	h2 := ciHardeningServer(s, ciHardeningConfig(t, mock), true)
	req = ciAuthedRequest(t, http.MethodGet, "/v1/workspaces/"+c.WorkspaceID+"/ci", token, "")
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("configured overview: %d", rec.Code)
	}
	body = rec.Body.String()
	for _, feature := range []string{`"id":"github-app","name":"GitHub App","status":"available"`, `"id":"job-ingest","name":"Job ingest","status":"available"`} {
		if !strings.Contains(body, feature) {
			t.Fatalf("configured overview must mark feature available %s: %s", feature, body)
		}
	}
}

func TestCIHardeningSettingsDemotionDuringBody(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	h := ciTestServer(s, CIConfig{})
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	pr, pw := io.Pipe()
	req := ciAuthedRequest(t, http.MethodPatch, "/v1/workspaces/"+c.WorkspaceID+"/ci/settings", token, "")
	req.Body = pr
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, req)
	}()
	if _, err := pw.Write([]byte(`{"paidOverrunEnabled":false,"monthlySpendLimitCents":1000,"cacheEnabled":true,"retainFailedMinutes":0,"defaultSize":"linux-x64-4"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE memberships SET role='operator' WHERE workspace_id=$1 AND account_id=$2`, c.WorkspaceID, c.AccountID); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	<-done
	if rec.Code != http.StatusForbidden {
		t.Fatalf("demoted owner settings: got %d want 403: %s", rec.Code, rec.Body.String())
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_settings WHERE workspace_id=$1`, c.WorkspaceID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("demoted owner must not persist settings")
	}
}

func TestCIHardeningWebhookMalformedAllEvents(t *testing.T) {
	s, c, _ := operatorFixture(t)
	h := ciTestServer(s, CIConfig{WebhookSecret: []byte(ciTestWebhookSecret), APIBaseURL: "http://127.0.0.1:1"})
	ciEnableProduct(t, s, context.Background(), c.WorkspaceID, c.AccountID)
	for _, event := range []string{"ping", "installation", "workflow_job", "check_run", "totally-unknown"} {
		delivery := ciDeliveryID("d-malformed-" + event)
		req := ciWebhookRequest(t, ciTestWebhookSecret, delivery, event, `{"action":`, true)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s malformed: got %d want 400", event, rec.Code)
		}
		var count int
		if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM ci_webhook_deliveries WHERE delivery_id=$1`, delivery).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s malformed must not persist delivery", event)
		}
	}
}

func TestCIHardeningCallbackInstallIDMismatch(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	mock := newCIMockGitHub(t)
	mock.installationID = 7999
	mock.repos = []ciGitHubRepo{{ID: 8601, FullName: "octo/z", Private: false}}
	h := ciHardeningServer(s, ciHardeningConfig(t, mock), true)
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	ciSaveGitHub(t, h, ctx, c.AccountID, c.WorkspaceID)
	state := ciConnectState(t, h, token, c.WorkspaceID)
	rec := ciCallback(t, h, 7001, state)
	if rec.Code == http.StatusSeeOther {
		t.Fatal("installation id mismatch must not bind")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_github_installations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("mismatched installation must not bind")
	}
	sum := sha256.Sum256([]byte(state))
	if _, _, _, err := s.GetCIConnectState(ctx, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("failed proof must leave state retryable: %v", err)
	}
}

func TestCIHardeningBindScopeRace(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `CREATE OR REPLACE FUNCTION ci_test_bind_gate() RETURNS trigger AS $$ BEGIN PERFORM pg_advisory_xact_lock(760001); RETURN NEW; END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE TRIGGER ci_test_bind_gate BEFORE INSERT ON ci_github_installations FOR EACH ROW EXECUTE FUNCTION ci_test_bind_gate()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS ci_test_bind_gate ON ci_github_installations`)
		_, _ = s.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS ci_test_bind_gate()`)
	})
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock(760001)`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(760001)`)
	setup := func(email string) (string, string, string, string) {
		account, workspace, _, err := s.Signup(ctx, email, "correct horse battery staple", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.EnableWorkspaceProduct(ctx, workspace.ID, "ci", sdk.ActorRef{Kind: "human", ID: account.ID}); err != nil {
			t.Fatal(err)
		}
		raw := make([]byte, 32)
		if _, err = rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		hash := hex.EncodeToString(func() []byte { sum := sha256.Sum256(raw); return sum[:] }())
		if err = s.CreateCIConnectState(ctx, workspace.ID, account.ID, hash, time.Now().Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		return workspace.ID, account.ID, hex.EncodeToString(raw), hash
	}
	ws1, acct1, state1, hash1 := setup("race-a@example.com")
	ws2, acct2, state2, hash2 := setup("race-b@example.com")
	_ = state1
	_ = state2
	repos := []ciGitHubRepo{{ID: 8701, FullName: "octo/race", Private: false}}
	type result struct {
		workspaceID string
		hash        string
		id          string
		err         error
	}
	results := make(chan result, 2)
	for _, args := range [][4]string{{ws1, acct1, hash1, ws1}, {ws2, acct2, hash2, ws2}} {
		go func(workspaceID, accountID, hash string) {
			id, err := s.BindCIInstallation(ctx, workspaceID, accountID, hash, 7701, 4242, "octo", "User", repos)
			results <- result{workspaceID, hash, id, err}
		}(args[0], args[1], args[2])
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND classid=0 AND objid=760001 AND NOT granted`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bind race waiters never queued: %d", waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_unlock(760001)`); err != nil {
		t.Fatal(err)
	}
	var outcomes []result
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			outcomes = append(outcomes, r)
		case <-time.After(10 * time.Second):
			t.Fatal("bind did not finish")
		}
	}
	var winner, loser *result
	for i := range outcomes {
		r := outcomes[i]
		if r.err == nil {
			winner = &outcomes[i]
		} else if errors.Is(r.err, errCIInstallationBound) {
			loser = &outcomes[i]
		}
	}
	if winner == nil || loser == nil {
		t.Fatalf("race outcomes: %+v", outcomes)
	}
	var boundWorkspace string
	if err = s.pool.QueryRow(ctx, `SELECT workspace_id FROM ci_github_installations WHERE github_installation_id=7701`).Scan(&boundWorkspace); err != nil {
		t.Fatal(err)
	}
	if boundWorkspace != winner.workspaceID {
		t.Fatalf("installation bound to %q, winner was %q", boundWorkspace, winner.workspaceID)
	}
	var repoWorkspace string
	if err = s.pool.QueryRow(ctx, `SELECT workspace_id FROM ci_repositories`).Scan(&repoWorkspace); err != nil {
		t.Fatal(err)
	}
	if repoWorkspace != winner.workspaceID {
		t.Fatalf("repos written to %q, winner was %q", repoWorkspace, winner.workspaceID)
	}
	for _, check := range []struct {
		hash, workspace string
		want            int
	}{
		{winner.hash, winner.workspaceID, 0},
		{loser.hash, loser.workspaceID, 1},
	} {
		var remaining int
		if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_connect_states WHERE state_hash=$1 AND workspace_id=$2`, check.hash, check.workspace).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != check.want {
			t.Fatalf("state %s in %s: got %d want %d", check.hash[:8], check.workspace, remaining, check.want)
		}
	}
}

func TestCIHardeningStateExpiryDuringLockWait(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	if err := s.EnableWorkspaceProduct(ctx, c.WorkspaceID, "ci", sdk.ActorRef{Kind: "human", ID: c.AccountID}); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	if err := s.CreateCIConnectState(ctx, c.WorkspaceID, c.AccountID, hash, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	lockTx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(ctx)
	var locked string
	if err = lockTx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, c.AccountID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	var controlPID int32
	if err = conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&controlPID); err != nil {
		t.Fatal(err)
	}
	type bindResult struct {
		id  string
		err error
	}
	done := make(chan bindResult, 1)
	go func() {
		id, err := s.BindCIInstallation(ctx, c.WorkspaceID, c.AccountID, hash, 7801, 4242, "octo", "User", []ciGitHubRepo{{ID: 8801, FullName: "octo/e", Private: false}})
		done <- bindResult{id, err}
	}()
	var waitingPID int32
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = s.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND pid<>$1 AND wait_event_type='Lock' AND position('accounts' in query)>0 ORDER BY pid LIMIT 1`, controlPID).Scan(&waitingPID)
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("bind never blocked on account lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		var blockers []int32
		if err = s.pool.QueryRow(ctx, `SELECT pg_blocking_pids($1)`, waitingPID).Scan(&blockers); err != nil {
			t.Fatal(err)
		}
		blocked := false
		for _, pid := range blockers {
			if pid == controlPID {
				blocked = true
			}
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting pid %d not blocked by control pid %d", waitingPID, controlPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE ci_connect_states SET expires_at=clock_timestamp()+interval '100 milliseconds' WHERE state_hash=$1`, hash); err != nil {
		t.Fatal(err)
	}
	for {
		var expired bool
		if err = s.pool.QueryRow(ctx, `SELECT clock_timestamp()>(SELECT expires_at FROM ci_connect_states WHERE state_hash=$1)`, hash).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state never expired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if !errors.Is(r.err, errCIConnectState) {
			t.Fatalf("expired-during-lock state: got %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bind did not finish")
	}
	var remaining int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_connect_states WHERE state_hash=$1`, hash).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatal("expired state must remain unconsumed")
	}
	var installations int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_github_installations WHERE github_installation_id=7801`).Scan(&installations); err != nil {
		t.Fatal(err)
	}
	if installations != 0 {
		t.Fatal("expired state must not bind")
	}
}

func TestCIHardeningVisibilityEdgeCases(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	mock := newCIMockGitHub(t)
	h := ciHardeningServer(s, ciHardeningConfig(t, mock), true)
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	instID := ciSeedInstallation(t, s, ctx, c.WorkspaceID, 7001)
	ciSeedRepo(t, s, ctx, c.WorkspaceID, instID, 8901, "octo/edge", true)
	mock.repoInfo["octo/edge"] = ciRepoInfo{ID: 8901, Private: boolPtr(true)}
	body := ciJobWebhookBody(7001, 8901, 5901, 6901, "completed", []string{"canter"}, strPtr("success"))
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-edge"), "workflow_job", body); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d", rec.Code)
	}
	ciSaveGitHub(t, h, ctx, c.AccountID, c.WorkspaceID)
	var jobPK string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM ci_jobs WHERE github_job_id=5901`).Scan(&jobPK); err != nil {
		t.Fatal(err)
	}
	get := func(tok, path string) *httptest.ResponseRecorder {
		req := ciAuthedRequest(t, http.MethodGet, path, tok, "")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	detail := "/v1/workspaces/" + c.WorkspaceID + "/ci/jobs/" + jobPK
	listPath := "/v1/workspaces/" + c.WorkspaceID + "/ci"
	mock.mu.Lock()
	mock.userDenied["octo/edge"] = true
	mock.mu.Unlock()
	rec := get(token, listPath)
	if strings.Contains(rec.Body.String(), "octo/edge") {
		t.Fatal("user token without repo grant must not see private job")
	}
	if auth := mock.repoBearer["octo/edge"]; auth != "Bearer user-tok" {
		t.Fatalf("visibility must use viewer's own token, got %q", auth)
	}
	mock.mu.Lock()
	mock.userDenied["octo/edge"] = false
	mock.mu.Unlock()
	rec = get(token, listPath)
	if !strings.Contains(rec.Body.String(), "octo/edge") {
		t.Fatal("granted user token must see private job")
	}
	other, _, otherToken, err := s.Signup(ctx, "edge-member@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, other.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	mock.mu.Lock()
	mock.repoRaw["octo/edge"] = `{"id":8901}`
	mock.mu.Unlock()
	rec = get(otherToken, listPath)
	if strings.Contains(rec.Body.String(), "octo/edge") {
		t.Fatal("missing private field must fail closed for anonymous viewers")
	}
	mock.mu.Lock()
	mock.repoRaw["octo/edge"] = `{"id":8901,"private":null}`
	mock.mu.Unlock()
	rec = get(otherToken, listPath)
	if strings.Contains(rec.Body.String(), "octo/edge") {
		t.Fatal("explicit null private field must fail closed for anonymous viewers")
	}
	mock.mu.Lock()
	delete(mock.repoRaw, "octo/edge")
	mock.repoInfo["octo/edge"] = ciRepoInfo{ID: 8901, Private: boolPtr(false)}
	mock.mu.Unlock()
	rec = get(otherToken, listPath)
	if !strings.Contains(rec.Body.String(), "octo/edge") {
		t.Fatal("genuine public repo must be visible anonymously")
	}
	mock.mu.Lock()
	mock.repoInfo["octo/edge"] = ciRepoInfo{ID: 8901, Private: boolPtr(true)}
	mock.tokenGrants["octo/edge"] = map[string]bool{"user-tok": true, "viewer-denied": false}
	mock.mu.Unlock()
	if err := h.saveGitHubConnection(ctx, other.ID, c.WorkspaceID, "456", "viewer", "repo", &oauth2.Token{AccessToken: "viewer-denied", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	rec = get(otherToken, detail)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("member token without repo grant: got %d want 404: %s", rec.Code, rec.Body.String())
	}
	if auth := mock.repoBearer["octo/edge"]; auth != "Bearer viewer-denied" {
		t.Fatalf("member visibility must use member's own token, got %q", auth)
	}
	rec = get(token, detail)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner detail with grant: got %d want 200", rec.Code)
	}
	if auth := mock.repoBearer["octo/edge"]; auth != "Bearer user-tok" {
		t.Fatalf("owner visibility must use owner token, got %q", auth)
	}
	if err := h.saveGitHubConnection(ctx, other.ID, c.WorkspaceID, "456", "viewer", "repo", &oauth2.Token{AccessToken: "viewer-allowed", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	mock.mu.Lock()
	mock.tokenGrants["octo/edge"]["viewer-allowed"] = true
	mock.mu.Unlock()
	rec = get(otherToken, detail)
	if rec.Code != http.StatusOK {
		t.Fatalf("member token with repo grant: got %d want 200: %s", rec.Code, rec.Body.String())
	}
	synthetic := Principal{Account: &Account{ID: c.AccountID}, Session: &AgentSession{ID: "synthetic-session"}, WorkspaceID: c.WorkspaceID}
	visible, err := h.ciCheckRepoAccess(ctx, synthetic, c.WorkspaceID, 8901, "octo/edge")
	if err != nil || visible {
		t.Fatalf("account+session principal must not inherit owner token: visible=%v err=%v", visible, err)
	}
	if auth := mock.repoBearer["octo/edge"]; auth != "" {
		t.Fatalf("session principal must check anonymously, got auth %q", auth)
	}
}

func TestCIHardeningJobDocumentIntegrity(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	h := ciTestServer(s, CIConfig{WebhookSecret: []byte(ciTestWebhookSecret), APIBaseURL: "http://127.0.0.1:1"})
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	instID := ciSeedInstallation(t, s, ctx, c.WorkspaceID, 7001)
	ciSeedRepo(t, s, ctx, c.WorkspaceID, instID, 9001, "octo/doc", true)
	post := func(delivery, body string) {
		t.Helper()
		if rec := ciPostWebhook(t, h, delivery, "workflow_job", body); rec.Code != http.StatusOK {
			t.Fatalf("webhook %s: %d %s", delivery, rec.Code, rec.Body.String())
		}
	}
	post(ciDeliveryID("d-doc-1"), ciJobWebhookBody(7001, 9001, 5901, 6901, "queued", []string{"canter-9vcpu"}, nil))
	var jobPK, createdDoc string
	if err := s.pool.QueryRow(ctx, `SELECT id,document->'job'->>'createdAt' FROM ci_jobs WHERE github_job_id=5901`).Scan(&jobPK, &createdDoc); err != nil {
		t.Fatal(err)
	}
	if createdDoc == "" {
		t.Fatal("document must carry createdAt")
	}
	post(ciDeliveryID("d-doc-2"), ciJobWebhookBody(7001, 9001, 5901, 6901, "in_progress", []string{"canter-9vcpu"}, nil))
	corrupt := `{"job":{"id":"cijob_fake","repository":"evil/repo","name":"build","workflowName":"ci","ref":"main","headSha":"abc123def","githubJobId":999999,"githubRunId":999999,"githubURL":"https://evil.example/x","status":"queued","size":"linux-x64-4","createdAt":"2020-01-01T00:00:00Z","elapsedSeconds":0,"billableSeconds":0,"costMicros":0,"queueReason":""},"steps":[]}`
	if _, err := s.pool.Exec(ctx, `UPDATE ci_jobs SET document=$1 WHERE github_job_id=5901`, corrupt); err != nil {
		t.Fatal(err)
	}
	row, _, err := s.CIJobRow(ctx, c.WorkspaceID, jobPK)
	if err != nil {
		t.Fatal(err)
	}
	if row.Job.ID != jobPK || row.Job.GitHubJobID != 5901 || row.Job.GitHubRunID != 6901 {
		t.Fatalf("document ids must be normalized from SQL: %+v", row.Job)
	}
	if row.Job.Status != "running" || row.Job.Repository != "octo/doc" {
		t.Fatalf("document status/repo must be normalized from SQL: %+v", row.Job)
	}
	if row.Job.GitHubURL != "https://github.com/octo/doc/actions/runs/6901/job/5901" {
		t.Fatalf("document URL must be rebuilt canonically, got %q", row.Job.GitHubURL)
	}
	post(ciDeliveryID("d-doc-3"), ciJobWebhookBody(7001, 9001, 5901, 6901, "queued", []string{"canter-9vcpu"}, nil))
	post(ciDeliveryID("d-doc-4"), ciJobWebhookBody(7001, 9001, 5901, 6901, "queued", []string{"canter"}, nil))
	var status, docID, docCreated string
	if err := s.pool.QueryRow(ctx, `SELECT status,document->'job'->>'id',document->'job'->>'createdAt' FROM ci_jobs WHERE github_job_id=5901`).Scan(&status, &docID, &docCreated); err != nil {
		t.Fatal(err)
	}
	if status != "running" || docID != "cijob_fake" || docCreated != "2020-01-01T00:00:00Z" {
		t.Fatalf("stale delivery rewound/renamed job: status=%s id=%q created=%q", status, docID, docCreated)
	}
	post(ciDeliveryID("d-doc-5"), ciJobWebhookBody(7001, 9001, 5901, 6901, "completed", []string{"canter"}, strPtr("success")))
	if err := s.pool.QueryRow(ctx, `SELECT document->'job'->>'id' FROM ci_jobs WHERE github_job_id=5901`).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	if docID != jobPK {
		t.Fatalf("accepted update must heal document id to primary key, got %q", docID)
	}
	post(ciDeliveryID("d-doc-6"), ciJobWebhookBody(7001, 9001, 5903, 6903, "queued", []string{"canter"}, nil))
	var provPK string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM ci_jobs WHERE github_job_id=5903`).Scan(&provPK); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_jobs SET status='provisioning',document=$2 WHERE id=$1`, provPK, corrupt); err != nil {
		t.Fatal(err)
	}
	post(ciDeliveryID("d-doc-7"), ciJobWebhookBody(7001, 9001, 5903, 6903, "queued", []string{"canter"}, nil))
	post(ciDeliveryID("d-doc-8"), ciJobWebhookBody(7001, 9001, 5903, 6903, "queued", []string{"canter-9vcpu"}, nil))
	var provStatus string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM ci_jobs WHERE id=$1`, provPK).Scan(&provStatus); err != nil {
		t.Fatal(err)
	}
	if provStatus != "provisioning" {
		t.Fatalf("stale queued/blocked delivery rewound provisioning job: %s", provStatus)
	}
	post(ciDeliveryID("d-doc-9"), ciJobWebhookBody(7001, 9001, 5902, 6902, "queued", []string{"canter"}, nil))
	post(ciDeliveryID("d-doc-10"), ciJobWebhookBody(7001, 9001, 5902, 6902, "in_progress", []string{"canter"}, nil))
	post(ciDeliveryID("d-doc-11"), ciJobWebhookBody(7001, 9001, 5902, 6902, "completed", []string{"canter"}, strPtr("success")))
	post(ciDeliveryID("d-doc-12"), ciJobWebhookBody(7001, 9001, 5902, 6902, "queued", []string{"canter-9vcpu"}, nil))
	var fstatus string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM ci_jobs WHERE github_job_id=5902`).Scan(&fstatus); err != nil {
		t.Fatal(err)
	}
	if fstatus != "completed" {
		t.Fatalf("late blocked/queued delivery rewound completed job: %s", fstatus)
	}
}

func TestCIHardeningRepositoryRenameAndURL(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	mock := newCIMockGitHub(t)
	h := ciHardeningServer(s, ciHardeningConfig(t, mock), true)
	ciEnableProduct(t, s, ctx, c.WorkspaceID, c.AccountID)
	ciSaveGitHub(t, h, ctx, c.AccountID, c.WorkspaceID)
	bind := func(repos []ciGitHubRepo, repoInfo map[string]ciRepoInfo) {
		t.Helper()
		mock.mu.Lock()
		mock.repos = repos
		mock.repoInfo = repoInfo
		mock.mu.Unlock()
		state := ciConnectState(t, h, token, c.WorkspaceID)
		if rec := ciCallback(t, h, 7001, state); rec.Code != http.StatusSeeOther {
			t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
		}
	}
	bind([]ciGitHubRepo{{ID: 9101, FullName: "octo/oldname", Private: true}}, map[string]ciRepoInfo{
		"octo/oldname": {ID: 9101, Private: boolPtr(true)},
	})
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-ren-1"), "workflow_job", ciJobWebhookBody(7001, 9101, 5911, 6911, "completed", []string{"canter"}, strPtr("success"))); rec.Code != http.StatusOK {
		t.Fatalf("seed job: %d", rec.Code)
	}
	var instID string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM ci_github_installations WHERE github_installation_id=7001`).Scan(&instID); err != nil {
		t.Fatal(err)
	}
	get := func() string {
		req := ciAuthedRequest(t, http.MethodGet, "/v1/workspaces/"+c.WorkspaceID+"/ci", token, "")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if body := get(); !strings.Contains(body, "https://github.com/octo/oldname/actions/runs/6911/job/5911") {
		t.Fatalf("job URL must be rebuilt from trusted fields: %s", body)
	}
	bind([]ciGitHubRepo{{ID: 9101, FullName: "octo/newname", Private: true}}, map[string]ciRepoInfo{
		"octo/newname": {ID: 9101, Private: boolPtr(true)},
	})
	var renamed string
	if err := s.pool.QueryRow(ctx, `SELECT full_name FROM ci_repositories WHERE installation_id=$1 AND github_repository_id=9101`, instID).Scan(&renamed); err != nil {
		t.Fatal(err)
	}
	if renamed != "octo/newname" {
		t.Fatalf("reconnect must refresh repo name, got %q", renamed)
	}
	if body := get(); !strings.Contains(body, "https://github.com/octo/newname/actions/runs/6911/job/5911") {
		t.Fatalf("renamed repo must show rebuilt URL under new name: %s", body)
	}
	if strings.Contains(get(), "octo/oldname/actions/runs") {
		t.Fatal("renamed repo must not leak stale URL under old name")
	}
	bind([]ciGitHubRepo{{ID: 9200, FullName: "octo/newname", Private: true}}, map[string]ciRepoInfo{
		"octo/newname": {ID: 9200, Private: boolPtr(true)},
	})
	var oldEnabled bool
	var oldJobs int
	if err := s.pool.QueryRow(ctx, `SELECT enabled FROM ci_repositories WHERE installation_id=$1 AND github_repository_id=9101`, instID).Scan(&oldEnabled); err != nil {
		t.Fatal(err)
	}
	if oldEnabled {
		t.Fatal("deselected old repo id must be disabled after name reuse")
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_jobs WHERE installation_id=$1 AND github_repository_id=9101`, instID).Scan(&oldJobs); err != nil {
		t.Fatal(err)
	}
	if oldJobs != 1 {
		t.Fatal("old repo id job history must remain attached to 9101")
	}
	if rec := ciPostWebhook(t, h, ciDeliveryID("d-ren-2"), "workflow_job", ciJobWebhookBody(7001, 9200, 5921, 6921, "completed", []string{"canter"}, strPtr("success"))); rec.Code != http.StatusOK {
		t.Fatalf("reuse-name job: %d", rec.Code)
	}
	body := get()
	if strings.Contains(body, `"githubJobId":5911`) {
		t.Fatal("old repo id job must stay hidden after name reuse")
	}
	if !strings.Contains(body, `"githubJobId":5921`) {
		t.Fatal("new repo id job must be visible under reused name")
	}
	bind([]ciGitHubRepo{{ID: 9200, FullName: "octo/other", Private: true}, {ID: 9300, FullName: "octo/newname", Private: true}}, map[string]ciRepoInfo{
		"octo/other":   {ID: 9200, Private: boolPtr(true)},
		"octo/newname": {ID: 9300, Private: boolPtr(true)},
	})
	rows, err := s.pool.Query(ctx, `SELECT github_repository_id,full_name,enabled FROM ci_repositories WHERE installation_id=$1 ORDER BY github_repository_id`, instID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64][2]any{}
	for rows.Next() {
		var id int64
		var name string
		var enabled bool
		if err = rows.Scan(&id, &name, &enabled); err != nil {
			t.Fatal(err)
		}
		got[id] = [2]any{name, enabled}
	}
	rows.Close()
	if len(got) != 3 {
		t.Fatalf("expected 3 repo rows after swap, got %d", len(got))
	}
	if got[9101][0] != "octo/newname" || got[9101][1] != false {
		t.Fatalf("9101 must stay disabled with stale name: %v", got[9101])
	}
	if got[9200][0] != "octo/other" || got[9200][1] != true {
		t.Fatalf("9200 must hold swapped name octo/other: %v", got[9200])
	}
	if got[9300][0] != "octo/newname" || got[9300][1] != true {
		t.Fatalf("9300 must hold swapped name octo/newname: %v", got[9300])
	}
	body = get()
	if !strings.Contains(body, "https://github.com/octo/other/actions/runs/6921/job/5921") {
		t.Fatalf("swapped repo job must rebuild URL under new name: %s", body)
	}
	if strings.Contains(body, `"githubJobId":5911`) {
		t.Fatal("old repo id job must remain hidden after swap")
	}
	var nameIdx bool
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*)=1 FROM pg_indexes WHERE schemaname=current_schema() AND indexname='ci_repositories_name' AND indexdef NOT LIKE '%UNIQUE%'`).Scan(&nameIdx); err != nil {
		t.Fatal(err)
	}
	if !nameIdx {
		t.Fatal("077 must leave non-unique repo name index")
	}
}

func strPtr(v string) *string { return &v }

func boolPtr(v bool) *bool { return &v }
