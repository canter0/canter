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

	"github.com/canter0/canter/sdk"
)

type computerFakeEngine struct {
	vpsFakeEngine
	plans    int
	lastPlan sdk.VPSPlan
}

func (e *computerFakeEngine) PlanVPS(ctx context.Context, workspace, id string, r sdk.VPSRequest) (sdk.VPSPlan, error) {
	e.plans++
	plan, err := e.vpsFakeEngine.PlanVPS(ctx, workspace, id, r)
	plan.Shape.GB = 40
	e.lastPlan = plan
	return plan, err
}
func computerFixture(t *testing.T) (*HTTPServer, Conversation, string, *computerFakeEngine) {
	t.Helper()
	s, c, token := operatorFixture(t)
	engine := &computerFakeEngine{}
	h := NewHTTPServer(&Service{Store: s, Engine: engine}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Computers: WrenComputerConfig{SSHSourceCIDR: "203.0.113.8/32", AllowedWorkspaces: []string{c.WorkspaceID}, MaxLifetimeSeconds: 3600}}).(*HTTPServer)
	return h, c, token, engine
}
func readyComputer(t *testing.T, h *HTTPServer, v WrenComputer) WrenComputer {
	t.Helper()
	if _, err := h.service.Store.pool.Exec(context.Background(), `UPDATE wren_computers SET phase='ready',address='127.0.0.1',host_fingerprint='SHA256:test-host' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	v, err := h.service.Store.wrenComputerPrivate(context.Background(), v.WorkspaceID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestWrenComputersExplicitGrantScopesAndEncryptedKeys(t *testing.T) {
	h, c, _, engine := computerFixture(t)
	s := h.service.Store
	ctx := context.Background()
	a, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_computer_a", Name: "Designer", Shape: "square"}, true)
	if err != nil {
		t.Fatal(err)
	}
	h.config.Computers.AllowedWorkspaces = nil
	if _, err = h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, a.ConversationID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("missing paid grant: %v", err)
	}
	if engine.plans != 0 || engine.applied != 0 {
		t.Fatal("ungranted workspace touched provider")
	}
	h.config.Computers.AllowedWorkspaces = []string{c.WorkspaceID}
	private, err := h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, a.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if private.Phase != "queued" || private.ScopeKind != "private" || private.OwnerID != c.AccountID || private.MonthlyCents <= 0 || private.EstimatedCents <= 0 || !strings.Contains(private.Plan.Spec.Spec.M1.Prefix, "/wren-computers/") || !strings.Contains(private.Plan.Spec.Spec.Compute.Bootstrap, "restrict,command=") {
		t.Fatalf("computer allocation: %#v", private)
	}
	if engine.applied != 0 {
		t.Fatal("enable bypassed durable dispatcher")
	}
	retry, err := h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, a.ConversationID)
	if err != nil || retry.ID != private.ID || engine.plans != 1 {
		t.Fatalf("grant retry: %s %v plans=%d", retry.ID, err, engine.plans)
	}
	public, _ := json.Marshal(private)
	for _, secret := range []string{"ciphertext", "PRIVATE KEY", "private-shape", "private-network", "ssh_key", "hostFingerprint"} {
		if strings.Contains(string(public), secret) {
			t.Fatalf("private computer internals exposed: %s", secret)
		}
	}
	plain, err := h.config.Secrets.open(private.KeyVersion, private.KeyCiphertext, secretBinding(c.WorkspaceID, private.ID, "wren-computer-ssh"))
	if err != nil || !strings.Contains(string(plain), "PRIVATE KEY") {
		t.Fatalf("encrypted key unavailable: %v", err)
	}
	clear(plain)
	if _, err = h.config.Secrets.open(private.KeyVersion, private.KeyCiphertext, secretBinding(c.WorkspaceID, "another_computer", "wren-computer-ssh")); err == nil {
		t.Fatal("key unbound from environment identity")
	}
	if _, _, err = h.ClientForConversation(ctx, Conversation{ID: a.ConversationID, WorkspaceID: c.WorkspaceID, AccountID: c.AccountID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("queued computer usable: %v", err)
	}
	private = readyComputer(t, h, private)
	if client, _, err := h.ClientForConversation(ctx, Conversation{ID: a.ConversationID, WorkspaceID: c.WorkspaceID, AccountID: c.AccountID}); err != nil || client == nil {
		t.Fatalf("ready private client: %v", err)
	}
	guest, _, _, err := s.Signup(ctx, "computer-guest@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, guest.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.WrenComputer(ctx, c.WorkspaceID, guest.ID, private.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("private computer leaked: %v", err)
	}
	b, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_computer_b", Name: "Writer", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	room, err := s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, WrenRoom{ID: "room_computer", Kind: "group", Name: "Shared work", WrenIDs: []string{a.ID, b.ID}, MemberIDs: []string{guest.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, room.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if shared.ScopeKind != "room" || shared.ID == private.ID {
		t.Fatal("room reused private computer")
	}
	if _, err = h.EnableWrenComputer(ctx, c.WorkspaceID, guest.ID, room.ConversationID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("guest enabled paid compute: %v", err)
	}
	if _, err = h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, b.ConversationID); !errors.Is(err, ErrCapacity) {
		t.Fatalf("computer capacity cap: %v", err)
	}
	shared = readyComputer(t, h, shared)
	if client, v, err := h.ClientForConversation(ctx, Conversation{ID: room.ConversationID, WorkspaceID: c.WorkspaceID, AccountID: guest.ID}); err != nil || client == nil || v.ID != shared.ID {
		t.Fatalf("member shared client: %v", err)
	}
	visible, err := s.WrenComputers(ctx, c.WorkspaceID, guest.ID)
	if err != nil || len(visible) != 1 || visible[0].ID != shared.ID {
		t.Fatalf("computer roster isolation: %#v %v", visible, err)
	}
	room.MemberIDs = nil
	if _, err = s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, room, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err = h.ClientForConversation(ctx, Conversation{ID: room.ConversationID, WorkspaceID: c.WorkspaceID, AccountID: guest.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked guest retained computer: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE wren_computers SET expires_at=now()-interval '1 second' WHERE id=$1`, shared.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = h.ClientForConversation(ctx, Conversation{ID: room.ConversationID, WorkspaceID: c.WorkspaceID, AccountID: c.AccountID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired computer usable: %v", err)
	}
	// Deleting a scope preserves the durable paid-resource record for cleanup.
	if _, err = s.pool.Exec(ctx, `DELETE FROM operator_conversations WHERE id=$1`, a.ConversationID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.wrenComputerPrivate(ctx, c.WorkspaceID, private.ID); err != nil {
		t.Fatalf("scope deletion orphaned paid VM: %v", err)
	}
	dispatcher := WrenComputerDispatcher{Server: h}
	engine.deletionPending = true
	if err = dispatcher.Reconcile(ctx, c.WorkspaceID, private.ID); err == nil {
		t.Fatal("unverified provider deletion reported success")
	}
	pending, err := s.wrenComputerPrivate(ctx, c.WorkspaceID, private.ID)
	if err != nil || pending.Phase != "deleting" || len(pending.KeyCiphertext) == 0 {
		t.Fatalf("uncertain deletion: %#v %v", pending, err)
	}
	engine.deletionPending = false
	if err = dispatcher.Reconcile(ctx, c.WorkspaceID, private.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.wrenComputerPrivate(ctx, c.WorkspaceID, private.ID)
	if err != nil || deleted.Phase != "deleted" || len(deleted.KeyCiphertext) != 0 || deleted.Address != "" {
		t.Fatalf("verified deletion: %#v %v", deleted, err)
	}
	stale := private
	stale.Phase = "ready"
	if err = dispatcher.save(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale worker resurrected deleted VM: %v", err)
	}
	if err = dispatcher.Reconcile(ctx, c.WorkspaceID, shared.ID); err != nil {
		t.Fatal(err)
	}
	var reserved int
	if err = s.pool.QueryRow(ctx, `SELECT reserved_cents FROM workspace_usage_caps WHERE workspace_id=$1`, c.WorkspaceID).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatalf("reservation not released: %d %v", reserved, err)
	}
}
func TestWrenComputerHTTPHumanEnableAndPaidGrant(t *testing.T) {
	h, c, token, engine := computerFixture(t)
	ctx := context.Background()
	s := h.service.Store
	a, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_computer_http", Name: "Researcher", Shape: "flower"}, true)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/computers"+path, strings.NewReader(body))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		w := httptest.NewRecorder()
		h.workspaceComputers(w, r, Principal{Account: &Account{ID: c.AccountID}, Actor: sdk.ActorRef{Kind: "human", ID: c.AccountID}}, c.WorkspaceID, strings.FieldsFunc(strings.TrimPrefix(path, "/"), func(r rune) bool { return r == '/' }))
		return w
	}
	// Test the exact account-only HTTP entry point; no agent/model principal can
	// enable a paid environment regardless of a fabricated profile or intent.
	r := httptest.NewRequest("POST", "http://canter.test", strings.NewReader(`{"conversationId":"`+a.ConversationID+`"}`))
	w := httptest.NewRecorder()
	h.workspaceComputers(w, r, Principal{Installation: &Installation{}}, c.WorkspaceID, nil)
	if w.Code != 403 {
		t.Fatalf("agent provisioned compute: %d", w.Code)
	}
	h.config.Computers.AllowedWorkspaces = nil
	denied := request("POST", "/enable", `{"conversationId":"`+a.ConversationID+`"}`)
	if denied.Code != 403 || engine.plans != 0 {
		t.Fatalf("HTTP missing grant: %d %s", denied.Code, denied.Body)
	}
	h.config.Computers.AllowedWorkspaces = []string{c.WorkspaceID}
	created := request("POST", "/enable", `{"conversationId":"`+a.ConversationID+`"}`)
	if created.Code != 202 {
		t.Fatalf("HTTP enable: %d %s", created.Code, created.Body)
	}
	var v WrenComputer
	if err = json.Unmarshal(created.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.OwnerID != c.AccountID || engine.applied != 0 {
		t.Fatalf("HTTP enable state: %#v", v)
	}
	unsupported := request("POST", "/"+v.ID+"/suspend", "{}")
	if unsupported.Code != 409 {
		t.Fatalf("unverified suspend exposed: %d", unsupported.Code)
	}
	if got := request("GET", "/"+v.ID+"/screenshot", ""); got.Code != 409 {
		t.Fatalf("queued VM rendered fake screenshot: %d %s", got.Code, got.Body)
	}
	// An interrupted provisioning attempt keeps its intent; its cleanup is
	// reconciled independently of whether the browser remains connected.
	engine.failApply = true
	dispatcher := WrenComputerDispatcher{Server: h}
	if err = dispatcher.Reconcile(ctx, c.WorkspaceID, v.ID); err == nil {
		t.Fatal("provider failure hidden")
	}
	pending, err := s.wrenComputerPrivate(ctx, c.WorkspaceID, v.ID)
	if err != nil || pending.Phase != "deleting" {
		t.Fatalf("failed provisioning not cleaned: %#v %v", pending, err)
	}
	if err = dispatcher.Reconcile(ctx, c.WorkspaceID, v.ID); err != nil {
		t.Fatal(err)
	}
	terminal, err := s.wrenComputerPrivate(ctx, c.WorkspaceID, v.ID)
	if err != nil || terminal.Phase != "deleted" || !strings.Contains(terminal.Failure, "Computer setup failed during provider setup.") || !strings.Contains(terminal.Failure, "Allocated resources were removed") {
		t.Fatalf("setup failure vanished after cleanup: %s %v", terminal.Failure, err)
	}
	if engine.destroyed != 1 {
		t.Fatalf("cleanup attempts=%d", engine.destroyed)
	}
}

func TestWrenComputerHumanControlLease(t *testing.T) {
	h, c, _, _ := computerFixture(t)
	ctx := context.Background()
	s := h.service.Store
	a, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_computer_control", Name: "Reader", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	guest, _, _, err := s.Signup(ctx, "computer-control-guest@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, guest.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	room, err := s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, WrenRoom{ID: "room_computer_control", Kind: "group", Name: "Shared desktop", WrenIDs: []string{a.ID}, MemberIDs: []string{guest.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	v, err := h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, room.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	v = readyComputer(t, h, v)
	conversation := Conversation{ID: room.ConversationID, WorkspaceID: c.WorkspaceID, AccountID: c.AccountID}
	if err = h.requireHumanComputerControl(ctx, v, c.AccountID); !errors.Is(err, ErrConflict) {
		t.Fatalf("input accepted without lease: %v", err)
	}
	// Concurrent acquire has one winner; the second participant cannot steal it.
	start := make(chan struct{})
	results := make(chan struct {
		account  string
		computer WrenComputer
		err      error
	}, 2)
	for _, account := range []string{c.AccountID, guest.ID} {
		go func(account string) {
			<-start
			computer, e := h.SetComputerControl(ctx, c.WorkspaceID, account, v.ID, "human")
			results <- struct {
				account  string
				computer WrenComputer
				err      error
			}{account, computer, e}
		}(account)
	}
	close(start)
	winner, loser := "", ""
	for range 2 {
		result := <-results
		if result.err == nil {
			if winner != "" {
				t.Fatal("two simultaneous human lease holders")
			}
			winner = result.account
			if result.computer.ControlAccountID != winner || result.computer.ControlExpiresAt == nil {
				t.Fatal("lease metadata missing")
			}
		} else {
			if !errors.Is(result.err, ErrConflict) {
				t.Fatal(result.err)
			}
			loser = result.account
		}
	}
	if winner == "" || loser == "" {
		t.Fatal("lease arbitration did not choose one holder")
	}
	if err = h.requireHumanComputerControl(ctx, v, winner); err != nil {
		t.Fatalf("holder cannot input: %v", err)
	}
	if err = h.ComputerAgentWriteAllowed(ctx, conversation); !errors.Is(err, ErrConflict) {
		t.Fatalf("bot wrote during takeover: %v", err)
	}
	if _, err = h.SetComputerControl(ctx, c.WorkspaceID, loser, v.ID, "agent"); !errors.Is(err, ErrConflict) {
		t.Fatalf("other person released lease: %v", err)
	}
	if _, err = h.SetComputerControl(ctx, c.WorkspaceID, winner, v.ID, "human"); err != nil {
		t.Fatalf("holder renewal failed: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE wren_computers SET control_expires_at=now()-interval '1 second' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if err = h.ComputerAgentWriteAllowed(ctx, conversation); err != nil {
		t.Fatalf("expired lease blocks bot: %v", err)
	}
	controlled, err := h.SetComputerControl(ctx, c.WorkspaceID, guest.ID, v.ID, "human")
	if err != nil || controlled.ControlAccountID != guest.ID || controlled.ControlName != "computer-control-guest" {
		t.Fatalf("expired lease takeover: %v", err)
	}
	room.MemberIDs = nil
	if _, err = s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, room, false); err != nil {
		t.Fatal(err)
	}
	visible, err := s.WrenComputer(ctx, c.WorkspaceID, c.AccountID, v.ID)
	if err != nil || visible.ControlAccountID != "" || visible.ControlName != "" || visible.ControlExpiresAt != nil {
		t.Fatal("revoked holder still presented as controlling", err)
	}
	if _, err = h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "human"); err != nil {
		t.Fatalf("revoked holder blocked current owner: %v", err)
	}
	if _, err = h.SetComputerControl(ctx, c.WorkspaceID, guest.ID, v.ID, "human"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked guest renewed control: %v", err)
	}
	if _, err = h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "agent"); err != nil {
		t.Fatal(err)
	}
	if err = h.ComputerAgentWriteAllowed(ctx, conversation); err != nil {
		t.Fatalf("released control still blocks bot: %v", err)
	}
	first, err := h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "human")
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "human", *first.ControlExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "agent", *first.ControlExpiresAt); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale same-account panel released new lease: %v", err)
	}
	if _, err = h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "human", *first.ControlExpiresAt); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale same-account heartbeat renewed new lease: %v", err)
	}
	if _, err = h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "agent", *renewed.ControlExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err = h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "human", *renewed.ControlExpiresAt); !errors.Is(err, ErrConflict) {
		t.Fatalf("late heartbeat reacquired released lease: %v", err)
	}

}

