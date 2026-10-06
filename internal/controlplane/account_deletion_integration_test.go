package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/pquerna/otp/totp"
)

func deletionAccount(t *testing.T, h *HTTPServer, email string) (*http.Cookie, Principal) {
	t.Helper()
	w := signupHTTP(t, h, email)
	requireStatus(t, w, http.StatusCreated)
	cookie := authCookie(t, h, w, "session")
	p, err := h.service.Store.ResolveHuman(context.Background(), cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	var result struct{ Workspace Workspace }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	p.WorkspaceID = result.Workspace.ID
	return cookie, p
}

func startDeletion(t *testing.T, h *HTTPServer, cookie *http.Cookie) *http.Cookie {
	t.Helper()
	w := authRequest(t, h, "account/delete/start", map[string]string{"password": "correct horse battery staple"}, cookie)
	requireStatus(t, w, http.StatusAccepted)
	return authCookie(t, h, w, "deletion")
}

func TestAccountDeletionRequiresPasswordAndSessionBoundEmail(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	cookie, p := deletionAccount(t, h, "delete@example.com")
	other, _ := deletionAccount(t, h, "other@example.com")
	if _, err := s.CreateConversation(ctx, p.WorkspaceID, p.Account.ID, "conv_delete", "Private conversation"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO operator_runs(id,conversation_id,request_id,model) VALUES('run_delete','conv_delete','request','model'); INSERT INTO operator_messages(id,conversation_id,run_id,role,content,attachments) VALUES('msg_delete','conv_delete','run_delete','user','private content','[{"name":"private.txt","text":"private attachment"}]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO oauth_identities(provider,subject,account_id,email,created_at) VALUES('github','delete-subject',$1,$2,now())`, p.Account.ID, p.Account.Email); err != nil {
		t.Fatal(err)
	}
	w := authRequest(t, h, "signin", authInput{Email: p.Account.Email, Password: "correct horse battery staple"})
	requireStatus(t, w, http.StatusOK)
	second := authCookie(t, h, w, "session")
	w = authRequest(t, h, "account/delete/start", map[string]string{"password": "wrong"}, cookie)
	requireStatus(t, w, http.StatusUnauthorized)
	requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": "123456"}, cookie), http.StatusGone)
	proof := startDeletion(t, h, cookie)
	code := lastEmailCode(t, h)
	// Neither another account nor another session of this account can use it.
	requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, other, proof), http.StatusGone)
	requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, second, proof), http.StatusGone)
	requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": "invalid"}, cookie, proof), http.StatusBadRequest)
	if _, err := s.ResolveHuman(ctx, cookie.Value); err != nil {
		t.Fatal("a failed proof deleted the account")
	}
	w = authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, cookie, proof)
	requireStatus(t, w, http.StatusOK)
	for _, table := range []string{"accounts", "human_sessions", "oauth_identities", "operator_conversations", "operator_runs", "operator_messages", "account_security_events", "auth_email_outbox"} {
		var count int
		// The other disposable account and its mail are expected to remain.
		where := ""
		if table == "accounts" {
			where = " WHERE id='" + p.Account.ID + "'"
		}
		if table == "human_sessions" || table == "account_security_events" || table == "oauth_identities" || table == "auth_email_outbox" {
			where = " WHERE account_id='" + p.Account.ID + "'"
		}
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+where).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s retained deleted account data: count=%d err=%v", table, count, err)
		}
	}
	var retainedMail int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM auth_email_outbox WHERE recipient_hash=$1`, secretHash(p.Account.Email)).Scan(&retainedMail); err != nil || retainedMail != 0 {
		t.Fatal("signup/deletion email data survived deletion")
	}
	for _, session := range []*http.Cookie{cookie, second} {
		if _, err := s.ResolveHuman(ctx, session.Value); !errors.Is(err, ErrUnauthorized) {
			t.Fatal("a deleted account's session survived")
		}
	}
	if _, err := s.ResolveHuman(ctx, other.Value); err != nil {
		t.Fatal("deletion affected another account")
	}
	requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, cookie, proof), http.StatusUnauthorized)
}

func TestAccountDeletionMFAHasNoEmailBypass(t *testing.T) {
	for _, factor := range []string{"authenticator", "recovery"} {
		t.Run(factor, func(t *testing.T) {
			s, h, _ := newAuthTestServer(t)
			cookie, p := deletionAccount(t, h, "mfa-delete@example.com")
			emailCode := lastEmailCode(t, h)
			secret, recovery := enrollTOTP(t, h, cookie)
			now := s.now().Add(31 * time.Second)
			s.now = func() time.Time { return now }
			proof := startDeletion(t, h, cookie)
			var payload string
			if err := s.pool.QueryRow(context.Background(), `SELECT payload->>'factor' FROM auth_challenges WHERE token_hash=$1`, secretHash(proof.Value)).Scan(&payload); err != nil || payload != "mfa" {
				t.Fatal("MFA was bypassed")
			}
			requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": emailCode}, cookie, proof), http.StatusBadRequest)
			code := recovery[0]
			if factor == "authenticator" {
				var err error
				code, err = totp.GenerateCode(secret, now)
				if err != nil {
					t.Fatal(err)
				}
			}
			requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, cookie, proof), http.StatusOK)
			var count int
			if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM accounts WHERE id=$1`, p.Account.ID).Scan(&count); err != nil || count != 0 {
				t.Fatal("MFA deletion failed")
			}
		})
	}
}

