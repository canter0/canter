package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
	"github.com/canter0/canter/sdk"
	"golang.org/x/crypto/ssh"
)

type vpsFakeEngine struct {
	initialDeploymentFakeEngine
	applied, destroyed int
	failApply          bool
	deletionPending    bool
}

func (f *vpsFakeEngine) PlanVPS(_ context.Context, workspace, id string, r sdk.VPSRequest) (sdk.VPSPlan, error) {
	return sdk.VPSPlan{Spec: sdk.Spec{Metadata: sdk.Metadata{Name: id}, Spec: sdk.Desired{M1: sdk.M1Spec{Prefix: "workspaces/" + workspace + "/vps/" + id}}}, Shape: compute.Shape{ID: "private-shape", VCPU: 2, Memory: 4096, GB: 80}, ImageID: "private-image", Networks: []string{"private-network"}}, nil
}
func (f *vpsFakeEngine) ApplyVPS(context.Context, sdk.VPSPlan) (sdk.State, error) {
	f.applied++
	if f.failApply {
		return sdk.State{}, errors.New("provider-private-error")
	}
	return sdk.State{Phase: "ready", Resources: []sdk.Resource{{ID: "private-server", Address: "192.0.2.10"}}}, nil
}
func (f *vpsFakeEngine) VPSHostFingerprint(context.Context, sdk.VPSPlan) (string, error) {
	return "SHA256:test-host-key", nil
}
func (f *vpsFakeEngine) DestroyVPS(context.Context, sdk.VPSPlan) (sdk.State, error) {
	f.destroyed++
	if f.deletionPending {
		return sdk.State{}, errors.New("still deleting")
	}
	return sdk.State{Phase: "destroyed"}, nil
}
func (f *vpsFakeEngine) VPSBillingResources(context.Context, sdk.VPSPlan) ([]sdk.BillingResource, error) {
	return []sdk.BillingResource{{ID: "compute/private-server", Kind: "compute", Units: 4}}, nil
}
func vpsTestRequest(t *testing.T) sdk.VPSRequest {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return sdk.VPSRequest{Name: "temporary-server", VCPUs: 2, MemoryMiB: 4096, DiskGiB: 80, SSHPublicKey: string(ssh.MarshalAuthorizedKey(key)), SSHCIDR: "192.0.2.1/32", ForSeconds: 3600}
}
func TestVPSApprovalExpiryAndVerifiedDeletion(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, "vps-owner@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	engine := &vpsFakeEngine{}
	service := &Service{Store: store, Engine: engine}
	actor := sdk.ActorRef{Kind: "human", ID: account.ID}
	v, err := service.DraftVPS(ctx, workspace.ID, vpsTestRequest(t), actor)
	if err != nil {
		t.Fatal(err)
	}
	if engine.applied != 0 {
		t.Fatal("draft provisioned a VM")
	}
	data, _ := json.Marshal(publicVPS(v))
	if strings.Contains(string(data), "private-") {
		t.Fatalf("provider identity exposed: %s", data)
	}
	if _, err = store.ApproveVPS(ctx, workspace.ID, v.ID, v.Digest, sdk.ActorRef{Kind: "agent", ID: "operator"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent approved: %v", err)
	}
	if _, err = store.ApproveVPS(ctx, workspace.ID, v.ID, "wrong", actor); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong digest approved: %v", err)
	}
	if _, err = store.VPS(ctx, "another-workspace", v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("workspace isolation failed: %v", err)
	}
	d := &VPSDispatcher{Store: store, Engine: engine}
	if err = d.Reconcile(ctx, workspace.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	if engine.applied != 0 {
		t.Fatal("unapproved VM created")
	}
	v, err = store.ApproveVPS(ctx, workspace.ID, v.ID, v.Digest, actor)
	if err != nil {
		t.Fatal(err)
	}
	if v.ExpiresAt == nil || v.ExpiresAt.Sub(*v.ApprovedAt) != time.Hour {
		t.Fatal("one-hour expiry not durable")
	}
	if _, err = store.ApproveVPS(ctx, workspace.ID, v.ID, v.Digest, actor); err != nil {
		t.Fatal(err)
	}
	if err = d.Reconcile(ctx, workspace.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = store.VPS(ctx, workspace.ID, v.ID)
	if v.Phase != "ready" || v.Address == "" || v.HostFingerprint == "" {
		t.Fatalf("no access: %+v", publicVPS(v))
	}
	if err = d.Reconcile(ctx, workspace.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	if engine.applied != 1 {
		t.Fatal("ready server recreated")
	}
	store.now = func() time.Time { return v.ExpiresAt.Add(time.Second) }
	restarted := &VPSDispatcher{Store: store, Engine: engine}
	engine.deletionPending = true
	if err = restarted.Reconcile(ctx, workspace.ID, v.ID); err == nil {
		t.Fatal("unverified removal reported success")
	}
	current, _ := store.VPS(ctx, workspace.ID, v.ID)
	if current.Phase != "deleting" {
		t.Fatal("expiry lost on restart")
	}
	engine.deletionPending = false
	if err = restarted.Reconcile(ctx, workspace.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	current, _ = store.VPS(ctx, workspace.ID, v.ID)
	if current.Phase != "deleted" || current.Address != "" {
		t.Fatal("deletion not verified")
	}
	var reserved int
	if err = store.pool.QueryRow(ctx, `SELECT reserved_cents FROM workspace_usage_caps WHERE workspace_id=$1`, workspace.ID).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatalf("reservation leaked: %d %v", reserved, err)
	}
}
func TestVPSFailedProvisioningCleansUp(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, "vps-fail@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	engine := &vpsFakeEngine{failApply: true}
	service := &Service{Store: store, Engine: engine}
	actor := sdk.ActorRef{Kind: "human", ID: account.ID}
	v, err := service.DraftVPS(ctx, workspace.ID, vpsTestRequest(t), actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApproveVPS(ctx, workspace.ID, v.ID, v.Digest, actor); err != nil {
		t.Fatal(err)
	}
	d := &VPSDispatcher{Store: store, Engine: engine}
	if err = d.Reconcile(ctx, workspace.ID, v.ID); err == nil {
		t.Fatal("provider error lost")
	}
	current, _ := store.VPS(ctx, workspace.ID, v.ID)
	if current.Phase != "deleting" || strings.Contains(current.Failure, "private") {
		t.Fatalf("unsafe failure: %s", current.Failure)
	}
	if err = d.Reconcile(ctx, workspace.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	if engine.applied != 1 || engine.destroyed != 1 {
		t.Fatal("cleanup repeated provisioning")
	}
}
func TestVPSOperatorCreatesReviewWithoutProvisioning(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, "vps-operator@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	engine := &vpsFakeEngine{}
	h := &HTTPServer{service: &Service{Store: store, Engine: engine}}
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"digest":"anything"}`))
	response := httptest.NewRecorder()
	h.workspaceVPS(response, request, Principal{Account: &account, Installation: &Installation{}, WorkspaceID: workspace.ID}, workspace.ID, []string{"unknown", "approve"})
	if response.Code == http.StatusAccepted {
		t.Fatal("mixed principal authorized VM")
	}
	operator := &OperatorRuntime{Server: h}
	catalog := operator.tools()
	found := false
	for _, tool := range catalog {
		if tool.Name == "canter_prepare_vps" {
			found = true
			policy, ok := operatorPolicy(tool.Name)
			if !ok || !policy.RequireDraft || policy.Effect != "draft" {
				t.Fatal("VM preparation is not draft guarded")
			}
		}
	}
	if !found || !operator.vpsAvailable() {
		t.Fatal("VPS path missing from operator")
	}
}
