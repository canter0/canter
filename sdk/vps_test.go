package sdk

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/canter0/canter/internal/provider/compute"
	"golang.org/x/crypto/ssh"
)

func validVPSRequest(t *testing.T) VPSRequest {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewPublicKey(pub)
	return VPSRequest{Name: "one-hour", VCPUs: 2, MemoryMiB: 4096, DiskGiB: 80, SSHPublicKey: string(ssh.MarshalAuthorizedKey(key)), SSHCIDR: "192.0.2.10/32", ForSeconds: 3600}
}
func TestVPSRequestRejectsUnsafeAccess(t *testing.T) {
	for _, change := range []func(*VPSRequest){func(r *VPSRequest) { r.SSHPublicKey = "-----BEGIN PRIVATE KEY-----" }, func(r *VPSRequest) { r.SSHPublicKey = `command="curl evil" ` + r.SSHPublicKey }, func(r *VPSRequest) { r.SSHCIDR = "192.0.2.1/32; touch /tmp/evil" }, func(r *VPSRequest) { r.ForSeconds = -1 }, func(r *VPSRequest) { r.SSHPublicKey += r.SSHPublicKey }} {
		r := validVPSRequest(t)
		change(&r)
		if err := r.Validate(); err == nil {
			t.Fatal("accepted unsafe request")
		}
	}
	r := validVPSRequest(t)
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	script := vpsBootstrap(r)
	if !strings.Contains(script, "PasswordAuthentication no") || !strings.Contains(script, "ufw allow from 192.0.2.10/32") || !strings.Contains(script, "ssh-keygen -lf") {
		t.Fatal("missing access protection/proof")
	}
}
func TestVPSApplyUsesPinnedAllocationAndResumes(t *testing.T) {
	spec := testSpec()
	client, _, provider, model := testClient(spec)
	plan := VPSPlan{Spec: spec, Shape: compute.Shape{ID: "approved-shape", VCPU: 2, Memory: 4096, GB: 80}, ImageID: "approved-image", Networks: []string{"approved-network"}}
	provider.assertIntentOnCreate = func(state State, input compute.ManagedServerRequest) error {
		if input.FlavorID != plan.Shape.ID || input.ImageID != plan.ImageID || input.NetworkID != plan.Networks[0] {
			t.Fatal("substituted approved allocation")
		}
		return nil
	}
	state, err := client.ApplyVPS(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "ready" || len(state.NetworkPolicies) != 1 || state.NetworkPolicies[0].Port != 22 {
		t.Fatal("SSH exposure missing")
	}
	if _, err = client.ApplyVPS(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if provider.createCalls != 1 || provider.resolveCalls != 0 || model.compileCalls != 0 {
		t.Fatal("recovery re-created/re-priced/replanned")
	}
	plan.Shape.ID = "different"
	if _, err = client.ApplyVPS(context.Background(), plan); err == nil {
		t.Fatal("accepted changed allocation")
	}
}
func TestVPSCancelledBeforeCreateCannotReappear(t *testing.T) {
	spec := testSpec()
	client, _, provider, _ := testClient(spec)
	plan := VPSPlan{Spec: spec, Shape: compute.Shape{ID: "shape"}, ImageID: "image", Networks: []string{"network"}}
	if _, err := client.DestroyVPS(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ApplyVPS(context.Background(), plan); err == nil {
		t.Fatal("expired VM recreated")
	}
	if provider.createCalls != 0 {
		t.Fatal("late create escaped deletion fence")
	}
}
