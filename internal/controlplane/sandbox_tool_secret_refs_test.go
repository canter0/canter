package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"testing"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
)

func TestSandboxToolSecretRefs(t *testing.T) {
	for _, tool := range []string{"canter_sandbox_run", "canter_sandbox_start"} {
		t.Run(tool, func(t *testing.T) {
			f := sandboxFixture(t)
			f.enable(t)
			ctx := context.Background()
			for id, value := range map[string]string{"sec_tool1": "first-value", "sec_tool2": "second-value"} {
				key, sealed, err := f.h.config.Secrets.seal([]byte(value), secretBinding(f.space.ID, id, "stored"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_secrets(id,workspace_id,name,purpose,note,ciphertext,key_id,updated_by,sandbox_allowed) VALUES($1,$2,$3,'stored','n',$4,$5,$6,true)`, id, f.space.ID, id, sealed, key, f.account.ID); err != nil {
					t.Fatal(err)
				}
			}
			created := environmentPost(t, f, map[string]any{
				"name": "toolrefs", "size": "free-1x1",
				"environment": map[string]any{
					"env":        map[string]string{"BASE": "public"},
					"secretRefs": map[string]string{"API_TOKEN": "sec_tool1"},
				},
			}, http.StatusAccepted)
			sandbox, err := f.s.Sandbox(ctx, f.space.ID, created["id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			sandbox = f.ready(t, sandbox)
			if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET setup='{}' WHERE id=$1`, sandbox.ID); err != nil {
				t.Fatal(err)
			}
			var capturedEnv map[string]string
			var starts int
			dial := f.h.sandboxDial
			f.h.sandboxDial = func(config wrencomputer.Config) (sandboxAgentClient, error) {
				client, err := dial(config)
				if err != nil {
					return nil, err
				}
				client.(*sandboxFakeClient).startHook = func(_ context.Context, req wrencomputer.JobRequest) (wrencomputer.Job, error) {
					starts++
					capturedEnv = maps.Clone(req.Env)
					exit := 0
					return wrencomputer.Job{ID: req.ID, Status: "completed", ExitCode: &exit}, nil
				}
				return client, nil
			}
			jobCount := func(t *testing.T) int {
				t.Helper()
				var count int
				if err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_jobs WHERE sandbox_id=$1`, sandbox.ID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				return count
			}
			for _, tc := range []struct {
				name       string
				refs       map[string]string
				revoked    bool
				wantDenied bool
				wantEnv    map[string]string
			}{
				{name: "inherit", wantEnv: map[string]string{"BASE": "public", "API_TOKEN": "first-value"}},
				{name: "clear", refs: map[string]string{}, wantEnv: map[string]string{"BASE": "public"}},
				{name: "replace", refs: map[string]string{"OTHER": "sec_tool2"}, wantEnv: map[string]string{"BASE": "public", "OTHER": "second-value"}},
				{name: "revoked-inherit", revoked: true, wantDenied: true},
				{name: "revoked-explicit-clear", refs: map[string]string{}, revoked: true, wantEnv: map[string]string{"BASE": "public"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if tc.revoked {
						if _, err := f.s.pool.Exec(ctx, `UPDATE workspace_secrets SET sandbox_allowed=false WHERE workspace_id=$1 AND id='sec_tool1'`, f.space.ID); err != nil {
							t.Fatal(err)
						}
					}
					args := map[string]any{"id": sandbox.ID, "command": "true"}
					if tc.refs != nil {
						args["secretRefs"] = tc.refs
					}
					raw, err := json.Marshal(args)
					if err != nil {
						t.Fatal(err)
					}
					beforeStarts, beforeJobs := starts, jobCount(t)
					_, err = f.h.callSandboxTool(ctx, f.space.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, true, tool, raw)
					if tc.wantDenied {
						if !errors.Is(err, ErrForbidden) {
							t.Fatalf("revoked inherited reference: got %v, want ErrForbidden", err)
						}
						if starts != beforeStarts || jobCount(t) != beforeJobs {
							t.Fatal("forbidden reference started a guest job or created a durable job")
						}
						return
					}
					if err != nil {
						t.Fatalf("tool call failed: %v", err)
					}
					if starts != beforeStarts+1 || jobCount(t) != beforeJobs+1 {
						t.Fatal("successful tool call must start exactly one durable guest job")
					}
					if !maps.Equal(capturedEnv, tc.wantEnv) {
						t.Fatalf("guest environment = %v, want %v", capturedEnv, tc.wantEnv)
					}
				})
			}
		})
	}
}