func TestAccountDeletionProofFences(t *testing.T) {
	for _, scenario := range []string{"attempts", "expiry", "cancel", "credential-change", "cross-origin", "concurrent"} {
		t.Run(scenario, func(t *testing.T) {
			s, h, _ := newAuthTestServer(t)
			cookie, p := deletionAccount(t, h, "fenced-delete@example.com")
			proof := startDeletion(t, h, cookie)
			code := lastEmailCode(t, h)
			switch scenario {
			case "attempts":
				for i := 0; i < 5; i++ {
					requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": "invalid"}, cookie, proof), http.StatusBadRequest)
				}
			case "expiry":
				now := s.now().Add(11 * time.Minute)
				s.now = func() time.Time { return now }
			case "cancel":
				requireStatus(t, authMethod(t, h, http.MethodDelete, "account/delete", nil, cookie, proof), http.StatusNoContent)
			case "credential-change":
				if _, err := s.pool.Exec(context.Background(), `UPDATE accounts SET auth_version=auth_version+1 WHERE id=$1`, p.Account.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.pool.Exec(context.Background(), `UPDATE human_sessions SET auth_version=auth_version+1 WHERE account_id=$1`, p.Account.ID); err != nil {
					t.Fatal(err)
				}
			case "cross-origin":
				for _, origin := range []string{"", "https://attacker.test"} {
					r := httptest.NewRequest(http.MethodPost, "/v1/auth/account/delete/finish", strings.NewReader(`{"code":"`+code+`"}`))
					r.Header.Set("Origin", origin)
					r.Header.Set("Content-Type", "application/json")
					r.AddCookie(cookie)
					r.AddCookie(proof)
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					requireStatus(t, w, http.StatusForbidden)
				}
				requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, cookie, proof), http.StatusOK)
				return
			case "concurrent":
				statuses := make(chan int, 2)
				var wg sync.WaitGroup
				for i := 0; i < 2; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						statuses <- authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, cookie, proof).Code
					}()
				}
				wg.Wait()
				close(statuses)
				wins := 0
				for status := range statuses {
					if status == http.StatusOK {
						wins++
					} else if status != http.StatusUnauthorized && status != http.StatusGone {
						t.Fatalf("unexpected concurrent status: %d", status)
					}
				}
				if wins != 1 {
					t.Fatalf("concurrent deletion had %d winners", wins)
				}
				return
			}
			requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, cookie, proof), http.StatusGone)
			if _, err := s.ResolveHuman(context.Background(), cookie.Value); err != nil {
				t.Fatal("a rejected proof deleted the account")
			}
		})
	}
}

