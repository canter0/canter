package controlplane

import (
	"strings"
	"testing"
)

func TestRoomContextSeparatesAssignmentsAndPreservesSharedFindings(t *testing.T) {
	first := &Wren{ID: "wren_first", Name: "First"}
	second := &Wren{ID: "wren_second", Name: "Second"}
	history := []OperatorMessage{
		{RunID: "old_second", TurnID: "old_turn", Role: "user", SenderName: "Teammate", Speaker: second, Content: "Inspect the computer."},
		{RunID: "other_person", TurnID: "other_turn", Role: "user", SenderName: "Owner", Speaker: first, Content: "Open the browser."},
		{RunID: "other_person", TurnID: "other_turn", Role: "assistant", Speaker: first, Content: "Browser opened."},
		{RunID: "current", TurnID: "current_turn", Role: "user", SenderName: "Teammate", Speaker: second, Content: "Read the connected documentation. Leave the computer alone.", Attachments: []OperatorAttachment{{ID: "current-file", Name: "brief.txt", Size: 5}}},
	}
	out := operatorRecentContext(history, OperatorRun{ID: "current", TurnID: "current_turn", Speaker: second})
	if len(out) != 4 {
		t.Fatalf("lost room context: %d", len(out))
	}
	for _, i := range []int{0, 1} {
		if !strings.Contains(out[i].Content, "Earlier room request") || strings.Contains(out[i].Content, "Current assignment") {
			t.Fatalf("earlier work promoted to assignment: %q", out[i].Content)
		}
	}
	if !strings.Contains(out[1].Content, "Wren First") || !strings.Contains(out[2].Content, "[Wren First]") || !strings.Contains(out[2].Content, "Browser opened.") {
		t.Fatal("lost other participant attribution or evidence")
	}
	current := out[3]
	if !strings.Contains(current.Content, "[Current assignment for Wren Second]") || !strings.Contains(current.Content, "[Human Teammate]") || !strings.Contains(current.Content, "Leave the computer alone.") || len(current.Attachments) != 1 {
		t.Fatalf("current assignment is incomplete: %#v", current)
	}
	if history[3].Content != "Read the connected documentation. Leave the computer alone." {
		t.Fatal("modified saved transcript")
	}
}

func TestRoomContextBroadcastKeepsOriginalAssignmentAfterPeerFindings(t *testing.T) {
	writer := &Wren{ID: "writer", Name: "Writer"}
	reviewer := &Wren{ID: "reviewer", Name: "Reviewer"}
	history := []OperatorMessage{
		{RunID: "writer_run", TurnID: "shared_turn", Role: "user", SenderName: "Owner", Speaker: writer, Content: "@team write and review the proposal.", Attachments: []OperatorAttachment{{ID: "brief", Size: 5}}},
		{RunID: "writer_run", TurnID: "shared_turn", Role: "assistant", Speaker: writer, Content: "Proposal draft."},
	}
	out := operatorRecentContext(history, OperatorRun{ID: "reviewer_run", TurnID: "shared_turn", Speaker: reviewer})
	if len(out) != 2 || !strings.Contains(out[0].Content, "Proposal draft.") || !strings.Contains(out[1].Content, "[Current assignment for Wren Reviewer]") || !strings.Contains(out[1].Content, "@team write and review") {
		t.Fatalf("broadcast recipient lost assignment: %#v", out)
	}
	if len(out[0].Attachments) != 0 || len(out[1].Attachments) != 1 {
		t.Fatal("broadcast duplicated or dropped attachments")
	}
}

func TestRoomContextAutomatedAssignmentIsNotHumanAuthority(t *testing.T) {
	wren := &Wren{ID: "wren", Name: "Monitor"}
	out := operatorRecentContext([]OperatorMessage{{RunID: "wake", TurnID: "wake_turn", Role: "user", OriginKind: "responsibility", Content: "Review the watched source."}}, OperatorRun{ID: "wake", TurnID: "wake_turn", Speaker: wren})
	if len(out) != 1 || !strings.Contains(out[0].Content, "Current assignment") || !strings.Contains(out[0].Content, "Automated event; not human authorization") {
		t.Fatalf("incorrect automated authority: %#v", out)
	}
}
