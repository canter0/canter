package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"

	"github.com/canter0/canter/internal/provider/compute"
	"golang.org/x/crypto/ssh"
)

// VPSPlan is private control-plane evidence. Provider identifiers and bootstrap
// scripts are never included in the public proposal or model observations.
type VPSPlan struct {
	Spec     Spec          `json:"spec"`
	Shape    compute.Shape `json:"shape"`
	ImageID  string        `json:"imageId"`
	Networks []string      `json:"networks"`
}

type VPSRequest struct {
	Name         string `json:"name"`
	VCPUs        int    `json:"vcpus"`
	MemoryMiB    int    `json:"memoryMiB"`
	DiskGiB      int    `json:"diskGiB"`
	SSHPublicKey string `json:"sshPublicKey"`
	SSHCIDR      string `json:"sshCidr"`
	ForSeconds   int    `json:"forSeconds"`
}

func (r *VPSRequest) Validate() error {
	if !safeName.MatchString(r.Name) {
		return fmt.Errorf("server name must start with a letter and contain up to 48 lowercase letters, numbers, or hyphens")
	}
	if r.VCPUs < 1 || r.VCPUs > 32 || r.MemoryMiB < 512 || r.MemoryMiB > 131072 || r.DiskGiB < 10 || r.DiskGiB > 2048 {
		return fmt.Errorf("requested VM dimensions are outside supported bounds")
	}
	if r.ForSeconds != 0 && (r.ForSeconds < 300 || r.ForSeconds > 604800) {
		return fmt.Errorf("temporary servers require a duration from 5 minutes to 7 days; use 0 for no automatic expiry")
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(r.SSHPublicKey))
	if err != nil || len(rest) != 0 || len(options) != 0 || len(r.SSHPublicKey) > 16384 {
		return fmt.Errorf("supply one SSH public key without authorized_keys options; never a private key")
	}
	r.SSHPublicKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	cidr, err := netip.ParsePrefix(r.SSHCIDR)
	if err != nil || !cidr.Addr().Is4() {
		return fmt.Errorf("SSH access needs an IPv4 CIDR, for example your public IP followed by /32")
	}
	r.SSHCIDR = cidr.Masked().String()
	return nil
}

func (c *Client) PlanVPS(ctx context.Context, workspace, id string, request VPSRequest) (VPSPlan, error) {
	if err := request.Validate(); err != nil {
		return VPSPlan{}, err
	}
	if !safeM1Segment.MatchString(workspace) || !safeM1Segment.MatchString(id) {
		return VPSPlan{}, fmt.Errorf("invalid VPS ownership namespace")
	}
	resolver, ok := c.compute.(interface {
		ResolveSize(context.Context, int, int, int, string) (compute.Shape, string, []string, error)
	})
	if !ok {
		return VPSPlan{}, fmt.Errorf("VM catalog is unavailable")
	}
	shape, image, networks, err := resolver.ResolveSize(ctx, request.VCPUs, request.MemoryMiB, request.DiskGiB, "ubuntu-24.04")
	if err != nil {
		return VPSPlan{}, err
	}
	identity := sha256.Sum256([]byte(workspace + "/" + id))
	spec := Spec{APIVersion: APIVersion, Kind: "Sandbox", Metadata: Metadata{Name: "vps-" + hex.EncodeToString(identity[:16])}, Spec: Desired{Intent: "Standalone VPS " + request.Name, Compute: ComputeSpec{Class: "vps", Image: "ubuntu-24.04", Replicas: 1, Bootstrap: vpsBootstrap(request)}, M1: M1Spec{Prefix: "workspaces/" + workspace + "/vps/" + id}, Policy: Policy{MaxReplicas: 1}}}
	return VPSPlan{Spec: spec, Shape: shape, ImageID: image, Networks: networks}, nil
}

func vpsBootstrap(request VPSRequest) string {
	key := base64.StdEncoding.EncodeToString([]byte(request.SSHPublicKey + "\n"))
	// Only validated public key/CIDR enter the fixed script. No user shell or
	// passwords are accepted. The guest firewall also restricts the provider's
	// TCP/22 rule to the source range approved by the human.
	return `export DEBIAN_FRONTEND=noninteractive
command -v ufw >/dev/null
command -v sshd >/dev/null
id canter >/dev/null 2>&1 || useradd -m -s /bin/bash canter
install -d -m 700 -o canter -g canter /home/canter/.ssh
printf '%s' '` + key + `' | base64 -d > /home/canter/.ssh/authorized_keys
chown canter:canter /home/canter/.ssh/authorized_keys
chmod 600 /home/canter/.ssh/authorized_keys
printf 'canter ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/canter
chmod 440 /etc/sudoers.d/canter
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin no\nAllowUsers canter\n' > /etc/ssh/sshd_config.d/00-canter.conf
ufw default deny incoming
ufw default allow outgoing
ufw allow from ` + request.SSHCIDR + ` to any port 22 proto tcp
ufw --force enable
sshd -t
systemctl restart ssh
systemctl is-active --quiet ssh
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256 | awk '{print $2}'`
}

