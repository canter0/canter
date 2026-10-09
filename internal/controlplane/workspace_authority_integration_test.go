package controlplane

import (
	"context"
	"net/http"
	"testing"

	"github.com/canter0/canter/sdk"
)

func TestViewerCannotDraftSystemChangeOverHTTP(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	account, workspace, sessionToken, err := store.Signup(ctx, "change-viewer@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	system, err := sdk.NewSystem("viewer-api", "Serve the viewer permission test").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/viewer-api").
		Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (&Service{Store: store}).PutSystem(ctx, workspace.ID, system); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE account_id=$1 AND workspace_id=$2`, account.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}

	handler := NewHTTPServer(&Service{Store: store}, HTTPConfig{PublicURL: "http://canter.test"})
	path := "/v1/workspaces/" + workspace.ID + "/systems/" + system.Metadata.Name + "/changes"
	response := requestJSON(t, handler, http.MethodPost, path, sdk.ChangeRequest{}, &http.Cookie{Name: "canter_session", Value: sessionToken})
	if response.Code != http.StatusForbidden {
		t.Fatalf("viewer drafted a System change: %d %s", response.Code, response.Body.String())
	}
}
