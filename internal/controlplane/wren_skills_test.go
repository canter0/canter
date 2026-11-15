package controlplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func skillInput() WrenSkillInput {
	return WrenSkillInput{Name: "Research and report", Description: "A reusable source-checking workflow, visible to this workspace.", Instructions: "Read the requested current sources. Return a sourced CSV and explain uncertainty.", RequiredCapabilities: []string{"canter_computer_read", "canter_publish_artifact"}}
}
func TestWrenSkillVersionPinRollbackDisableUninstall(t *testing.T) {
	s, private, w, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	skill, err := s.SaveWrenSkill(ctx, private.WorkspaceID, private.AccountID, skillInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.WrenSkill(ctx, private.WorkspaceID, member, skill.ID, 0); err != nil {
		t.Fatalf("intentional workspace catalog inaccessible %v", err)
	}
	input := WrenSkillInstallationInput{WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: private.ID, Scope: "private", WrenID: w.ID}, SkillID: skill.ID, Version: skill.Version, Enabled: true}
	installed, err := s.InstallWrenSkill(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil || installed.Revision != 1 {
		t.Fatalf("install %+v %v", installed, err)
	}
	if items, err := s.WrenSkillInstallations(ctx, private.WorkspaceID, member, WrenKnowledgeScope{ConversationID: shared.ID, Scope: "room"}); err != nil || len(items) != 0 {
		t.Fatalf("private installation imported %+v %v", items, err)
	}
	edit := skillInput()
	edit.ID = skill.ID
	edit.ExpectedVersion = skill.Version
	edit.Instructions = "Version two: write the human-approved output columns."
	skill, err = s.SaveWrenSkill(ctx, private.WorkspaceID, private.AccountID, edit)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.WrenSkillInstallations(ctx, private.WorkspaceID, private.AccountID, input.WrenKnowledgeScope)
	if err != nil || got[0].Skill.Version != 1 || got[0].Skill.Instructions == skill.Instructions {
		t.Fatalf("pinned install silently upgraded %+v %v", got, err)
	}
	input.ExpectedRevision = installed.Revision
	input.Version = 2
	installed, err = s.InstallWrenSkill(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil || installed.Skill.Version != 2 {
		t.Fatalf("upgrade %+v %v", installed, err)
	}
	input.ExpectedRevision = installed.Revision
	input.Version = 1
	installed, err = s.InstallWrenSkill(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil || installed.Skill.Version != 1 {
		t.Fatalf("rollback %+v %v", installed, err)
	}
	if _, err = s.InstallWrenSkill(ctx, private.WorkspaceID, private.AccountID, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale pin accepted %v", err)
	}
	root, err := s.EnqueueOperator(ctx, private, "skill-human-root", "Use the research workflow", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.SkillContext(ctx, private, root)
	if err != nil || !strings.Contains(text, "Read the requested current sources") || strings.Contains(text, "Version two:") {
		t.Fatalf("pinned context %s %v", text, err)
	}
	caps, err := s.WrenSkillAvailability(ctx, private.WorkspaceID, private.AccountID, skill.ID, 1, []string{"canter_computer_read"})
	if err != nil || len(caps) != 2 || !caps[0].Available || caps[1].Available || caps[1].Reason == "" {
		t.Fatalf("dependency diagnostic %+v %v", caps, err)
	}
	edit.ExpectedVersion = skill.Version
	edit.Disabled = true
	skill, err = s.SaveWrenSkill(ctx, private.WorkspaceID, private.AccountID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if text, err = s.SkillContext(ctx, private, root); err != nil || text != "" {
		t.Fatalf("global disable failed %s %v", text, err)
	}
	input.ExpectedRevision = installed.Revision
	if _, err = s.InstallWrenSkill(ctx, private.WorkspaceID, private.AccountID, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("disabled catalog installed %v", err)
	}
	input.Enabled = false
	installed, err = s.InstallWrenSkill(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil || installed.Enabled {
		t.Fatalf("disable local %+v %v", installed, err)
	}
	if err = s.UninstallWrenSkill(ctx, private.WorkspaceID, private.AccountID, input.WrenKnowledgeScope, skill.ID, installed.Revision-1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale uninstall accepted %v", err)
	}
	if err = s.UninstallWrenSkill(ctx, private.WorkspaceID, private.AccountID, input.WrenKnowledgeScope, skill.ID, installed.Revision); err != nil {
		t.Fatal(err)
	}
	got, err = s.WrenSkillInstallations(ctx, private.WorkspaceID, private.AccountID, input.WrenKnowledgeScope)
	if err != nil || len(got) != 0 {
		t.Fatalf("uninstall %+v %v", got, err)
	}
	versions, err := s.WrenSkillVersions(ctx, private.WorkspaceID, member, skill.ID)
	if err != nil || len(versions) != 3 || versions[2].Instructions != skillInput().Instructions {
		t.Fatalf("version history %+v %v", versions, err)
	}
}
func TestWrenSkillSharedScopeAuthorAndRevisionGuards(t *testing.T) {
	s, private, w, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	skill, err := s.SaveWrenSkill(ctx, private.WorkspaceID, private.AccountID, skillInput())
	if err != nil {
		t.Fatal(err)
	}
	edit := skillInput()
	edit.ID = skill.ID
	edit.ExpectedVersion = 1
	edit.Instructions = "Edited by another member"
	if _, err = s.SaveWrenSkill(ctx, private.WorkspaceID, member, edit); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other member changed catalog author %v", err)
	}
	if _, err = s.WrenSkill(ctx, "foreign-workspace", private.AccountID, skill.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross workspace catalog leaked %v", err)
	}
	result := make(chan error, 2)
	var group sync.WaitGroup
	for _, instruction := range []string{"Version A", "Version B"} {
		group.Add(1)
		go func(instruction string) {
			defer group.Done()
			v := edit
			v.Instructions = instruction
			_, e := s.SaveWrenSkill(ctx, private.WorkspaceID, private.AccountID, v)
			result <- e
		}(instruction)
	}
	group.Wait()
	close(result)
	success, conflict := 0, 0
	for e := range result {
		if e == nil {
			success++
		} else if errors.Is(e, ErrConflict) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("parallel version overwrite %d %d", success, conflict)
	}
	roomScope := WrenKnowledgeScope{ConversationID: shared.ID, Scope: "room"}
	installed, err := s.InstallWrenSkill(ctx, private.WorkspaceID, member, WrenSkillInstallationInput{WrenKnowledgeScope: roomScope, SkillID: skill.ID, Version: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if installed.OwnerID != "" || installed.AuthorID != member {
		t.Fatalf("shared attribution %+v", installed)
	}
	root, err := s.EnqueueOperator(ctx, shared, "skill-room-run", "Use shared installed workflow", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET speaker=$2 WHERE id=$1`, root.ID, w); err != nil {
		t.Fatal(err)
	}
	if text, err := s.SkillContext(ctx, shared, root); err != nil || !strings.Contains(text, skill.ID) {
		t.Fatalf("shared context %s %v", text, err)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM wren_room_wrens WHERE room_id=(SELECT id FROM wren_rooms WHERE conversation_id=$1) AND wren_id=$2`, shared.ID, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SkillContext(ctx, shared, root); !errors.Is(err, ErrForbidden) {
		t.Fatalf("removed participant used shared skill %v", err)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM memberships WHERE workspace_id=$1 AND account_id=$2`, private.WorkspaceID, member); err != nil {
		t.Fatal(err)
	}
	if _, err = s.WrenSkillInstallations(ctx, private.WorkspaceID, member, roomScope); err == nil {
		t.Fatal("revoked member read installations")
	}
	if _, err = s.WrenSkill(ctx, private.WorkspaceID, member, skill.ID, 0); err == nil {
		t.Fatal("revoked member read catalog")
	}
}
func TestWrenSkillValidation(t *testing.T) {
	base := skillInput()
	base.ID = "skill_validate"
	for _, mutate := range []func(*WrenSkillInput){func(v *WrenSkillInput) { v.Instructions = "" }, func(v *WrenSkillInput) { v.RequiredCapabilities = []string{"tool", "tool"} }, func(v *WrenSkillInput) { v.RequiredCapabilities = []string{"tool;rm"} }, func(v *WrenSkillInput) { v.Name = strings.Repeat("x", 101) }, func(v *WrenSkillInput) { v.ID = "skill_../../x" }} {
		v := base
		mutate(&v)
		if err := validateSkill(&v); err == nil {
			t.Fatalf("invalid skill accepted %+v", v)
		}
	}
}
