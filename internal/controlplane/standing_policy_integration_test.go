package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
)

type standingPolicyEngine struct {
	changes map[string]sdk.Change
}

func (e *standingPolicyEngine) InspectSystem(_ context.Context, system sdk.System) (sdk.SystemView, error) {
	return sdk.CompileSystemView(system)
}
func (e *standingPolicyEngine) DraftChangeRequest(context.Context, sdk.System, sdk.ChangeRequest) (sdk.Change, error) {
	return sdk.Change{}, errors.New("unused")
}
func (e *standingPolicyEngine) InspectChange(_ context.Context, _ sdk.System, id string) (sdk.Change, error) {
	change, ok := e.changes[id]
	if !ok {
		return sdk.Change{}, ErrNotFound
	}
	return change, nil
}
func (e *standingPolicyEngine) AuthorizeChange(ctx context.Context, _ sdk.System, id, digest string) (sdk.Change, error) {
	change, ok := e.changes[id]
	if !ok || change.Phase != "drafted" || change.Digest != digest {
		return sdk.Change{}, ErrConflict
	}
	actor, _ := sdk.ActorFromContext(ctx)
	now := time.Now().UTC()
	change.Phase = "authorized"
	change.Authorization = &sdk.Authorization{Digest: digest, AuthorizedAt: now, AuthorizedBy: &actor}
	change.UpdatedAt = now
	e.changes[id] = change
	return change, nil
}
func (e *standingPolicyEngine) ApplyChange(context.Context, sdk.System, string) (sdk.Change, error) {
	return sdk.Change{}, errors.New("unused")
}

