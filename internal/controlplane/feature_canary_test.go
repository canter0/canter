package controlplane

import (
	"context"
	"net/http"
	"testing"

	"github.com/canter0/canter/pricing"
	"github.com/canter0/canter/sdk"
)

func TestFeatureCanaryScopesAllFiveFlags(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	ctx := context.Background()
	canary := f.create(t, "canary", "free-1x1", http.StatusAccepted)
	outsider, other, cookie := workspaceSession(t, f.s, "canary-other@example.com")
	if err := f.s.EnableWorkspaceProduct(ctx, other.ID, "sandboxes", sdk.ActorRef{Kind: "human", ID: outsider.ID}); err != nil {
		t.Fatal(err)
	}
	response := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+other.ID+"/sandboxes",
		map[string]string{"name": "outside canary", "size": "free-1x1"}, cookie)
	requireStatus(t, response, http.StatusAccepted)
	items, err := f.s.Sandboxes(ctx, other.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("outside fixture: %v, %d sandboxes", err, len(items))
	}

	f.s.SetFeatureCanaryWorkspace("missing-workspace")
	if f.s.canaryFeatureEnabled(f.space.ID, true) || f.s.canaryFeatureEnabled(other.ID, true) {
		t.Fatal("an unknown canary target matched an existing workspace")
	}
	f.s.SetFeatureCanaryWorkspace(f.space.ID)
	if f.s.canaryFeatureEnabled(other.ID, true) {
		t.Fatal("global flag escaped the configured scope")
	}
	for _, test := range []struct {
		sandbox Sandbox
		canary  bool
		version string
		cents   int64
	}{
		{canary, true, pricing.CIRateVersion, 500},
		{items[0], false, pricing.CIRateVersionV1, 600},
	} {
		id := test.sandbox.WorkspaceID
		got, err := f.s.sandboxComputeEstimate(id, 1, 2048)
		if err != nil || got != test.cents || f.s.ciAdmissionRateVersion(id) != test.version ||
			f.s.canReadCILogRanges(id) != test.canary || f.h.sandboxHistoryEnabled(id) != test.canary {
			t.Fatalf("incorrect rollout decision for %s: price=%d err=%v", id, got, err)
		}
		want, caller := http.StatusNotFound, cookie
		if test.canary {
			want, caller = http.StatusOK, f.cookie
		}
		path := "/v1/workspaces/" + id + "/sandboxes/" + test.sandbox.ID + "/transfers"
		rec := requestJSON(t, f.h, http.MethodGet, path, nil, caller)
		requireStatus(t, rec, want)
	}
}