func TestAccountDeletionOwnershipAndBillingGuards(t *testing.T) {
	for _, scenario := range []string{"last-owner", "system", "vps", "billing", "pending-subscription", "checkout", "unreported-usage", "late-resource"} {
		t.Run(scenario, func(t *testing.T) {
			s, h, _ := newAuthTestServer(t)
			ctx := context.Background()
			cookie, p := deletionAccount(t, h, "owner-delete@example.com")
			var proof *http.Cookie
			if scenario == "late-resource" {
				proof = startDeletion(t, h, cookie)
			}
			var err error
			switch scenario {
			case "last-owner":
				_, other := deletionAccount(t, h, "member@example.com")
				_, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, other.Account.ID, p.WorkspaceID)
			case "system", "late-resource":
				_, err = s.pool.Exec(ctx, `INSERT INTO systems(workspace_id,name,contract,m1_prefix) VALUES($1,'hosted','{}','account-deletion-test')`, p.WorkspaceID)
			case "vps":
				_, err = s.pool.Exec(ctx, `INSERT INTO workspace_vps(id,workspace_id,phase,document,created_at,updated_at) VALUES('vps_live',$1,'ready','{}',now(),now())`, p.WorkspaceID)
			case "billing":
				_, err = s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id,status) VALUES($1,'cus_live','sub_live','active')`, p.WorkspaceID)
			case "pending-subscription":
				_, err = s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id) VALUES($1,'cus_pending','sub_pending')`, p.WorkspaceID)
			case "checkout":
				_, err = s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,checkout_expires_at) VALUES($1,now()+interval '1 hour')`, p.WorkspaceID)
			case "unreported-usage":
				_, err = s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id,status) VALUES($1,'cus_closed','sub_closed','canceled')`, p.WorkspaceID)
				if err == nil {
					_, err = s.pool.Exec(ctx, `INSERT INTO billing_usage_events(id,workspace_id,customer_id,subscription_id,amount_cents,resource,rate_version,occurred_at) VALUES('usage_pending',$1,'cus_closed','sub_closed',1,'compute','test',now())`, p.WorkspaceID)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "late-resource" {
				requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": lastEmailCode(t, h)}, cookie, proof), http.StatusConflict)
			} else {
				requireStatus(t, authRequest(t, h, "account/delete/start", map[string]string{"password": "correct horse battery staple"}, cookie), http.StatusConflict)
			}
			if _, err := s.ResolveHuman(ctx, cookie.Value); err != nil {
				t.Fatal("guard did not preserve the account")
			}
		})
	}
}

func TestAccountDeletionRetainsClosedBillingWithoutAccountAccess(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	cookie, p := deletionAccount(t, h, "closed-billing-delete@example.com")
	if _, err := s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id,status,checkout_id,checkout_url,checkout_plan,checkout_expires_at) VALUES($1,'cus_closed','sub_closed','canceled','checkout_old','https://billing.example.test/old','pro',now()-interval '1 hour')`, p.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO billing_paid_invoices(invoice_id,workspace_id,amount_paid,currency,paid_at) VALUES('invoice_closed',$1,1200,'usd',now())`, p.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateConversation(ctx, p.WorkspaceID, p.Account.ID, "conv_closed_billing", "Private conversation"); err != nil {
		t.Fatal(err)
	}
	proof := startDeletion(t, h, cookie)
	requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": lastEmailCode(t, h)}, cookie, proof), http.StatusOK)
	var name, status, checkout string
	var members, conversations, invoices int
	if err := s.pool.QueryRow(ctx, `SELECT w.name,b.status,b.checkout_id||b.checkout_url||b.checkout_plan,(SELECT count(*) FROM memberships WHERE workspace_id=w.id),(SELECT count(*) FROM operator_conversations WHERE workspace_id=w.id),(SELECT count(*) FROM billing_paid_invoices WHERE workspace_id=w.id) FROM workspaces w JOIN workspace_billing b ON b.workspace_id=w.id WHERE w.id=$1`, p.WorkspaceID).Scan(&name, &status, &checkout, &members, &conversations, &invoices); err != nil {
		t.Fatal(err)
	}
	if name != "Closed workspace" || status != "canceled" || checkout != "" || members != 0 || conversations != 0 || invoices != 1 {
		t.Fatalf("closed billing retention: name=%q status=%q checkout=%q members=%d conversations=%d invoices=%d", name, status, checkout, members, conversations, invoices)
	}
	if _, err := s.ResolveHuman(ctx, cookie.Value); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("deleted account retained billing access")
	}
}

type deletionArtifactEngine struct {
	Engine
	keys []string
	fail bool
}

func (e *deletionArtifactEngine) DeleteControlPlaneArtifact(_ context.Context, key string) error {
	if e.fail {
		return errors.New("temporarily unavailable")
	}
	e.keys = append(e.keys, key)
	return nil
}

