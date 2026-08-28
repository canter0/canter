package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ReleaseManifest struct {
	SchemaVersion string            `json:"schemaVersion"`
	System        string            `json:"system"`
	Version       string            `json:"version"`
	ArtifactKey   string            `json:"artifactKey"`
	ArtifactSHA   string            `json:"artifactSha256"`
	Command       []string          `json:"command"`
	Environment   map[string]string `json:"environment,omitempty"`
	HealthPath    string            `json:"healthPath"`
	PublicPort    int               `json:"publicPort"`
	RequestedAt   time.Time         `json:"requestedAt"`
}

type ObservedRelease struct {
	SchemaVersion  string    `json:"schemaVersion"`
	System         string    `json:"system"`
	Phase          string    `json:"phase"`
	DesiredVersion string    `json:"desiredVersion,omitempty"`
	RunningVersion string    `json:"runningVersion,omitempty"`
	PID            int       `json:"pid,omitempty"`
	Restarts       int       `json:"restarts"`
	PublicPort     int       `json:"publicPort"`
	InternalPort   int       `json:"internalPort,omitempty"`
	Healthy        bool      `json:"healthy"`
	Message        string    `json:"message,omitempty"`
	Node           string    `json:"node,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type RuntimeControl struct {
	ID          string    `json:"id"`
	Action      string    `json:"action"`
	RequestedAt time.Time `json:"requestedAt"`
}

type PublishReleaseInput struct {
	ArtifactPath string
	Command      []string
	Environment  map[string]string
	HealthPath   string
	PublicPort   int
}

func (c *Client) PublishRelease(ctx context.Context, system System, input PublishReleaseInput) (ReleaseManifest, error) {
	if err := system.Validate(); err != nil {
		return ReleaseManifest{}, err
	}
	if len(input.Command) == 0 || input.PublicPort < 1 || input.PublicPort > 65535 || !strings.HasPrefix(input.HealthPath, "/") {
		return ReleaseManifest{}, fmt.Errorf("release requires a command, absolute health path, and valid public port")
	}
	artifact, err := os.ReadFile(input.ArtifactPath)
	if err != nil {
		return ReleaseManifest{}, err
	}
	sum := sha256.Sum256(artifact)
	digest := hex.EncodeToString(sum[:])
	version := digest[:12]
	prefix := strings.TrimRight(system.Spec.M1.Prefix, "/")
	artifactKey := fmt.Sprintf("%s/artifacts/%s%s", prefix, digest, filepath.Ext(input.ArtifactPath))
	if err := c.m1.Put(ctx, artifactKey, artifact, "application/gzip"); err != nil {
		return ReleaseManifest{}, fmt.Errorf("upload release artifact: %w", err)
	}
	manifest := ReleaseManifest{
		SchemaVersion: "v1", System: system.Metadata.Name, Version: version,
		ArtifactKey: artifactKey, ArtifactSHA: digest, Command: append([]string(nil), input.Command...),
		Environment: input.Environment, HealthPath: input.HealthPath, PublicPort: input.PublicPort, RequestedAt: time.Now().UTC(),
	}
	if err := c.m1.PutJSON(ctx, releaseKey(system, version), manifest); err != nil {
		return ReleaseManifest{}, fmt.Errorf("persist release manifest: %w", err)
	}
	if err := c.m1.PutJSON(ctx, desiredKey(system), manifest); err != nil {
		return ReleaseManifest{}, fmt.Errorf("set desired release: %w", err)
	}
	return manifest, nil
}

func (c *Client) ReleaseStatus(ctx context.Context, system System) (ObservedRelease, error) {
	var observed ObservedRelease
	if err := c.m1.Get(ctx, observedKey(system), &observed); err != nil {
		return ObservedRelease{}, err
	}
	return observed, nil
}

func (c *Client) RollbackRelease(ctx context.Context, system System, version string) (ReleaseManifest, error) {
	if version == "" {
		return ReleaseManifest{}, fmt.Errorf("rollback version is required")
	}
	var manifest ReleaseManifest
	if err := c.m1.Get(ctx, releaseKey(system, version), &manifest); err != nil {
		return ReleaseManifest{}, fmt.Errorf("load rollback release: %w", err)
	}
	manifest.RequestedAt = time.Now().UTC()
	if err := c.m1.PutJSON(ctx, desiredKey(system), manifest); err != nil {
		return ReleaseManifest{}, err
	}
	return manifest, nil
}

func (c *Client) RestartRelease(ctx context.Context, system System) (RuntimeControl, error) {
	control := RuntimeControl{ID: newID(), Action: "restart", RequestedAt: time.Now().UTC()}
	if err := c.m1.PutJSON(ctx, controlKey(system), control); err != nil {
		return RuntimeControl{}, err
	}
	return control, nil
}

func (c *Client) BootstrapSystemHost(ctx context.Context, system System, nodeBinary []byte) (ApplyResult, error) {
	if err := system.Validate(); err != nil {
		return ApplyResult{}, err
	}
	host := system.Spec.Constraints.Host
	if host.Count != 1 {
		return ApplyResult{}, fmt.Errorf("node runtime experiment currently requires exactly one host")
	}
	sum := sha256.Sum256(nodeBinary)
	digest := hex.EncodeToString(sum[:])
	prefix := strings.TrimRight(system.Spec.M1.Prefix, "/")
	nodeKey := fmt.Sprintf("%s/runtime/canter-node-%s", prefix, digest[:12])
	if err := c.m1.Put(ctx, nodeKey, nodeBinary, "application/octet-stream"); err != nil {
		return ApplyResult{}, err
	}
	url, err := c.m1.PresignGet(ctx, nodeKey, 15*time.Minute)
	if err != nil {
		return ApplyResult{}, err
	}
	publicPort, err := systemPublicPort(system)
	if err != nil {
		return ApplyResult{}, err
	}
	bootstrap, err := renderNodeBootstrap(system, url, digest, publicPort)
	if err != nil {
		return ApplyResult{}, err
	}
	spec := SystemHostSpec(system, bootstrap)
	result, err := c.Apply(ctx, spec)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := c.exposeSystemHost(ctx, system, &result); err != nil {
		return ApplyResult{}, err
	}
	return result, nil
}

func (c *Client) ExposeSystemHost(ctx context.Context, system System) (State, error) {
	state, err := c.SystemHostStatus(ctx, system)
	if err != nil {
		return State{}, err
	}
	result := ApplyResult{State: state}
	if err := c.exposeSystemHost(ctx, system, &result); err != nil {
		return State{}, err
	}
	return result.State, nil
}

func (c *Client) exposeSystemHost(ctx context.Context, system System, result *ApplyResult) error {
	port, err := systemPublicPort(system)
	if err != nil {
		return err
	}
	if len(result.State.Resources) != 1 {
		return fmt.Errorf("public endpoint currently requires exactly one host")
	}
	policy, err := c.compute.ExposeTCP(ctx, result.State.Resources[0].ID, "canter-"+system.Metadata.Name, port)
	if err != nil {
		return err
	}
	mapped := NetworkPolicy{ID: policy.ID, PortID: policy.PortID, RuleID: policy.RuleID, Protocol: "tcp", Port: policy.Port}
	found := false
	for _, existing := range result.State.NetworkPolicies {
		if existing.ID == mapped.ID {
			found = true
		}
	}
	if !found {
		result.State.NetworkPolicies = append(result.State.NetworkPolicies, mapped)
	}
	spec := SystemHostSpec(system, "true")
	if err := c.m1.PutJSON(ctx, stateKey(spec), result.State); err != nil {
		return err
	}
	if result.ReceiptKey != "" {
		if err := c.m1.PutJSON(ctx, result.ReceiptKey, result); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) SystemHostStatus(ctx context.Context, system System) (State, error) {
	return c.Status(ctx, SystemHostSpec(system, "true"))
}

func (c *Client) DestroySystemHost(ctx context.Context, system System) (State, error) {
	state, err := c.Destroy(ctx, SystemHostSpec(system, "true"))
	if err != nil {
		return State{}, err
	}
	var observed ObservedRelease
	if found, getErr := c.m1.GetOptional(ctx, observedKey(system), &observed); getErr != nil {
		return State{}, getErr
	} else if found {
		observed.Phase = "host-destroyed"
		observed.RunningVersion = ""
		observed.PID = 0
		observed.InternalPort = 0
		observed.Healthy = false
		observed.Message = "compute host destroyed"
		observed.UpdatedAt = time.Now().UTC()
		if putErr := c.m1.PutJSON(ctx, observedKey(system), observed); putErr != nil {
			return State{}, putErr
		}
	}
	return state, nil
}

func SystemHostSpec(system System, bootstrap string) Spec {
	host := system.Spec.Constraints.Host
	return Spec{
		APIVersion: APIVersion, Kind: "Sandbox", Metadata: system.Metadata,
		Spec: Desired{
			Intent:  "Run the Canter node runtime and reconcile versioned application releases.",
			Compute: ComputeSpec{Class: host.Class, Image: "ubuntu-24.04", Replicas: host.Count, Bootstrap: bootstrap},
			M1:      system.Spec.M1, Policy: Policy{MaxReplicas: host.Count},
		},
	}
}

func systemPublicPort(system System) (int, error) {
	for _, service := range system.Spec.Services {
		if service.Readiness.Protocol == "http" && service.Readiness.Port > 0 {
			return service.Readiness.Port, nil
		}
	}
	return 0, fmt.Errorf("system has no HTTP service readiness port")
}

func renderNodeBootstrap(system System, nodeURL, digest string, publicPort int) (string, error) {
	values := map[string]string{
		"endpoint": os.Getenv("CANTER_M1_ENDPOINT"), "bucket": os.Getenv("CANTER_M1_BUCKET"),
		"access": os.Getenv("CANTER_M1_ACCESS_KEY"), "secret": os.Getenv("CANTER_M1_SECRET_KEY"), "region": os.Getenv("CANTER_M1_REGION"),
	}
	for name, value := range values {
		if value == "" {
			return "", fmt.Errorf("missing m1 value %s for node bootstrap", name)
		}
	}
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	return fmt.Sprintf(`set -eu
curl -fsSL --proto '=https' --tlsv1.2 %s -o /usr/local/bin/canter-node
printf '%%s  %%s\n' %s /usr/local/bin/canter-node | sha256sum --check --status
chmod 0755 /usr/local/bin/canter-node
install -d -m 0750 /etc/canter /var/lib/canter-node
decode() { printf '%%s' "$1" | base64 -d; }
{
  printf 'CANTER_M1_ENDPOINT=%%s\n' "$(decode %s)"
  printf 'CANTER_M1_BUCKET=%%s\n' "$(decode %s)"
  printf 'CANTER_M1_ACCESS_KEY=%%s\n' "$(decode %s)"
  printf 'CANTER_M1_SECRET_KEY=%%s\n' "$(decode %s)"
  printf 'CANTER_M1_REGION=%%s\n' "$(decode %s)"
} > /etc/canter/node.env
chmod 0600 /etc/canter/node.env
cat > /etc/systemd/system/canter-node.service <<'EOF'
[Unit]
Description=Canter application reconciler
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/canter/node.env
ExecStart=/usr/local/bin/canter-node --system %s --prefix %s --public-port %d
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now canter-node.service
for attempt in $(seq 1 30); do systemctl is-active --quiet canter-node.service && exit 0; sleep 1; done
systemctl status --no-pager canter-node.service || true
exit 1`, shellQuote(nodeURL), shellQuote(digest), shellQuote(encode(values["endpoint"])), shellQuote(encode(values["bucket"])), shellQuote(encode(values["access"])), shellQuote(encode(values["secret"])), shellQuote(encode(values["region"])), system.Metadata.Name, system.Spec.M1.Prefix, publicPort), nil
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func desiredKey(system System) string {
	return strings.TrimRight(system.Spec.M1.Prefix, "/") + "/desired.json"
}
func observedKey(system System) string {
	return strings.TrimRight(system.Spec.M1.Prefix, "/") + "/observed.json"
}
func controlKey(system System) string {
	return strings.TrimRight(system.Spec.M1.Prefix, "/") + "/control.json"
}
func releaseKey(system System, version string) string {
	return strings.TrimRight(system.Spec.M1.Prefix, "/") + "/releases/" + version + ".json"
}