// ApplyVPS reuses the fenced lifecycle without a second model-generated plan.
// Recovery cannot recreate a destroyed server or substitute another shape.
func (c *Client) ApplyVPS(ctx context.Context, plan VPSPlan) (State, error) {
	spec := plan.Spec
	if err := spec.Validate(); err != nil {
		return State{}, err
	}
	key := stateKey(spec)
	var state State
	found, etag, err := c.m1.GetJSONVersion(ctx, key, &state)
	if err != nil {
		return state, err
	}
	if !found {
		state, err = c.createResolvedIntent(ctx, spec, key, false, etag, plan.Shape, plan.ImageID, plan.Networks)
		if err != nil {
			return state, err
		}
	}
	if state.CreationIntent == nil || state.CreationIntent.ShapeID != plan.Shape.ID || state.CreationIntent.ImageID != plan.ImageID {
		return state, fmt.Errorf("VM state differs from the approved allocation")
	}
	if state.Phase == "creating" {
		if err = validateCreationIntent(spec, state); err != nil {
			return state, err
		}
		if err = c.reconcileCreation(ctx, spec, key, &state); err != nil {
			return state, err
		}
	}
	if state.Phase != "ready" {
		return state, fmt.Errorf("VM needs attention in phase %s", state.Phase)
	}
	result := ApplyResult{State: state}
	sum := sha256.Sum256([]byte(spec.Spec.M1.Prefix))
	if err = c.exposeSpecTCP(ctx, spec, &result, false, 22, spec.Metadata.Name+"-ssh", fmt.Sprintf("sha256:%x", sum)); err != nil {
		return result.State, err
	}
	return result.State, nil
}

func (c *Client) VPSHostFingerprint(ctx context.Context, plan VPSPlan) (string, error) {
	var state State
	if err := c.m1.Get(ctx, stateKey(plan.Spec), &state); err != nil {
		return "", err
	}
	if len(state.ProofKeys) != 1 {
		return "", fmt.Errorf("VM boot proof is unavailable")
	}
	var proof BootProof
	if err := c.m1.Get(ctx, state.ProofKeys[0], &proof); err != nil {
		return "", err
	}
	value, err := base64.StdEncoding.DecodeString(proof.MessageBase64)
	if err != nil || proof.Status != "booted" || proof.ExitCode != 0 || !strings.HasPrefix(strings.TrimSpace(string(value)), "SHA256:") {
		return "", fmt.Errorf("VM has no verified SSH host fingerprint")
	}
	return strings.TrimSpace(string(value)), nil
}

func (c *Client) DestroyVPS(ctx context.Context, plan VPSPlan) (State, error) {
	var prior State
	found, err := c.m1.GetOptional(ctx, stateKey(plan.Spec), &prior)
	if err != nil {
		return prior, err
	}
	if !found {
		// A tombstone prevents a worker that lost its DB lease from starting
		// a late create after expiry cancelled an as-yet uncreated VM.
		tombstone := State{Sandbox: plan.Spec.Metadata.Name, Phase: "destroyed"}
		_, written, err := c.m1.PutJSONIfAbsent(ctx, stateKey(plan.Spec), tombstone)
		if err != nil {
			return State{}, err
		}
		if written {
			return tombstone, nil
		}
	}
	state, err := c.Destroy(ctx, plan.Spec)
	if err != nil {
		return state, err
	}
	// DELETE acceptance alone does not prove removal or that allocation stopped.
	for _, resource := range state.Resources {
		_, err := c.compute.Server(ctx, resource.ID)
		if !compute.IsNotFound(err) {
			if err != nil {
				return state, err
			}
			return state, fmt.Errorf("VM deletion is still in progress")
		}
	}
	return state, nil
}

func (c *Client) VPSBillingResources(ctx context.Context, plan VPSPlan) ([]BillingResource, error) {
	var state State
	found, err := c.m1.GetOptional(ctx, stateKey(plan.Spec), &state)
	if err != nil || !found {
		return nil, err
	}
	meter, ok := c.compute.(interface {
		ServerShape(context.Context, string) (compute.Shape, error)
	})
	if !ok {
		return nil, fmt.Errorf("VM metering unavailable")
	}
	out := []BillingResource{}
	for _, resource := range state.Resources {
		server, err := c.compute.Server(ctx, resource.ID)
		if compute.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if server.Metadata["canter.sandbox"] != plan.Spec.Metadata.Name || server.Metadata["canter.managed"] != "true" {
			return nil, fmt.Errorf("VM ownership changed")
		}
		shape, err := meter.ServerShape(ctx, resource.ID)
		if err != nil {
			return nil, err
		}
		if shape.VCPU < 1 || shape.Memory < 1 {
			return nil, fmt.Errorf("invalid observed VM allocation")
		}
		out = append(out, BillingResource{ID: "compute/" + resource.ID, Kind: "compute", Units: int64(max(shape.VCPU, (shape.Memory+1023)/1024))})
	}
	return out, nil
}
