package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
)

type approvalCapabilityEngine struct {
	change       sdk.Change
	authorizedBy sdk.ActorRef
	digest       string
}

func (e *approvalCapabilityEngine) InspectSystem(_ context.Context, system sdk.System) (sdk.SystemView, error) {
	return sdk.CompileSystemView(system)
}
func (e *approvalCapabilityEngine) DraftChangeRequest(context.Context, sdk.System, sdk.ChangeRequest) (sdk.Change, error) {
	return sdk.Change{}, errors.New("unused")
}
func (e *approvalCapabilityEngine) InspectChange(_ context.Context, _ sdk.System, id string) (sdk.Change, error) {
	if id != e.change.ID {
		return sdk.Change{}, ErrNotFound
	}
	return e.change, nil
}
func (e *approvalCapabilityEngine) AuthorizeChange(ctx context.Context, _ sdk.System, id, digest string) (sdk.Change, error) {
	if id != e.change.ID || digest != e.change.Digest || e.change.Phase != "drafted" {
		return sdk.Change{}, ErrConflict
	}
	e.digest = digest
	e.authorizedBy, _ = sdk.ActorFromContext(ctx)
	now := time.Now().UTC()
	e.change.Phase = "authorized"
	e.change.Authorization = &sdk.Authorization{Digest: digest, AuthorizedAt: now, AuthorizedBy: &e.authorizedBy}
	e.change.UpdatedAt = now
	return e.change, nil
}
func (e *approvalCapabilityEngine) ApplyChange(context.Context, sdk.System, string) (sdk.Change, error) {
	return sdk.Change{}, errors.New("unused")
}

func TestAgentRequestsHumanGatedChangeApprovalAndLinkCannotReplay(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	account, workspace, humanToken, err := store.Signup(ctx, "approval-owner@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	system, err := sdk.NewSystem("approval-api", "Run the approval test API").
		OnHost("compute", 1, 1024, 256).
		WithM1("systems/approval-api").
		Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}, Networking: "public"}).
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
	device, err := store.BeginDeviceAuthorization(ctx, "Approval requester", "blackout", Authority{Inspect: true, Draft: true}, "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApproveDevice(ctx, device.UserCode, account.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	pair, err := store.ExchangeDevice(ctx, device.DeviceCode, "approval-request-conversation")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	change := sdk.Change{
		SchemaVersion: "v1", ID: "change-focused-approval", System: system.Metadata.Name,
		Summary: "Temporarily move to the exact reviewed release", Phase: "drafted", Digest: strings.Repeat("a", 64),
		Plan:       sdk.ChangePlan{BaseVersion: "old-release", Release: sdk.ReleaseManifest{ArtifactKey: "private/object/key", Command: []string{"./app", "approval-secret"}, Environment: map[string]string{"API_TOKEN": "approval-secret"}}, Impact: sdk.ChangeImpact{AffectedServices: []string{"web"}, Availability: "rolling replacement", Data: "none", MonthlyCostDeltaCents: 0}},
		Operations: []sdk.ChangeOperation{{ID: "01-release", Kind: "release.set-desired", Description: "set exact desired release", Reversibility: "compensatable", Phase: "pending"}},
		CreatedAt:  now, UpdatedAt: now,
	}
	if err = store.RecordChange(ctx, workspace.ID, change); err != nil {
		t.Fatal(err)
	}
	engine := &approvalCapabilityEngine{change: change}
	handler := NewHTTPServer(&Service{Store: store, Engine: engine}, HTTPConfig{PublicURL: "http://canter.test"})
	requestPath := "/v1/workspaces/" + workspace.ID + "/systems/" + system.Metadata.Name + "/changes/" + change.ID + "/approval-links"

	wrongDigest := requestJSONWithBearer(t, handler, requestPath, map[string]string{"digest": strings.Repeat("b", 64)}, pair.AccessToken)
	if wrongDigest.Code != http.StatusConflict {
		t.Fatalf("wrong digest status %d: %s", wrongDigest.Code, wrongDigest.Body.String())
	}
	humanRequest := requestJSON(t, handler, http.MethodPost, requestPath, map[string]string{"digest": change.Digest}, &http.Cookie{Name: "canter_session", Value: humanToken})
	if humanRequest.Code != http.StatusForbidden {
		t.Fatalf("human created agent approval capability: %d %s", humanRequest.Code, humanRequest.Body.String())
	}
	created := requestJSONWithBearer(t, handler, requestPath, map[string]string{"digest": change.Digest}, pair.AccessToken)
	if created.Code != http.StatusCreated {
		t.Fatalf("create approval status %d: %s", created.Code, created.Body.String())
	}
	var capability ChangeApprovalCapability
	if err = json.Unmarshal(created.Body.Bytes(), &capability); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(capability.ReviewURL)
	if err != nil {
		t.Fatal(err)
	}
	token := path.Base(parsed.Path)
	if !strings.HasPrefix(token, "cap_") || capability.Digest != change.Digest || capability.RequestedBy.ID != pair.Installation.ID || capability.Action != "authorize-and-apply" {
		t.Fatalf("approval capability is not exactly bound: %#v", capability)
	}

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/v1/change-approvals/"+token, nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated review status %d: %s", unauthenticated.Code, unauthenticated.Body.String())
	}
	otherSignup := requestJSON(t, handler, http.MethodPost, "/v1/auth/signup", map[string]any{"email": "approval-outsider@example.com", "password": "correct horse battery staple"}, nil)
	if otherSignup.Code != http.StatusCreated {
		t.Fatal(otherSignup.Body.String())
	}
	otherCookie := otherSignup.Result().Cookies()[0]
	outsider := httptest.NewRequest(http.MethodGet, "/v1/change-approvals/"+token, nil)
	outsider.AddCookie(otherCookie)
	outsiderResponse := httptest.NewRecorder()
	handler.ServeHTTP(outsiderResponse, outsider)
	if outsiderResponse.Code != http.StatusNotFound {
		t.Fatalf("outsider review status %d: %s", outsiderResponse.Code, outsiderResponse.Body.String())
	}

	ownerCookie := &http.Cookie{Name: "canter_session", Value: humanToken}
	reviewRequest := httptest.NewRequest(http.MethodGet, "/v1/change-approvals/"+token, nil)
	reviewRequest.AddCookie(ownerCookie)
	reviewResponse := httptest.NewRecorder()
	handler.ServeHTTP(reviewResponse, reviewRequest)
	if reviewResponse.Code != http.StatusOK || !bytes.Contains(reviewResponse.Body.Bytes(), []byte(change.Digest)) || !bytes.Contains(reviewResponse.Body.Bytes(), []byte(pair.Installation.Name)) || bytes.Contains(reviewResponse.Body.Bytes(), []byte("approval-secret")) || bytes.Contains(reviewResponse.Body.Bytes(), []byte("private/object/key")) {
		t.Fatalf("owner review status %d: %s", reviewResponse.Code, reviewResponse.Body.String())
	}

	approved := requestJSON(t, handler, http.MethodPost, "/v1/change-approvals/"+token+"/approve", struct{}{}, ownerCookie)
	if approved.Code != http.StatusAccepted {
		t.Fatalf("focused approval status %d: %s", approved.Code, approved.Body.String())
	}
	var result ChangeApprovalResult
	if err = json.Unmarshal(approved.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(approved.Body.Bytes(), []byte("approval-secret")) || result.Change.Plan.Release.Environment["API_TOKEN"] != redactedValue {
		t.Fatalf("approval response leaked the sealed Change environment: %s", approved.Body.String())
	}
	if result.Execution.ID == "" || result.Change.Phase != "authorized" || engine.digest != change.Digest || engine.authorizedBy.Kind != "human" || engine.authorizedBy.ID != account.ID {
		t.Fatalf("focused approval did not bind human, digest, and execution: %#v actor=%#v", result, engine.authorizedBy)
	}
	replayed := requestJSON(t, handler, http.MethodPost, "/v1/change-approvals/"+token+"/approve", struct{}{}, ownerCookie)
	if replayed.Code != http.StatusNotFound {
		t.Fatalf("replayed capability status %d: %s", replayed.Code, replayed.Body.String())
	}
	storedExecution, err := store.Execution(ctx, result.Execution.ID)
	if err != nil || storedExecution.RequestedBy.Kind != "human" || storedExecution.RequestedBy.ID != account.ID {
		t.Fatalf("execution did not retain human requester: %#v %v", storedExecution, err)
	}
}