func TestAccountDeletionKeepsSharedWorkspaceAndErasesOrphanArtifacts(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	cookie, p := deletionAccount(t, h, "shared-delete@example.com")
	otherCookie, other := deletionAccount(t, h, "remaining-owner@example.com")
	if _, err := s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'owner')`, p.Account.ID, other.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO agent_installations(id,workspace_id,name,harness,created_by) VALUES('agt_deleted_owner',$1,'Authorized agent','codex',$2);`, other.WorkspaceID, p.Account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO workspace_tasks(id,workspace_id,prompt,status,requested_by) VALUES('task_history',$1,'Shared task','completed',$2)`, other.WorkspaceID, p.Account.ID); err != nil {
		t.Fatal(err)
	}
	privateDigest, sharedDigest := strings.Repeat("a", 64), strings.Repeat("b", 64)
	privateKey, _ := sdk.ControlPlaneArtifactKey(privateDigest)
	sharedKey, _ := sdk.ControlPlaneArtifactKey(sharedDigest)
	for _, artifact := range []struct{ workspace, digest, key string }{{p.WorkspaceID, privateDigest, privateKey}, {p.WorkspaceID, sharedDigest, sharedKey}, {other.WorkspaceID, sharedDigest, sharedKey}} {
		if _, err := s.RecordDeploymentArtifact(ctx, artifact.workspace, sdk.StagedArtifact{SHA256: artifact.digest, Key: artifact.key, Size: 1}, nil, p.Actor); err != nil {
			t.Fatal(err)
		}
	}
	proof := startDeletion(t, h, cookie)
	requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": lastEmailCode(t, h)}, cookie, proof), http.StatusOK)
	if _, err := s.Workspace(ctx, p.WorkspaceID); !errors.Is(err, ErrNotFound) {
		t.Fatal("private workspace survived")
	}
	if _, err := s.ResolveHuman(ctx, otherCookie.Value); err != nil {
		t.Fatal("other owner lost access")
	}
	agents, err := s.ListInstallations(ctx, other.WorkspaceID)
	if err != nil || len(agents) != 1 || agents[0].CreatedBy != "" || agents[0].RevokedAt == nil {
		t.Fatalf("shared agent cleanup: %v %v", agents, err)
	}
	tasks, err := s.ListWorkspaceTasks(ctx, other.WorkspaceID)
	if err != nil || len(tasks) != 1 || tasks[0].RequestedBy != "" {
		t.Fatalf("shared task cleanup: %v %v", tasks, err)
	}
	engine := &deletionArtifactEngine{fail: true}
	h.service.Engine = engine
	if processed, err := h.eraseDeletedAccountArtifact(ctx); err != nil || !processed {
		t.Fatal("erasure retry was not queued", err)
	}
	engine.fail = false
	if _, err := s.pool.Exec(ctx, `UPDATE account_deletion_artifacts SET next_attempt_at=now()`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := h.eraseDeletedAccountArtifact(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(engine.keys) != 1 || engine.keys[0] != privateKey {
		t.Fatalf("erased another owner's artifact: %v", engine.keys)
	}
}

func TestAccountDeletionPasswordlessUsesRecentPrimaryAuthentication(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	cookie, p := deletionAccount(t, h, "passwordless-delete@example.com")
	if _, err := s.pool.Exec(ctx, `UPDATE accounts SET password_hash='!oauth' WHERE id=$1`, p.Account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE human_sessions SET authenticated_at=now()-interval '10 minutes' WHERE account_id=$1`, p.Account.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, authRequest(t, h, "account/delete/start", map[string]string{}, cookie), http.StatusPreconditionRequired)
	if _, err := s.pool.Exec(ctx, `UPDATE human_sessions SET authenticated_at=now() WHERE account_id=$1`, p.Account.ID); err != nil {
		t.Fatal(err)
	}
	w := authRequest(t, h, "account/delete/start", map[string]string{}, cookie)
	requireStatus(t, w, http.StatusAccepted)
	proof := authCookie(t, h, w, "deletion")
	requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": lastEmailCode(t, h)}, cookie, proof), http.StatusOK)
}

// A deletion proof cannot be swapped for a signup/reset proof, even if the code
// is valid. Purpose and browser cookies are independent.
func TestAccountDeletionRejectsOtherProofPurposes(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	cookie, _ := deletionAccount(t, h, "purpose-delete@example.com")
	now := s.now().Add(2 * time.Minute)
	s.now = func() time.Time { return now }
	w := authRequest(t, h, "password/reset/start", authInput{Email: "purpose-delete@example.com"}, cookie)
	requireStatus(t, w, http.StatusAccepted)
	proof := authCookie(t, h, w, "challenge")
	proof.Name = h.authCookieName("deletion")
	body, _ := json.Marshal(map[string]string{"code": lastEmailCode(t, h)})
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/account/delete/finish", bytes.NewReader(body))
	r.Header.Set("Origin", h.config.PublicURL)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(cookie)
	r.AddCookie(proof)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	requireStatus(t, w, http.StatusGone)
}
