package controlplane

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func multiplayerPerson(t *testing.T, s *Store, email string) Account {
	t.Helper()
	a, _, _, err := s.Signup(context.Background(), email, "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestWrenMultiplayerInvitationBindingReplayAndOwnership(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	member := multiplayerPerson(t, s, "invited@example.com")
	wrong := multiplayerPerson(t, s, "wrong-recipient@example.com")
	input := WorkspaceInviteInput{ID: "winvite_stable_request", Email: member.Email, Role: "operator"}
	invite, token, err := s.CreateWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, input)
	if err != nil || token == "" {
		t.Fatalf("create %+v %v", invite, err)
	}
	var hash []byte
	if err = s.pool.QueryRow(ctx, `SELECT token_hash FROM workspace_invites WHERE id=$1`, invite.ID).Scan(&hash); err != nil || subtle.ConstantTimeCompare(hash, secretHash(token)) != 1 || strings.Contains(string(hash), token) {
		t.Fatalf("token not securely hashed %v", err)
	}
	replay, replayedToken, err := s.CreateWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, input)
	if err != nil || replay.ID != invite.ID || replayedToken != "" {
		t.Fatalf("create replay %+v %q %v", replay, replayedToken, err)
	}
	changed := input
	changed.Email = wrong.Email
	if _, _, err = s.CreateWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed create replay %v", err)
	}
	if _, err = s.ConsumeWorkspaceInvite(ctx, wrong.ID, token, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("different email accepted %v", err)
	}
	var group sync.WaitGroup
	result := make(chan error, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			v, e := s.ConsumeWorkspaceInvite(ctx, member.ID, token, true)
			if e == nil && v.State != "accepted" {
				e = errors.New("not accepted")
			}
			result <- e
		}()
	}
	group.Wait()
	close(result)
	for e := range result {
		if e != nil {
			t.Fatal(e)
		}
	}
	members, err := s.WorkspaceMembers(ctx, c.WorkspaceID, member.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("one-use join %+v %v", members, err)
	}
	if _, err = s.Conversation(ctx, c.WorkspaceID, member.ID, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invite copied private history %v", err)
	}
	if err = s.UpdateWorkspaceMember(ctx, c.WorkspaceID, member.ID, c.AccountID, "viewer", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator changed owner %v", err)
	}
	if err = s.LeaveWorkspace(ctx, c.WorkspaceID, c.AccountID); !errors.Is(err, ErrConflict) {
		t.Fatalf("last owner left %v", err)
	}
	if err = s.TransferWorkspaceOwnership(ctx, c.WorkspaceID, c.AccountID, member.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.LeaveWorkspace(ctx, c.WorkspaceID, c.AccountID); err != nil {
		t.Fatal(err)
	}
	if err = s.LeaveWorkspace(ctx, c.WorkspaceID, member.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("new last owner left %v", err)
	}
	if _, err = s.ConsumeWorkspaceInvite(ctx, member.ID, token, true); err != nil {
		t.Fatalf("accepted member lost retry receipt %v", err)
	}
}
func TestWrenMultiplayerInviteDeclineExpiryRevocationViewer(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	member := multiplayerPerson(t, s, "viewer-invite@example.com")
	invite, token, err := s.CreateWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, WorkspaceInviteInput{Email: member.Email, Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, invite.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConsumeWorkspaceInvite(ctx, member.ID, token, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoked invite consumed %v", err)
	}
	invite, token, err = s.CreateWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, WorkspaceInviteInput{Email: member.Email, Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ConsumeWorkspaceInvite(ctx, member.ID, token, false); err != nil || got.State != "declined" {
		t.Fatalf("decline %+v %v", got, err)
	}
	if _, err = s.ConsumeWorkspaceInvite(ctx, member.ID, token, false); err != nil {
		t.Fatalf("decline replay %v", err)
	}
	if _, err = s.ConsumeWorkspaceInvite(ctx, member.ID, token, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("declined invite accepted %v", err)
	}
	invite, token, err = s.CreateWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, WorkspaceInviteInput{Email: member.Email, Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE workspace_invites SET expires_at=now()-interval '1 minute' WHERE id=$1`, invite.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConsumeWorkspaceInvite(ctx, member.ID, token, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired invite accepted %v", err)
	}
	_, token, err = s.CreateWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, WorkspaceInviteInput{Email: member.Email, Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConsumeWorkspaceInvite(ctx, member.ID, token, true); err != nil {
		t.Fatal(err)
	}
	if err = s.RequireWorkspaceWriter(ctx, c.WorkspaceID, member.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer can write %v", err)
	}
	if _, _, err = s.CreateWorkspaceInvite(ctx, c.WorkspaceID, member.ID, WorkspaceInviteInput{Email: "another@example.com"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer invited someone %v", err)
	}
	if err = s.UpdateWorkspaceMember(ctx, c.WorkspaceID, c.AccountID, member.ID, "", true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConsumeWorkspaceInvite(ctx, member.ID, token, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("old accepted token restored membership %v", err)
	}
	if _, err = s.WorkspaceMembers(ctx, c.WorkspaceID, member.ID); err == nil {
		t.Fatal("revoked member can read roster")
	}
}
func TestWrenMultiplayerReadReplySearchAndScope(t *testing.T) {
	s, private, w, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	privateRun, err := s.EnqueueOperator(ctx, private, "search-private-request", "Secret kumquat private customer", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = privateRun
	roomRun, err := s.EnqueueOperator(ctx, shared, "search-room-request", "Shared kumquat source", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET speaker=$2 WHERE id=$1`, roomRun.ID, w); err != nil {
		t.Fatal(err)
	}
	replyID := "msg_shared_answer"
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_messages(id,conversation_id,run_id,role,content) VALUES($1,$2,$3,'assistant','A sourced kumquat answer.')`, replyID, shared.ID, roomRun.ID); err != nil {
		t.Fatal(err)
	}
	target, err := s.WrenReplyTarget(ctx, private.WorkspaceID, member, shared.ID, replyID)
	if err != nil || target.WrenID != w.ID {
		t.Fatalf("reply target %+v %v", target, err)
	}
	if _, err = s.WrenReplyTarget(ctx, private.WorkspaceID, member, private.ID, replyID); err == nil {
		t.Fatal("cross conversation reply allowed")
	}
	shared.AccountID = member
	userRun, err := s.EnqueueOperator(ctx, shared, "reply-member-request", "Follow up on that answer", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	var messageID string
	if err = s.pool.QueryRow(ctx, `SELECT id FROM operator_messages WHERE run_id=$1 AND role='user'`, userRun.ID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = RecordWrenMessageReply(ctx, tx, private.WorkspaceID, member, shared.ID, messageID, replyID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	replies, err := s.WrenMessageReplies(ctx, private.WorkspaceID, member, shared.ID)
	if err != nil || replies[messageID].WrenID != w.ID {
		t.Fatalf("stable reply metadata %+v %v", replies, err)
	}
	artifact, err := s.PutWrenArtifact(ctx, private.WorkspaceID, private.AccountID, WrenArtifactInput{ConversationID: shared.ID, Name: "kumquat.csv", MediaType: "text/csv", Content: []byte("name\nk umquat\n")})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := s.MarkWrenRead(ctx, private.WorkspaceID, member, shared.ID, 1<<60)
	if err != nil || cursor.Sequence != cursor.LatestSequence || cursor.Sequence < 1 {
		t.Fatalf("cursor beyond real max %+v %v", cursor, err)
	}
	old := cursor.Sequence
	cursor, err = s.MarkWrenRead(ctx, private.WorkspaceID, member, shared.ID, 0)
	if err != nil || cursor.Sequence != old {
		t.Fatalf("cursor moved backwards %+v %v", cursor, err)
	}
	own, err := s.WrenReadCursor(ctx, private.WorkspaceID, private.AccountID, shared.ID)
	if err != nil || own.Sequence != 0 {
		t.Fatalf("cursor shared across users %+v %v", own, err)
	}
	results, err := s.SearchWrens(ctx, private.WorkspaceID, member, "kumquat", 50)
	if err != nil {
		t.Fatal(err)
	}
	foundFile := false
	for _, v := range results {
		if v.ConversationID == private.ID || strings.Contains(v.Excerpt, "Secret") {
			t.Fatalf("private result leaked %+v", v)
		}
		if v.ID == artifact.ID {
			foundFile = true
		}
	}
	if !foundFile {
		t.Fatalf("artifact not found %+v", results)
	}
	// A persisted read receipt and exact hit IDs survive a second Store value.
	reopened := NewStore(s.pool)
	reopened.now = s.now
	cursor, err = reopened.WrenReadCursor(ctx, private.WorkspaceID, member, shared.ID)
	if err != nil || cursor.Sequence != old {
		t.Fatalf("read receipt disappeared %+v %v", cursor, err)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM memberships WHERE workspace_id=$1 AND account_id=$2`, private.WorkspaceID, member); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MarkWrenRead(ctx, private.WorkspaceID, member, shared.ID, old); err == nil {
		t.Fatal("revoked member advanced cursor")
	}
	if _, err = s.SearchWrens(ctx, private.WorkspaceID, member, "kumquat", 10); err == nil {
		t.Fatal("revoked member searched")
	}
}
func TestWrenMultiplayerDowngradeFencesWorkAndPendingInvites(t *testing.T) {
	s, private, w, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	shared.AccountID = member
	run, err := s.EnqueueOperator(ctx, shared, "downgrade-member-run", "Compute a team report", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	other := shared
	other.AccountID = private.AccountID
	unrelated, err := s.EnqueueOperator(ctx, other, "downgrade-owner-run", "Unrelated owner work", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	responsibility, err := s.SaveWrenResponsibility(ctx, private.WorkspaceID, member, responsibilityInput(shared, w))
	if err != nil {
		t.Fatal(err)
	}
	invite, token, err := s.CreateWorkspaceInvite(ctx, private.WorkspaceID, member, WorkspaceInviteInput{Email: "pending-recipient@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateWorkspaceMember(ctx, private.WorkspaceID, private.AccountID, member, "viewer", false); err != nil {
		t.Fatal(err)
	}
	var status string
	s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, run.ID).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("downgraded active run %s", status)
	}
	s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, unrelated.ID).Scan(&status)
	if status != "queued" {
		t.Fatalf("unrelated stopped %s", status)
	}
	r, err := s.WrenResponsibility(ctx, private.WorkspaceID, member, responsibility.ID)
	if err != nil || r.State != "paused" {
		t.Fatalf("routine not paused %+v %v", r, err)
	}
	s.pool.QueryRow(ctx, `SELECT state FROM workspace_invites WHERE id=$1`, invite.ID).Scan(&status)
	if status != "revoked" {
		t.Fatalf("pending invite %s", status)
	}
	_ = token
	if err = s.ValidateResponsibilityRun(ctx, shared, OperatorRun{ID: run.ID}); err != nil && !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if _, err = s.TriggerWrenResponsibility(ctx, private.WorkspaceID, member, responsibility.ID, "viewer-retrigger", "", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer triggered %v", err)
	}
	if err = s.UpdateWorkspaceMember(ctx, private.WorkspaceID, private.AccountID, member, "", true); err != nil {
		t.Fatal(err)
	}
	var groupLinks int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_room_members WHERE account_id=$1 AND room_id IN (SELECT id FROM wren_rooms WHERE workspace_id=$2)`, member, private.WorkspaceID).Scan(&groupLinks)
	if groupLinks != 0 {
		t.Fatalf("stale room grants %d", groupLinks)
	}
}

func TestWrenMultiplayerInviteEmailOutboxIsSealedAndRevocable(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	mailer := &authTestMailer{}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "https://canter.test", Secrets: testVault(t), Auth: AuthConfig{Mailer: mailer}}).(*HTTPServer)
	callback := func(tx pgx.Tx, v WorkspaceInvite, token string) error {
		return h.queueWorkspaceInviteEmail(ctx, tx, v, "Explicit test invitation: "+token)
	}
	input := WorkspaceInviteInput{ID: "winvite_email_outbox", Email: "owned-qa@example.com", Role: "operator", SendEmail: true}
	invite, token, err := s.createWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, input, callback)
	if err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	var mailID string
	if err = s.pool.QueryRow(ctx, `SELECT m.id,m.ciphertext FROM workspace_invites i JOIN auth_email_outbox m ON m.id=i.mail_outbox_id WHERE i.id=$1`, invite.ID).Scan(&mailID, &sealed); err != nil || bytes.Contains(sealed, []byte(token)) {
		t.Fatalf("invite outbox not sealed %v", err)
	}
	if replay, again, err := s.createWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, input, callback); err != nil || replay.ID != invite.ID || again != "" {
		t.Fatalf("mail retry recreated %+v %v", replay, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM auth_email_outbox WHERE id=$1`, mailID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate mail %d %v", count, err)
	}
	if sent, err := h.deliverAuthEmail(ctx); err != nil || !sent {
		t.Fatalf("fake delivery %v %v", sent, err)
	}
	if len(mailer.messages) != 1 || mailer.messages[0].To != input.Email || !strings.Contains(mailer.messages[0].Text, token) {
		t.Fatal("fake delivery missing intended recipient/link")
	}
	if err = s.RevokeWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, invite.ID); err != nil {
		t.Fatal(err)
	}
	input.ID = "winvite_email_revoked_before_send"
	invite, _, err = s.createWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, input, callback)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeWorkspaceInvite(ctx, c.WorkspaceID, c.AccountID, invite.ID); err != nil {
		t.Fatal(err)
	}
	if sent, err := h.deliverAuthEmail(ctx); err != nil || sent {
		t.Fatalf("revoked queued email sent %v %v", sent, err)
	}
	if len(mailer.messages) != 1 {
		t.Fatal("extra email after revocation")
	}
	var state string
	if err = s.pool.QueryRow(ctx, `SELECT m.status FROM workspace_invites i JOIN auth_email_outbox m ON m.id=i.mail_outbox_id WHERE i.id=$1`, invite.ID).Scan(&state); err != nil || state != "suppressed" {
		t.Fatalf("revoked mail state %s %v", state, err)
	}
}
func TestWrenMultiplayerSearchExcludesWorkerTranscript(t *testing.T) {
	s, private, _, _, _ := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.EnqueueOperator(ctx, private, "search-child-parent", "A bounded authorized task", "test", nil); err != nil {
		t.Fatal(err)
	}
	parent, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	job, err := s.CreateWrenJob(ctx, parent, private, "search-child-operation", "Bounded report", "Only the requested task", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_messages(id,conversation_id,run_id,role,content) VALUES('msg_hidden_child_search',$1,$2,'assistant','kumquat hidden worker scratch')`, job.ChildConversationID, job.ChildRunID); err != nil {
		t.Fatal(err)
	}
	results, err := s.SearchWrens(ctx, private.WorkspaceID, private.AccountID, "kumquat", 50)
	if err != nil || len(results) != 0 {
		t.Fatalf("hidden worker transcript leaked %+v %v", results, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE wren_jobs SET result='kumquat published summary',status='completed' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	results, err = s.SearchWrens(ctx, private.WorkspaceID, private.AccountID, "kumquat", 50)
	if err != nil || len(results) != 1 || results[0].Kind != "job" || results[0].ConversationID != private.ID {
		t.Fatalf("published result not scoped to origin %+v %v", results, err)
	}
}

func TestWrenMultiplayerUnreadTracksPeopleAndMeaningfulResults(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	own, err := s.EnqueueOperator(ctx, private, "unread-own-private", "My own prompt", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := s.WrenReadCursor(ctx, private.WorkspaceID, private.AccountID, private.ID)
	if err != nil || cursor.Unread != 0 {
		t.Fatalf("own private prompt unread %+v %v", cursor, err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) VALUES($1,$2,'queued','{"status":"queued"}'),($1,$2,'finished','{"status":"completed","content":"   "}')`, private.ID, own.ID); err != nil {
		t.Fatal(err)
	}
	cursor, err = s.WrenReadCursor(ctx, private.WorkspaceID, private.AccountID, private.ID)
	if err != nil || cursor.Unread != 0 {
		t.Fatalf("synthetic wake or quiet completion unread %+v %v", cursor, err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) VALUES($1,$2,'finished','{"status":"failed","content":""}')`, private.ID, own.ID); err != nil {
		t.Fatal(err)
	}
	cursor, err = s.WrenReadCursor(ctx, private.WorkspaceID, private.AccountID, private.ID)
	if err != nil || cursor.Unread != 1 {
		t.Fatalf("failed work not attention %+v %v", cursor, err)
	}
	roomRun, err := s.EnqueueOperator(ctx, shared, "unread-other-human", "A shared human message", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) VALUES($1,$2,'finished','{"status":"completed","content":"Actual useful result"}')`, shared.ID, roomRun.ID); err != nil {
		t.Fatal(err)
	}
	cursor, err = s.WrenReadCursor(ctx, private.WorkspaceID, member, shared.ID)
	if err != nil || cursor.Unread != 2 {
		t.Fatalf("other human plus assistant %+v %v", cursor, err)
	}
	owner, err := s.WrenReadCursor(ctx, private.WorkspaceID, private.AccountID, shared.ID)
	if err != nil || owner.Unread != 1 {
		t.Fatalf("own human counted foreign %+v %v", owner, err)
	}
	batch, err := s.WrenUnreadCounts(ctx, private.WorkspaceID, member)
	if err != nil || batch[shared.ID] != 2 || batch[private.ID] != 0 {
		t.Fatalf("scoped batch counts %+v %v", batch, err)
	}
	if _, err = s.MarkWrenRead(ctx, private.WorkspaceID, member, shared.ID, cursor.LatestSequence); err != nil {
		t.Fatal(err)
	}
	batch, err = s.WrenUnreadCounts(ctx, private.WorkspaceID, member)
	if err != nil || batch[shared.ID] != 0 {
		t.Fatalf("read did not clear counts %+v %v", batch, err)
	}
}

