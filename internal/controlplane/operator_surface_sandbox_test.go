package controlplane

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSandboxConversationSurfaceProjection(t *testing.T) {
	t.Run("list opens the sandboxes dashboard", func(t *testing.T) {
		surface, ok := sandboxResultSurface("canter_sandbox_list", map[string]any{"sandboxes": []Sandbox{}}, "ws-1")
		if !ok || surface.Kind != "sandboxes" || surface.ID != "" {
			t.Fatalf("expected {kind:sandboxes}, got %+v ok=%v", surface, ok)
		}
	})
	t.Run("created sandbox opens its work surface", func(t *testing.T) {
		surface, ok := sandboxResultSurface("canter_sandbox_create", Sandbox{ID: "sbx_1", WorkspaceID: "ws-1"}, "ws-1")
		if !ok || surface.Kind != "sandbox" || surface.ID != "sbx_1" {
			t.Fatalf("expected {kind:sandbox id:sbx_1}, got %+v ok=%v", surface, ok)
		}
	})
	t.Run("pointer result projects the same way", func(t *testing.T) {
		surface, ok := sandboxResultSurface("canter_sandbox_update", &Sandbox{ID: "sbx_2", WorkspaceID: "ws-1"}, "ws-1")
		if !ok || surface.Kind != "sandbox" || surface.ID != "sbx_2" {
			t.Fatalf("expected {kind:sandbox id:sbx_2}, got %+v ok=%v", surface, ok)
		}
	})
	t.Run("cross workspace results never surface", func(t *testing.T) {
		if _, ok := sandboxResultSurface("canter_sandbox_update", Sandbox{ID: "sbx_1", WorkspaceID: "ws-other"}, "ws-1"); ok {
			t.Fatal("surfaced a sandbox from another workspace")
		}
	})
	t.Run("empty id never surfaces", func(t *testing.T) {
		if _, ok := sandboxResultSurface("canter_sandbox_create", Sandbox{WorkspaceID: "ws-1"}, "ws-1"); ok {
			t.Fatal("surfaced a sandbox without an id")
		}
	})
	t.Run("non sandbox results project nothing", func(t *testing.T) {
		for _, value := range []any{
			map[string]any{"sleeping": true},
			map[string]any{"jobId": "job_1"},
			nil,
			"sbx_1",
		} {
			if _, ok := sandboxResultSurface("canter_sandbox_sleep", value, "ws-1"); ok {
				t.Fatalf("surfaced %T: %v", value, value)
			}
		}
	})
}

func TestSandboxConversationSurfaceValidation(t *testing.T) {
	t.Run("list surface needs no id", func(t *testing.T) {
		if err := validateOperatorSurface(&OperatorSurface{Kind: "sandboxes"}); err != nil {
			t.Fatalf("sandboxes surface rejected: %v", err)
		}
	})
	t.Run("sandbox surface requires an id", func(t *testing.T) {
		if err := validateOperatorSurface(&OperatorSurface{Kind: "sandbox"}); err == nil {
			t.Fatal("sandbox surface without id accepted")
		}
	})
	t.Run("sandbox surface accepts a bounded id", func(t *testing.T) {
		if err := validateOperatorSurface(&OperatorSurface{Kind: "sandbox", ID: "sbx_1"}); err != nil {
			t.Fatalf("sandbox surface rejected: %v", err)
		}
	})
	t.Run("oversized id rejected", func(t *testing.T) {
		long := make([]byte, 161)
		for i := range long {
			long[i] = 'x'
		}
		if err := validateOperatorSurface(&OperatorSurface{Kind: "sandbox", ID: string(long)}); err == nil {
			t.Fatal("sandbox surface with oversized id accepted")
		}
	})
}

// TestSandboxConversationSurfaceDispatch exercises the real localTool path:
// a successful sandbox list projects the dashboard and a successful update
// projects the sandbox work surface, both as persisted operator events.
func TestSandboxConversationSurfaceDispatch(t *testing.T) {
	fixture, _ := superSandboxFixture(t)
	s := fixture.service.Store
	c, err := s.CreateConversation(context.Background(), "ws", "owner", "conv_test", "Check my workspace")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "https://canter.test"}).(*HTTPServer)
	o := &OperatorRuntime{Server: h}
	if _, err := s.EnqueueOperator(ctx, c, "sandbox-surface-req", "list my sandboxes", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	p, err := s.operatorPrincipal(ctx, c, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if role, err := workspaceRole(ctx, s.pool, c.WorkspaceID, c.AccountID, false); err != nil || role != "owner" {
		t.Fatalf("fixture membership role changed: %q %v", role, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sandboxes(id,workspace_id,name,size,tier,phase,plan,sleep_requested) VALUES('sbx_surface_1',$1,'box','free-1x1','free','ready','{}',false)`, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	surfaceKinds := func() []string {
		events, err := s.OperatorEvents(ctx, c.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, event := range events {
			if event.Kind != "surface" {
				continue
			}
			var data struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			out = append(out, data.Kind+":"+data.ID)
		}
		return out
	}
	if value, handled, err := o.localTool(ctx, run, c, p, "canter_sandbox_list", json.RawMessage(`{}`)); err != nil || !handled {
		t.Fatalf("list: %v %v", handled, err)
	} else if _, ok := value.(map[string]any); !ok {
		t.Fatalf("list returned %T", value)
	}
	if value, handled, err := o.localTool(ctx, run, c, p, "canter_sandbox_update", json.RawMessage(`{"id":"sbx_surface_1","labels":{"review":"yes"}}`)); err != nil || !handled {
		t.Fatalf("update: %v %v", handled, err)
	} else if box, ok := value.(Sandbox); !ok || box.ID != "sbx_surface_1" {
		t.Fatalf("update returned %T %v", value, value)
	}
	got := surfaceKinds()
	want := []string{"sandboxes:", "sandbox:sbx_surface_1"}
	if len(got) != len(want) {
		t.Fatalf("surface events %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("surface events %v, want %v", got, want)
		}
	}
}
