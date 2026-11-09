package controlplane

import (
	"context"
	"encoding/json"
	"testing"
)

func TestWrenStandingToolsHumanScopeAndAutomatedDenial(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{}).(*HTTPServer)
	o := OperatorRuntime{Server: h}
	run, err := s.EnqueueOperator(ctx, c, "standing-human", "Remember that I prefer concise updates, and check weekly.", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || claimed.ID != run.ID {
		t.Fatalf("claim current human run: %v", err)
	}
	run = claimed
	raw := json.RawMessage(`{"title":"Update preference","content":"Use concise updates.","operationKey":"standing-memory"}`)
	v, handled, err := o.wrenStandingTool(ctx, run, c, "canter_save_memory", raw)
	if err != nil || !handled {
		t.Fatalf("save %v %v", handled, err)
	}
	memory := v.(WrenMemory)
	if memory.Scope != "private" || memory.WrenID != w.ID {
		t.Fatalf("scope %+v", memory)
	}
	var linked bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_events WHERE run_id=$1 AND kind='surface' AND data->>'kind'='wren-memory' AND data->>'id'=$2)`, run.ID, memory.ID).Scan(&linked); err != nil || !linked {
		t.Fatalf("saved memory is not linked to its actual turn: %v", err)
	}
	again, _, err := o.wrenStandingTool(ctx, run, c, "canter_save_memory", raw)
	if err != nil || again.(WrenMemory).ID != memory.ID {
		t.Fatalf("replay %v", err)
	}
	schedule := json.RawMessage(`{"name":"Weekly check","instruction":"Check current authorized sources.","schedule":{"kind":"interval","everySeconds":604800,"timeZone":"America/Toronto"},"state":"active","operationKey":"standing-routine"}`)
	v, _, err = o.wrenStandingTool(ctx, run, c, "canter_save_responsibility", schedule)
	if err != nil {
		t.Fatal(err)
	}
	r := v.(WrenResponsibility)
	if r.WrenID != w.ID || r.ConversationID != c.ID {
		t.Fatalf("wrong owner %+v", r)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET origin_kind='responsibility',responsibility_id=$2 WHERE id=$1`, run.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]json.RawMessage{"canter_save_memory": raw, "canter_save_responsibility": schedule, "canter_forget_memory": json.RawMessage(`{"id":"` + memory.ID + `","expectedRevision":1}`)} {
		if _, _, err = o.wrenStandingTool(ctx, run, c, name, body); err == nil {
			t.Fatalf("automated wake changed standing scope with %s", name)
		}
	}
	if _, _, err = o.wrenStandingTool(ctx, run, c, "canter_list_memory", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("read denied %v", err)
	}
}