func TestWrenMultiplayerChildReadsFollowOriginGrant(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	shared.AccountID = member
	if _, err := s.EnqueueOperator(ctx, shared, "child-grant-parent", "Do bounded delegated research", "test", nil); err != nil {
		t.Fatal(err)
	}
	parent, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	job, err := s.CreateWrenJob(ctx, parent, shared, "child-grant-operation", "Private worker", "Use only this brief", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_messages(id,conversation_id,run_id,role,content) VALUES('msg_child_acl_target',$1,$2,'assistant','Worker evidence')`, job.ChildConversationID, job.ChildRunID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.WrenReplyTarget(ctx, private.WorkspaceID, member, job.ChildConversationID, "msg_child_acl_target"); err != nil {
		t.Fatalf("initial authorized child %v", err)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM wren_room_members WHERE room_id=(SELECT id FROM wren_rooms WHERE conversation_id=$1) AND account_id=$2`, shared.ID, member); err != nil {
		t.Fatal(err)
	}
	if _, err = s.WrenReplyTarget(ctx, private.WorkspaceID, member, job.ChildConversationID, "msg_child_acl_target"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("child bypassed origin revocation %v", err)
	}
	if _, err = s.MarkWrenRead(ctx, private.WorkspaceID, member, job.ChildConversationID, 1<<40); !errors.Is(err, ErrNotFound) {
		t.Fatalf("child cursor bypassed origin revocation %v", err)
	}
	if _, err = s.ListWrenArtifacts(ctx, private.WorkspaceID, member, job.ChildConversationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("child artifact scope bypassed origin revocation %v", err)
	}
}