func TestStandingPolicyAutomaticallyAuthorizesOnlyMatchingExactChange(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	account, workspace, humanToken, err := store.Signup(ctx, "policy-owner@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	system, err := sdk.NewSystem("policy-api", "Serve the policy test API").
		OnHost("compute", 1, 1024, 256).
		WithM1("systems/policy-api").
		Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	system, err = canonicalizeSystemForWorkspace(workspace.ID, system)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.PutSystem(ctx, workspace.ID, system); err != nil {
		t.Fatal(err)
	}
	device, err := store.BeginDeviceAuthorization(ctx, "Policy blackout", "blackout", Authority{Inspect: true, Draft: true}, "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApproveDevice(ctx, device.UserCode, account.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	pair, err := store.ExchangeDevice(ctx, device.DeviceCode, "policy-conversation")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := store.ResolveAgent(ctx, pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	envelope := StandingPolicyEnvelope{
		AllowedInstallationIDs:        []string{pair.Installation.ID},
		AffectedServices:              []string{"web"},
		OperationKinds:                []string{"http.verify", "release.set-desired"},
		Availability:                  []string{"rolling replacement"},
		Data:                          []string{"none"},
		AllowedReversibility:          []string{"compensatable", "not-applicable"},
		MaxAdditionalMonthlyCostCents: 500,
		MaxOperations:                 2,
	}
	policyInput := CreateStandingPolicyInput{Name: "bounded-release", Description: "Permit only this agent to make no-data rolling releases", Envelope: envelope, ExpiresAt: time.Now().Add(2 * time.Hour)}
	policyPath := "/v1/workspaces/" + workspace.ID + "/systems/" + system.Metadata.Name + "/policies"
	handler := NewHTTPServer(&Service{Store: store}, HTTPConfig{PublicURL: "http://canter.test"})
	agentCreate := requestJSONWithBearer(t, handler, policyPath, policyInput, pair.AccessToken)
	if agentCreate.Code != http.StatusForbidden {
		t.Fatalf("agent created a standing policy: %d %s", agentCreate.Code, agentCreate.Body.String())
	}
	humanCreate := requestJSON(t, handler, http.MethodPost, policyPath, policyInput, &http.Cookie{Name: "canter_session", Value: humanToken})
	if humanCreate.Code != http.StatusCreated {
		t.Fatalf("human policy creation failed: %d %s", humanCreate.Code, humanCreate.Body.String())
	}
	overCapacity := policyInput
	overCapacity.Name = "unsafe-scale"
	overCapacity.Envelope.OperationKinds = []string{"state.assert", "release.scale", "release.wait-replicas", "http.verify"}
	overCapacity.Envelope.Availability = []string{"capacity adjustment; healthy replicas remain serving"}
	overCapacity.Envelope.AllowedReversibility = []string{"read-only", "compensatable"}
	overCapacity.Envelope.MaxOperations = 4
	overCapacity.Envelope.ScaleLimits = map[string]ReplicaRange{"web": {Min: 1, Max: 4}}
	overCapacity.Envelope.AllowPermanentScale = true
	tooWide := requestJSON(t, handler, http.MethodPost, policyPath, overCapacity, &http.Cookie{Name: "canter_session", Value: humanToken})
	if tooWide.Code != http.StatusBadRequest {
		t.Fatalf("policy wider than current host capacity was accepted: %d %s", tooWide.Code, tooWide.Body.String())
	}
	var policy StandingPolicy
	if err = json.Unmarshal(humanCreate.Body.Bytes(), &policy); err != nil {
		t.Fatal(err)
	}
	makeChange := func(id string, cost int64) sdk.Change {
		now := time.Now().UTC()
		return sdk.Change{SchemaVersion: "v1", ID: id, System: system.Metadata.Name, Summary: id, Phase: "drafted", Digest: strings.Repeat(string(id[len(id)-1]), 64), Plan: sdk.ChangePlan{Impact: sdk.ChangeImpact{AffectedServices: []string{"web"}, Availability: "rolling replacement", Data: "none", MonthlyCostDeltaCents: cost}}, Operations: []sdk.ChangeOperation{{ID: "01-release", Kind: "release.set-desired", Reversibility: "compensatable", Phase: "pending"}, {ID: "02-verify", Kind: "http.verify", Reversibility: "not-applicable", Phase: "pending"}}, CreatedAt: now, UpdatedAt: now}
	}
	matching := makeChange("change-a", 500)
	expensive := makeChange("change-b", 501)
	afterRevoke := makeChange("change-c", 0)
	for _, change := range []sdk.Change{matching, expensive, afterRevoke} {
		if err = store.RecordChange(ctx, workspace.ID, change); err != nil {
			t.Fatal(err)
		}
	}
	engine := &standingPolicyEngine{changes: map[string]sdk.Change{matching.ID: matching, expensive.ID: expensive, afterRevoke.ID: afterRevoke}}
	service := Service{Store: store, Engine: engine}

	automatic, err := service.ApplyChangeUnderPolicy(ctx, workspace.ID, system.Metadata.Name, matching.ID, matching.Digest, principal)
	if err != nil {
		t.Fatal(err)
	}
	if automatic.Decision.Outcome != "automatic" || automatic.Decision.PolicyID != policy.ID || automatic.Execution == nil || automatic.Execution.RequestedBy.Kind != "policy" || automatic.Execution.RequestedBy.ID != policy.ID {
		t.Fatalf("matching Change did not retain its policy chain: %#v", automatic)
	}
	authorized := engine.changes[matching.ID]
	if authorized.Authorization == nil || authorized.Authorization.AuthorizedBy == nil || authorized.Authorization.AuthorizedBy.Kind != "policy" || authorized.Authorization.AuthorizedBy.ID != policy.ID {
		t.Fatalf("engine authorization was not bound to the immutable policy: %#v", authorized.Authorization)
	}
	inspection, err := service.InspectChangeWithExecution(ctx, workspace.ID, system.Metadata.Name, matching.ID)
	if err != nil || inspection.Execution == nil || inspection.Execution.ID != automatic.Execution.ID {
		t.Fatalf("fresh agent inspection lost automatic execution identity: %#v %v", inspection, err)
	}

	denied, err := service.ApplyChangeUnderPolicy(ctx, workspace.ID, system.Metadata.Name, expensive.ID, expensive.Digest, principal)
	if err != nil {
		t.Fatal(err)
	}
	if denied.Decision.Outcome != "human-approval-required" || denied.Execution != nil || engine.changes[expensive.ID].Authorization != nil {
		t.Fatalf("out-of-envelope Change was mutated: %#v", denied)
	}
	if _, err = store.ExecutionForChange(ctx, workspace.ID, system.Metadata.Name, expensive.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-envelope Change gained an execution: %v", err)
	}

	if _, err = store.RevokeStandingPolicy(ctx, workspace.ID, system.Metadata.Name, policy.ID, account.ID); err != nil {
		t.Fatal(err)
	}
	revoked, err := service.ApplyChangeUnderPolicy(ctx, workspace.ID, system.Metadata.Name, afterRevoke.ID, afterRevoke.Digest, principal)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Decision.Outcome != "human-approval-required" || revoked.Execution != nil {
		t.Fatalf("revoked standing policy still authorized a Change: %#v", revoked)
	}
}
