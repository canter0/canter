package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type ownedMessageProvider struct {
	hits      atomic.Int32
	mu        sync.Mutex
	envelopes []WrenMessageEnvelope
	keys      []string
	err       error
}

func (p *ownedMessageProvider) Send(_ context.Context, key, id string, v WrenMessageEnvelope) (WrenMessageProviderReceipt, error) {
	p.hits.Add(1)
	p.mu.Lock()
	p.envelopes = append(p.envelopes, v)
	p.keys = append(p.keys, id)
	p.mu.Unlock()
	if key != "owned-correspondence-key" {
		return WrenMessageProviderReceipt{}, errors.New("wrong correspondence identity")
	}
	if p.err != nil {
		return WrenMessageProviderReceipt{}, p.err
	}
	return WrenMessageProviderReceipt{ID: "owned-message-1", Status: "accepted"}, nil
}
func correspondenceFixture(t *testing.T) (*HTTPServer, Conversation, Conversation, string, WrenMessageConnection, *ownedMessageProvider) {
	t.Helper()
	s, private, _, room, member := knowledgeFixture(t)
	provider := &ownedMessageProvider{}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{Secrets: testVault(t), PublicURL: "http://canter.test", Messaging: WrenMessagingConfig{Provider: provider}}).(*HTTPServer)
	c, e := h.SaveWrenMessageConnection(context.Background(), private.WorkspaceID, private.AccountID, WrenMessageConnectionInput{ID: "mailconn_owned_test", WrenKnowledgeScope: WrenKnowledgeScope{Scope: "room", ConversationID: room.ID}, Name: "Clinic correspondence", From: "clinic@example.test", APIKey: "owned-correspondence-key", Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	return h, private, room, member, c, provider
}
func messageDraftInput(c WrenMessageConnection, key string) WrenMessageDraftInput {
	return WrenMessageDraftInput{ConnectionID: c.ID, InstructionKey: key, Content: WrenMessageContent{To: []string{"recipient@example.test"}, Subject: "Opening review", Text: "The fictional clinic opens Tuesday."}}
}
func TestWrenCorrespondenceEditSendIdentityAndPermanentReceipt(t *testing.T) {
	h, private, room, member, c, provider := correspondenceFixture(t)
	s := h.service.Store
	ctx := context.Background()
	d, e := s.SaveWrenMessageDraft(ctx, private.WorkspaceID, member, messageDraftInput(c, "human-proposal-1"), nil)
	if e != nil {
		t.Fatal(e)
	}
	// Seeing a draft in the same room does not grant use of its owner's identity.
	if _, e = h.ActWrenMessageDraft(ctx, private.WorkspaceID, member, d.ID, d.Revision, "send"); !errors.Is(e, ErrForbidden) || provider.hits.Load() != 0 {
		t.Fatalf("borrowed sending identity: %v", e)
	}
	edit := messageDraftInput(c, "human-proposal-1")
	edit.ID = d.ID
	edit.ExpectedRevision = d.Revision
	edit.Content.Text = "Human reviewed: Tuesday at 09:00."
	edit.Content.To = []string{"reviewed@example.test"}
	d, e = s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, edit, nil)
	if e != nil || d.Revision != 2 {
		t.Fatalf("human edit: %+v %v", d, e)
	}
	if _, e = s.SaveWrenMessageDraft(ctx, private.WorkspaceID, member, edit, nil); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale edit: %v", e)
	}
	// Only one of two simultaneous clicks may dispatch the frozen revision.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.ActWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, d.ID, d.Revision, "send")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	sent, e := s.WrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, d.ID)
	if e != nil || sent.State != "accepted" || sent.ProviderID != "owned-message-1" || provider.hits.Load() != 1 {
		t.Fatalf("send %+v hits%d %v", sent, provider.hits.Load(), e)
	}
	if sent.State == "delivered" {
		t.Fatal("provider acceptance misreported as delivery")
	}
	provider.mu.Lock()
	actual := provider.envelopes[0]
	provider.mu.Unlock()
	if actual.From != c.From || actual.To[0] != "reviewed@example.test" || actual.Text != "Human reviewed: Tuesday at 09:00." {
		t.Fatalf("sent unreviewed content %+v", actual)
	}
	if _, e = h.ActWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, d.ID, 1, "send"); e != nil || provider.hits.Load() != 1 {
		t.Fatalf("replayed completed send %v", e)
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "owned-correspondence-key") {
		t.Fatal("connection leaked credentials")
	}
	if _, e = s.WrenMessageDrafts(ctx, private.WorkspaceID, member, private.ID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("private scope leak %v", e)
	}
	if sent.ConversationID != room.ID {
		t.Fatal("draft changed destination")
	}
}
func TestWrenCorrespondenceRejectReplayUnknownAndRevocation(t *testing.T) {
	h, private, _, _, c, provider := correspondenceFixture(t)
	s := h.service.Store
	ctx := context.Background()
	in := messageDraftInput(c, "reject-once")
	d, e := s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, in, nil)
	if e != nil {
		t.Fatal(e)
	}
	rejected, e := h.ActWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, d.ID, d.Revision, "reject")
	if e != nil || rejected.State != "rejected" {
		t.Fatal(e)
	}
	retry, e := s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, in, nil)
	if e != nil || retry.ID != d.ID || retry.State != "rejected" {
		t.Fatalf("recreated rejected proposal %+v %v", retry, e)
	}
	if _, e = h.ActWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, d.ID, rejected.Revision, "send"); e != nil || provider.hits.Load() != 0 {
		t.Fatalf("sent rejection %v", e)
	}
	provider.err = errors.New("network outcome unavailable")
	d, e = s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, messageDraftInput(c, "unknown-once"), nil)
	if e != nil {
		t.Fatal(e)
	}
	unknown, e := h.ActWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, d.ID, d.Revision, "send")
	if e != nil || unknown.State != "unknown" {
		t.Fatalf("unknown lost %+v %v", unknown, e)
	}
	if _, e = h.ActWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, d.ID, unknown.Revision, "send"); e != nil || provider.hits.Load() != 1 {
		t.Fatalf("unknown resend %v", e)
	}
	pending, e := s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, messageDraftInput(c, "pending-revoke"), nil)
	if e != nil {
		t.Fatal(e)
	}
	c, e = h.SaveWrenMessageConnection(ctx, private.WorkspaceID, private.AccountID, WrenMessageConnectionInput{ID: c.ID, WrenKnowledgeScope: c.WrenKnowledgeScope, Name: c.Name, From: c.From, ExpectedRevision: c.Revision, Revoke: true})
	if e != nil || c.RevokedAt == nil {
		t.Fatal(e)
	}
	if _, e = h.ActWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, pending.ID, pending.Revision, "send"); !errors.Is(e, ErrForbidden) || provider.hits.Load() != 1 {
		t.Fatalf("revoked dispatch %v", e)
	}
	var ciphertext []byte
	if e = s.pool.QueryRow(ctx, `SELECT ciphertext FROM wren_message_connections WHERE id=$1`, c.ID).Scan(&ciphertext); e != nil || len(ciphertext) > 0 {
		t.Fatalf("revocation retained credentials %v", e)
	}
}
func TestWrenCorrespondenceAttachmentRevisionAndPrivateFence(t *testing.T) {
	h, private, room, _, c, provider := correspondenceFixture(t)
	s := h.service.Store
	ctx := context.Background()
	a, e := s.PutWrenArtifact(ctx, private.WorkspaceID, private.AccountID, WrenArtifactInput{ConversationID: room.ID, Name: "review.txt", MediaType: "text/plain", Content: []byte("Reviewed version one")})
	if e != nil {
		t.Fatal(e)
	}
	in := messageDraftInput(c, "attached-exact")
	in.Content.Attachments = []WrenMessageArtifactRef{{ID: a.ID, Revision: a.Revision, SHA256: a.SHA256}}
	d, e := s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, in, nil)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.PutWrenArtifact(ctx, private.WorkspaceID, private.AccountID, WrenArtifactInput{ID: a.ID, ExpectedRevision: a.Revision, ConversationID: room.ID, Name: a.Name, MediaType: a.MediaType, Content: []byte("Unreviewed version two")})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = h.ActWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, d.ID, d.Revision, "send"); e != nil {
		t.Fatal(e)
	}
	if provider.envelopes[0].Attachments[0].ContentBase64 != "UmV2aWV3ZWQgdmVyc2lvbiBvbmU=" {
		t.Fatal("sent a newer unreviewed file")
	}
	hidden, e := s.PutWrenArtifact(ctx, private.WorkspaceID, private.AccountID, WrenArtifactInput{ConversationID: private.ID, Name: "private.txt", MediaType: "text/plain", Content: []byte("private material")})
	if e != nil {
		t.Fatal(e)
	}
	in = messageDraftInput(c, "private-leak")
	in.Content.Attachments = []WrenMessageArtifactRef{{ID: hidden.ID, Revision: hidden.Revision, SHA256: hidden.SHA256}}
	if _, e = s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, in, nil); !errors.Is(e, ErrNotFound) {
		t.Fatalf("private attachment entered shared draft %v", e)
	}
}
func TestWrenCorrespondenceCrashMarkerNeverResent(t *testing.T) {
	h, private, _, _, c, provider := correspondenceFixture(t)
	s := h.service.Store
	ctx := context.Background()
	d, e := s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, messageDraftInput(c, "crash-before-receipt"), nil)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.pool.Exec(ctx, `UPDATE wren_message_drafts SET state='dispatching',revision=revision+1,dispatch_at=now()-interval '2 days' WHERE id=$1`, d.ID)
	if e != nil {
		t.Fatal(e)
	}
	got, e := h.ActWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, d.ID, d.Revision, "send")
	if e != nil || got.State != "dispatching" || provider.hits.Load() != 0 {
		t.Fatalf("expired provider dedupe marker replayed %+v %v", got, e)
	}
	// Reopen the actual test store, recover the stale marker, and verify that
	// later human retry still cannot acquire a second send.
	reopened, e := Open(ctx, os.Getenv("CANTER_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if e = reopened.RecoverWrenMessageDispatches(ctx); e != nil {
		t.Fatal(e)
	}
	got, e = h.ActWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, d.ID, d.Revision, "send")
	if e != nil || got.State != "unknown" || provider.hits.Load() != 0 {
		t.Fatalf("recovered marker replayed %+v %v", got, e)
	}
	var stored string
	if e = s.pool.QueryRow(ctx, `SELECT state FROM wren_message_drafts WHERE id=$1`, d.ID).Scan(&stored); e != nil || stored != "unknown" {
		t.Fatal(e)
	}
}