func TestWrenMultiplayerAtomicReplyRoutingHistoryAndRemovedReference(t *testing.T) {
	s, private, w, shared, _ := knowledgeFixture(t)
	ctx := context.Background()
	duplicate, err := s.SaveWren(ctx, private.WorkspaceID, private.AccountID, Wren{ID: "wren_duplicate_scout", Name: w.Name, Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var roomID string
	if err = s.pool.QueryRow(ctx, `SELECT id FROM wren_rooms WHERE conversation_id=$1`, shared.ID).Scan(&roomID); err != nil {
		t.Fatal(err)
	}
	room, err := s.Room(ctx, private.WorkspaceID, private.AccountID, roomID)
	if err != nil {
		t.Fatal(err)
	}
	room.WrenIDs = append(room.WrenIDs, duplicate.ID)
	if _, err = s.SaveRoom(ctx, private.WorkspaceID, private.AccountID, room, false); err != nil {
		t.Fatal(err)
	}
	initial, err := s.EnqueueOperator(ctx, shared, "reply-initial-request", "Initial question", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET speaker=$2,status='completed',completed_at=now() WHERE id=$1`, initial.ID, duplicate); err != nil {
		t.Fatal(err)
	}
	targetID := "msg_reply_exact_target"
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_messages(id,conversation_id,run_id,role,content) VALUES($1,$2,$3,'assistant','The specific earlier answer.')`, targetID, shared.ID, initial.ID); err != nil {
		t.Fatal(err)
	}
	reply, err := s.enqueueOperatorWithRecipientsAndReply(ctx, shared, "reply-stable-request", "@team @Scout clarify this", "test", OperatorModelOptions{}, nil, []string{w.ID}, false, targetID)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Speaker == nil || reply.Speaker.ID != duplicate.ID {
		t.Fatalf("reply name/default routed wrong %+v", reply)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE id=$1 OR turn_id=$1`, reply.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reply broadcast %d %v", count, err)
	}
	replay, err := s.enqueueOperatorWithRecipientsAndReply(ctx, shared, "reply-stable-request", "@team @Scout clarify this", "test", OperatorModelOptions{}, nil, nil, false, targetID)
	if err != nil || replay.ID != reply.ID {
		t.Fatalf("reply retry %+v %v", replay, err)
	}
	if _, err = s.enqueueOperatorWithRecipientsAndReply(ctx, shared, "reply-stable-request", "changed target", "test", OperatorModelOptions{}, nil, nil, false, "msg_other_target"); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay changed ancestry %v", err)
	}
	page, err := s.OperatorMessages(ctx, shared.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range page.Messages {
		if m.RunID == reply.ID && m.Role == "user" {
			found = true
			if m.ReplyTo == nil || m.ReplyTo.MessageID != targetID || m.ReplyTo.WrenID != duplicate.ID || !m.ReplyTo.CanWake {
				t.Fatalf("reply history metadata %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("reply human message missing")
	}
	contextText, err := s.ReplyRunContext(ctx, shared, reply)
	if err != nil || !strings.Contains(contextText, targetID) || !strings.Contains(contextText, "specific earlier answer") {
		t.Fatalf("reply model context %s %v", contextText, err)
	}
	// Participant removal after work finishes retains the reference but cannot
	// silently route a new reply to a similarly named or default Wren.
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET status='completed',completed_at=now() WHERE id=$1`, reply.ID); err != nil {
		t.Fatal(err)
	}
	room.WrenIDs = []string{w.ID}
	if _, err = s.SaveRoom(ctx, private.WorkspaceID, private.AccountID, room, false); err != nil {
		t.Fatal(err)
	}
	historical, err := s.WrenReplyTarget(ctx, private.WorkspaceID, private.AccountID, shared.ID, targetID)
	if err != nil || historical.CanWake || historical.WrenID != duplicate.ID {
		t.Fatalf("historical reference %+v %v", historical, err)
	}
	if _, err = s.enqueueOperatorWithRecipientsAndReply(ctx, shared, "reply-removed-wake", "Continue", "test", OperatorModelOptions{}, nil, nil, false, targetID); !errors.Is(err, ErrConflict) {
		t.Fatalf("removed bot wake not rejected %v", err)
	}
	posted, err := s.enqueueOperatorWithRecipientsAndReply(ctx, shared, "reply-people-only", "A reference for the people here", "", OperatorModelOptions{}, nil, nil, true, targetID)
	if err != nil || posted.Status != "completed" || posted.Model != "" || posted.Speaker != nil {
		t.Fatalf("people-only reply woke bot %+v %v", posted, err)
	}
	page, err = s.OperatorMessages(ctx, shared.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range page.Messages {
		if m.RunID == posted.ID && (m.ReplyTo == nil || m.ReplyTo.CanWake || m.ReplyTo.MessageID != targetID) {
			t.Fatalf("historical reply ancestry lost %+v", m)
		}
	}
}
func TestWrenMultiplayerRoomOwnershipTransferRequiresAudience(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	var roomID string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM wren_rooms WHERE conversation_id=$1`, shared.ID).Scan(&roomID); err != nil {
		t.Fatal(err)
	}
	transferred, err := s.TransferWrenRoomOwnership(ctx, private.WorkspaceID, private.AccountID, roomID, member)
	if err != nil || transferred.OwnerID != member {
		t.Fatalf("creator transfer %+v %v", transferred, err)
	}
	// Workspace ownership cannot override a valid owner, but can recover an
	// orphan only when already included in this private room's audience.
	if _, err = s.TransferWrenRoomOwnership(ctx, private.WorkspaceID, private.AccountID, roomID, private.AccountID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("active owner override %v", err)
	}
	if err = s.UpdateWorkspaceMember(ctx, private.WorkspaceID, private.AccountID, member, "", true); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.TransferWrenRoomOwnership(ctx, private.WorkspaceID, private.AccountID, roomID, private.AccountID)
	if err != nil || recovered.OwnerID != private.AccountID {
		t.Fatalf("orphan management %+v %v", recovered, err)
	}

	operator := multiplayerPerson(t, s, "private-group-creator@example.com")
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, operator.ID, private.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	ownWren, err := s.SaveWren(ctx, private.WorkspaceID, operator.ID, Wren{ID: "wren_private_group_creator", Name: "Private", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	privateGroup, err := s.SaveRoom(ctx, private.WorkspaceID, operator.ID, WrenRoom{ID: "room_owner_no_access", Kind: "group", Name: "Private members", WrenIDs: []string{ownWren.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransferWrenRoomOwnership(ctx, private.WorkspaceID, private.AccountID, privateGroup.ID, private.AccountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("workspace owner bypassed private group %v", err)
	}
	if _, err = s.TransferWrenRoomOwnership(ctx, private.WorkspaceID, operator.ID, privateGroup.ID, private.AccountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("transfer granted nonmember private access %v", err)
	}
}

func TestWrenMultiplayerHTTPInviteJoinReplyReadSearchAndRevoke(t *testing.T) {
	s, base, ownerToken := operatorFixture(t)
	ctx := context.Background()
	member := multiplayerPerson(t, s, "http-join@example.com")
	_, _, memberToken, err := s.Signin(ctx, member.Email, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.SaveWren(ctx, base.WorkspaceID, base.AccountID, Wren{ID: "wren_multiplayer_http", Name: "Scout", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	room, err := s.SaveRoom(ctx, base.WorkspaceID, base.AccountID, WrenRoom{ID: "room_multiplayer_http", Kind: "group", Name: "Launch", WrenIDs: []string{w.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Auth: AuthConfig{Mailer: &authTestMailer{}}, Operator: OperatorConfig{APIKey: "test", Model: "test", BaseURL: "http://unused"}}).(*HTTPServer)
	request := func(method, path, session string, data any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(data)
		r := httptest.NewRequest(method, "http://canter.test/v1"+path, bytes.NewReader(body))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		if session != "" {
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: session})
		}
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out
	}
	prefix := "/workspaces/" + base.WorkspaceID
	input := WorkspaceInviteInput{ID: "winvite_http_join", Email: member.Email, Role: "operator", SendEmail: true}
	got := request("POST", prefix+"/wren-members/invites", ownerToken, input)
	if got.Code != 200 {
		t.Fatalf("invite HTTP %d %s", got.Code, got.Body)
	}
	var issued struct {
		Invite WorkspaceInvite `json:"invite"`
		Token  string          `json:"token"`
	}
	if err = json.Unmarshal(got.Body.Bytes(), &issued); err != nil || issued.Token == "" {
		t.Fatalf("issued link %v", err)
	}
	got = request("POST", "/workspace-invites", memberToken, map[string]string{"token": issued.Token, "action": "accept"})
	if got.Code != 200 {
		t.Fatalf("join %d %s", got.Code, got.Body)
	}
	got = request("GET", prefix+"/wren-members", memberToken, nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), member.Email) {
		t.Fatalf("member roster %d %s", got.Code, got.Body)
	}
	got = request("GET", prefix+"/conversations/"+w.ConversationID, memberToken, nil)
	if got.Code != 404 {
		t.Fatalf("join leaked private DM %d %s", got.Code, got.Body)
	}
	room.MemberIDs = []string{member.ID}
	if _, err = s.SaveRoom(ctx, base.WorkspaceID, base.AccountID, room, false); err != nil {
		t.Fatal(err)
	}
	conversationPath := prefix + "/conversations/" + room.ConversationID
	got = request("POST", conversationPath+"/messages", ownerToken, map[string]string{"requestId": "http-shared-source", "message": "Shared kumquat research"})
	if got.Code != 202 {
		t.Fatalf("source post %d %s", got.Code, got.Body)
	}
	var source OperatorRun
	if err = json.Unmarshal(got.Body.Bytes(), &source); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET status='completed',completed_at=now() WHERE id=$1`, source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_messages(id,conversation_id,run_id,role,content) VALUES('msg_http_reply_target',$1,$2,'assistant','The kumquat research result.')`, room.ConversationID, source.ID); err != nil {
		t.Fatal(err)
	}
	got = request("POST", conversationPath+"/messages", memberToken, map[string]string{"requestId": "http-shared-reply", "message": "Clarify that result", "replyToMessageId": "msg_http_reply_target"})
	if got.Code != 202 {
		t.Fatalf("reply post %d %s", got.Code, got.Body)
	}
	var reply OperatorRun
	if err = json.Unmarshal(got.Body.Bytes(), &reply); err != nil || reply.Speaker == nil || reply.Speaker.ID != w.ID {
		t.Fatalf("routed reply %+v %v", reply, err)
	}
	got = request("GET", conversationPath, memberToken, nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"replyTo"`) || !strings.Contains(got.Body.String(), "msg_http_reply_target") {
		t.Fatalf("reply ancestry GET %d %s", got.Code, got.Body)
	}
	got = request("GET", prefix+"/wren-read?conversationId="+room.ConversationID, memberToken, nil)
	if got.Code != 200 {
		t.Fatalf("read GET %d %s", got.Code, got.Body)
	}
	var cursor WrenReadCursor
	if err = json.Unmarshal(got.Body.Bytes(), &cursor); err != nil {
		t.Fatal(err)
	}
	got = request("POST", prefix+"/wren-read", memberToken, map[string]any{"conversationId": room.ConversationID, "sequence": cursor.LatestSequence})
	if got.Code != 200 {
		t.Fatalf("read POST %d %s", got.Code, got.Body)
	}
	got = request("GET", prefix+"/wren-search?q=kumquat", memberToken, nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), "msg_http_reply_target") {
		t.Fatalf("scoped search %d %s", got.Code, got.Body)
	}
	got = request("PATCH", prefix+"/wren-members/"+member.ID, ownerToken, map[string]string{"role": "viewer"})
	if got.Code != 200 {
		t.Fatalf("downgrade %d %s", got.Code, got.Body)
	}
	got = request("POST", conversationPath+"/messages", memberToken, map[string]string{"requestId": "http-viewer-denied", "message": "Do more work"})
	if got.Code != 403 {
		t.Fatalf("viewer posted %d %s", got.Code, got.Body)
	}
	got = request("GET", conversationPath, memberToken, nil)
	if got.Code != 200 {
		t.Fatalf("viewer lost reading %d %s", got.Code, got.Body)
	}
	got = request("DELETE", prefix+"/wren-members/"+member.ID, ownerToken, nil)
	if got.Code != 200 {
		t.Fatalf("remove %d %s", got.Code, got.Body)
	}
	got = request("GET", conversationPath, memberToken, nil)
	if got.Code != 403 && got.Code != 404 {
		t.Fatalf("revoked still reads %d %s", got.Code, got.Body)
	}
	got = request("POST", "/workspace-invites", memberToken, map[string]string{"token": issued.Token, "action": "accept"})
	if got.Code != 409 {
		t.Fatalf("consumed token restored member %d %s", got.Code, got.Body)
	}
}