func TestChangeApprovalCapabilityExpiresAndNewRequestRevokesPriorLink(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, "approval-expiry@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	device, err := store.BeginDeviceAuthorization(ctx, "Expiry requester", "blackout", Authority{Inspect: true, Draft: true}, "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	installation, err := store.ApproveDevice(ctx, device.UserCode, account.ID, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := store.ExchangeDevice(ctx, device.DeviceCode, "expiry-request")
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.ResolveAgent(ctx, pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	change := sdk.Change{SchemaVersion: "v1", ID: "change-expiry", System: "api", Summary: "expiry", Phase: "drafted", Digest: strings.Repeat("c", 64), CreatedAt: now, UpdatedAt: now}
	if err = store.RecordChange(ctx, workspace.ID, change); err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateChangeApprovalCapability(ctx, workspace.ID, "api", change.ID, change.Digest, p, "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateChangeApprovalCapability(ctx, workspace.ID, "api", change.ID, change.Digest, p, "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	firstToken := path.Base(mustParseURL(t, first.ReviewURL).Path)
	secondToken := path.Base(mustParseURL(t, second.ReviewURL).Path)
	if _, err = store.ReviewChangeApprovalCapability(ctx, firstToken, account.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("superseded link remained live: %v", err)
	}
	store.now = func() time.Time { return second.ExpiresAt.Add(time.Second) }
	if _, err = store.ReviewChangeApprovalCapability(ctx, secondToken, account.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired link remained live: %v", err)
	}
	third, err := store.CreateChangeApprovalCapability(ctx, workspace.ID, "api", change.ID, change.Digest, p, "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	thirdToken := path.Base(mustParseURL(t, third.ReviewURL).Path)
	type consumeResult struct{ err error }
	start := make(chan struct{})
	results := make(chan consumeResult, 2)
	for range 2 {
		go func() {
			<-start
			_, consumeErr := store.ConsumeChangeApprovalCapability(ctx, thirdToken, account.ID)
			results <- consumeResult{err: consumeErr}
		}()
	}
	close(start)
	successes, rejected := 0, 0
	for range 2 {
		result := <-results
		if result.err == nil {
			successes++
		} else if errors.Is(result.err, ErrNotFound) {
			rejected++
		} else {
			t.Fatalf("concurrent replay returned unexpected error: %v", result.err)
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("concurrent consume successes=%d rejected=%d", successes, rejected)
	}
	if installation.ID == "" {
		t.Fatal("installation was not created")
	}
}

func mustParseURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