func TestComputerSetupFailureDoesNotExposeProviderDetails(t *testing.T) {
	for _, raw := range []string{"compute became active but did not write boot proof before deadline: https://secret.example/?token=private", "public endpoint request failed secret-value", "unknown private-key payload"} {
		message := computerSetupFailure(fmt.Errorf("%s: %w", raw, context.DeadlineExceeded), "creating")
		if strings.Contains(message, "secret") || strings.Contains(message, "private") || strings.Contains(message, "https:") || !strings.Contains(message, "setup deadline reached") {
			t.Fatalf("unsafe classification: %s", message)
		}
	}
}

func TestWrenComputerMutationLeaseFencesOwnersAndHumanTakeover(t *testing.T) {
	h, c, run, job, _, _ := harnessFixture(t)
	ctx := context.Background()
	s := h.service.Store
	v, err := h.AcquireComputerMutationLease(ctx, c, run)
	if err != nil {
		t.Fatal(err)
	}
	if v.MutationRunID != run.ID || v.MutationExpiresAt == nil {
		t.Fatal("missing bounded mutation owner")
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), run.Lease) {
		t.Fatal("operator lease leaked to computer API")
	}
	var parent OperatorRun
	if err = s.pool.QueryRow(ctx, `SELECT id,lease_token FROM operator_runs WHERE id=$1`, job.ParentRunID).Scan(&parent.ID, &parent.Lease); err != nil {
		t.Fatal(err)
	}
	if _, err = h.AcquireComputerMutationLease(ctx, c, parent); !errors.Is(err, ErrConflict) {
		t.Fatalf("other run raced active coding worker: %v", err)
	}
	if _, err = h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "human"); !errors.Is(err, ErrConflict) {
		t.Fatalf("human takeover pretended to pause active shell: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET lease_token='replacement-run-fence',lease_expires_at=now()+interval '45 seconds' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.AcquireComputerMutationLease(ctx, c, run); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale operator reacquired: %v", err)
	}
	renewed := run
	renewed.Lease = "replacement-run-fence"
	if _, err = h.AcquireComputerMutationLease(ctx, c, renewed); err != nil {
		t.Fatal(err)
	}
	if err = h.ReleaseComputerMutationLease(ctx, v.ID, run.ID, run.Lease); !errors.Is(err, ErrConflict) {
		t.Fatalf("old worker released new owner: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE wren_harness_executions SET process_stopped=true,status='stopped' WHERE job_id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "human"); err != nil {
		t.Fatal(err)
	}
	if _, err = h.AcquireComputerMutationLease(ctx, c, renewed); !errors.Is(err, ErrConflict) {
		t.Fatalf("agent mutated during human lease: %v", err)
	}
	if _, err = h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err = h.AcquireComputerMutationLease(ctx, c, renewed); err != nil {
		t.Fatal(err)
	}
	if err = h.ReleaseComputerMutationLease(ctx, v.ID, run.ID, renewed.Lease); err != nil {
		t.Fatal(err)
	}
}

func TestWrenComputerInternalFileFence(t *testing.T) {
	for _, item := range []struct {
		path string
		want bool
	}{
		{"", true}, {"/workspace", true}, {"jobs/job_a/index.html", true}, {"/workspace/.git/config", true},
		{"/workspace/.canter/harness/job/launch.json", false}, {".canter/harness/job/opencode.json", false},
		{"/workspace/.browser/Default/Cookies", false}, {"/workspace/jobs/../.canter/harness/key", false}, {"/etc/passwd", false},
	} {
		if got := ComputerUserFileAllowed(item.path); got != item.want {
			t.Errorf("%q got%v want%v", item.path, got, item.want)
		}
	}
}