func TestWrenCorrespondenceModelProposalRejectAndCurrentLeaseFence(t *testing.T) {
	h, private, room, _, c, provider := correspondenceFixture(t)
	s := h.service.Store
	ctx := context.Background()
	_, e := s.EnqueueOperator(ctx, room, "draft-real-human-turn", "Prepare an email draft for me to review.", "test", nil)
	if e != nil {
		t.Fatal(e)
	}
	run, ok, e := s.claimOperator(ctx)
	if e != nil || !ok {
		t.Fatalf("claim %v %v", ok, e)
	}
	in := messageDraftInput(c, "model-first-operation")
	d, e := s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, in, &run)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = h.ActWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, d.ID, d.Revision, "reject"); e != nil {
		t.Fatal(e)
	}
	in.InstructionKey = "model-different-operation"
	in.Content.Text = "Another attempt without a new human instruction."
	got, e := s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, in, &run)
	if e != nil || got.ID != d.ID || got.State != "rejected" {
		t.Fatalf("model recreated rejected proposal %+v %v", got, e)
	}
	if e = s.finishOperator(ctx, run, "completed", "Draft rejected."); e != nil {
		t.Fatal(e)
	}
	if _, e = s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, messageDraftInput(c, "stale-lease"), &run); !errors.Is(e, ErrForbidden) {
		t.Fatalf("stale model created draft %v", e)
	}
	if provider.hits.Load() != 0 {
		t.Fatal("model proposal sent correspondence")
	}
}
func TestWrenCorrespondenceWriterDowngradeErasesSendingCredential(t *testing.T) {
	h, private, room, member, _, provider := correspondenceFixture(t)
	s := h.service.Store
	ctx := context.Background()
	c, e := h.SaveWrenMessageConnection(ctx, private.WorkspaceID, member, WrenMessageConnectionInput{WrenKnowledgeScope: WrenKnowledgeScope{Scope: "room", ConversationID: room.ID}, Name: "Member identity", From: "member@example.test", APIKey: "owned-correspondence-key", Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	d, e := s.SaveWrenMessageDraft(ctx, private.WorkspaceID, member, messageDraftInput(c, "before-downgrade"), nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.UpdateWorkspaceMember(ctx, private.WorkspaceID, private.AccountID, member, "viewer", false); e != nil {
		t.Fatal(e)
	}
	if _, e = h.ActWrenMessageDraft(ctx, private.WorkspaceID, member, d.ID, d.Revision, "send"); !errors.Is(e, ErrForbidden) {
		t.Fatalf("viewer sent %v", e)
	}
	if e = s.UpdateWorkspaceMember(ctx, private.WorkspaceID, private.AccountID, member, "operator", false); e != nil {
		t.Fatal(e)
	}
	if _, e = h.ActWrenMessageDraft(ctx, private.WorkspaceID, member, d.ID, d.Revision, "send"); !errors.Is(e, ErrForbidden) || provider.hits.Load() != 0 {
		t.Fatalf("regrant revived old sender %v", e)
	}
	var gone bool
	if e = s.pool.QueryRow(ctx, `SELECT ciphertext IS NULL AND revoked_at IS NOT NULL AND NOT enabled FROM wren_message_connections WHERE id=$1`, c.ID).Scan(&gone); e != nil || !gone {
		t.Fatalf("credential retained after downgrade %v", e)
	}
}

func TestWrenCorrespondenceStandingRejectedOrUnknownFencesLaterWakes(t *testing.T) {
	for _, outcome := range []string{"rejected", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			h, private, room, _, c, provider := correspondenceFixture(t)
			s := h.service.Store
			ctx := context.Background()
			var wren string
			if e := s.pool.QueryRow(ctx, `SELECT id FROM wrens WHERE conversation_id=$1`, private.ID).Scan(&wren); e != nil {
				t.Fatal(e)
			}
			input := responsibilityInput(room, Wren{ID: wren})
			input.Name = "Draft clinic correspondence"
			input.Instruction = "Read each incoming source and prepare an email draft for human review."
			input.Schedule = WrenResponsibilitySchedule{Kind: "event", EventName: "email_received", TimeZone: "UTC"}
			responsibility, e := s.SaveWrenResponsibility(ctx, private.WorkspaceID, private.AccountID, input)
			if e != nil {
				t.Fatal(e)
			}
			next := func(key string) OperatorRun {
				t.Helper()
				if _, e = s.TriggerWrenResponsibility(ctx, private.WorkspaceID, private.AccountID, responsibility.ID, key, "email_received", nil); e != nil {
					t.Fatal(e)
				}
				if _, e = s.DispatchWrenResponsibilities(ctx, "test", 8); e != nil {
					t.Fatal(e)
				}
				run, ok, e := s.claimOperator(ctx)
				if e != nil || !ok {
					t.Fatalf("claim %v %v", ok, e)
				}
				return run
			}
			first := next("received-first")
			d, e := s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, messageDraftInput(c, "first"), &first)
			if e != nil {
				t.Fatal(e)
			}
			action := "reject"
			wantCalls := int32(0)
			if outcome == "unknown" {
				action = "send"
				wantCalls = 1
				provider.err = errors.New("provider receipt lost")
			}
			if _, e = h.ActWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, d.ID, d.Revision, action); e != nil {
				t.Fatal(e)
			}
			if e = s.finishOperator(ctx, first, "completed", "The draft was rejected."); e != nil {
				t.Fatal(e)
			}
			second := next("received-second")
			blocked, e := s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, messageDraftInput(c, "another-operation"), &second)
			if e != nil || blocked.ID != d.ID || blocked.State != outcome {
				t.Fatalf("standing rejection retried %+v %v", blocked, e)
			}
			if e = s.finishOperator(ctx, second, "completed", "This instruction remains rejected."); e != nil {
				t.Fatal(e)
			}
			edited := responsibilityEdit(responsibility)
			edited.Instruction = "Prepare a new human-reviewed clinic email draft using the revised instruction."
			responsibility, e = s.SaveWrenResponsibility(ctx, private.WorkspaceID, private.AccountID, edited)
			if e != nil {
				t.Fatal(e)
			}
			third := next("received-third")
			fresh, e := s.SaveWrenMessageDraft(ctx, private.WorkspaceID, private.AccountID, messageDraftInput(c, "new-authorized-instruction"), &third)
			if e != nil || fresh.ID == d.ID || fresh.State != "draft" {
				t.Fatalf("revised instruction did not permit new proposal %+v %v", fresh, e)
			}
			if provider.hits.Load() != wantCalls {
				t.Fatal("standing proposal sent without a human Send action")
			}
		})
	}
}
