package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
)

type ReleaseManifest struct {
	SchemaVersion string            `json:"schemaVersion"`
	System        string            `json:"system"`
	Version       string            `json:"version"`
	ArtifactKey   string            `json:"artifactKey,omitempty"`
	ArtifactSHA   string            `json:"artifactSha256"`
	Command       []string          `json:"command"`
	Environment   map[string]string `json:"environment,omitempty"`
	HealthPath    string            `json:"healthPath"`
	PublicPort    int               `json:"publicPort"`
	Replicas      int               `json:"replicas"`
	CapacityLease *CapacityLease    `json:"capacityLease,omitempty"`
	RequestedAt   time.Time         `json:"requestedAt"`
}

type CapacityLease struct {
	ExpiresAt       time.Time `json:"expiresAt"`
	RestoreReplicas int       `json:"restoreReplicas"`
}

type ObservedRelease struct {
	SchemaVersion   string                 `json:"schemaVersion"`
	System          string                 `json:"system"`
	Phase           string                 `json:"phase"`
	DesiredVersion  string                 `json:"desiredVersion,omitempty"`
	RunningVersion  string                 `json:"runningVersion,omitempty"`
	PID             int                    `json:"pid,omitempty"`
	Restarts        int                    `json:"restarts"`
	PublicPort      int                    `json:"publicPort"`
	InternalPort    int                    `json:"internalPort,omitempty"`
	DesiredReplicas int                    `json:"desiredReplicas"`
	ReadyReplicas   int                    `json:"readyReplicas"`
	ReplicaPIDs     []int                  `json:"replicaPids,omitempty"`
	CapacityLease   *ObservedCapacityLease `json:"capacityLease,omitempty"`
	Healthy         bool                   `json:"healthy"`
	Message         string                 `json:"message,omitempty"`
	Services        []ObservedService      `json:"services,omitempty"`
	Node            string                 `json:"node,omitempty"`
	UpdatedAt       time.Time              `json:"updatedAt"`
}

type ObservedCapacityLease struct {
	ExpiresAt       time.Time `json:"expiresAt"`
	RestoreReplicas int       `json:"restoreReplicas"`
	Phase           string    `json:"phase"`
}

type ObservedService struct {
	Name     string `json:"name"`
	Binding  string `json:"binding,omitempty"`
	Kind     string `json:"kind"`
	Engine   string `json:"engine"`
	Phase    string `json:"phase"`
	Endpoint string `json:"endpoint,omitempty"`
}

type RuntimeControl struct {
	ID          string    `json:"id"`
	Action      string    `json:"action"`
	RequestedAt time.Time `json:"requestedAt"`
}

type PublishReleaseInput struct {
	ArtifactPath string            `json:"artifactPath"`
	Command      []string          `json:"command"`
	Environment  map[string]string `json:"environment,omitempty"`
	HealthPath   string            `json:"healthPath"`
	PublicPort   int               `json:"publicPort"`
}

// StagedArtifact is an immutable, content-addressed application artifact. The
// storage key is deliberately opaque to remote agents; only the control plane
// needs provider access in order to create and consume it.
type StagedArtifact struct {
	Key         string `json:"key"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	Filename    string `json:"filename,omitempty"`
}

// StagedReleaseInput publishes an artifact which the control plane has already
// uploaded. It is the server-side counterpart to PublishReleaseInput, whose
// ArtifactPath is intentionally only suitable for local SDK callers.
type StagedReleaseInput struct {
	Artifact    StagedArtifact    `json:"artifact"`
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment,omitempty"`
	HealthPath  string            `json:"healthPath"`
	PublicPort  int               `json:"publicPort"`
}

// ControlPlaneArtifactKey returns the only valid m1 key for a staged digest.
// Binding the key to the digest prevents a durable database record from being
// redirected to an unrelated mutable object.
func ControlPlaneArtifactKey(digest string) (string, error) {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size || digest != strings.ToLower(digest) {
		return "", fmt.Errorf("artifact requires a lowercase SHA-256 digest")
	}
	return "control-plane/artifacts/sha256/" + digest + ".tar.gz", nil
}

// StageControlPlaneArtifact uploads bytes to m1 under their content digest.
// The caller remains responsible for recording workspace ownership and actor
// attribution in its durable control-plane store.
func (c *Client) StageControlPlaneArtifact(ctx context.Context, artifact []byte, filename, contentType string) (StagedArtifact, error) {
	if len(artifact) == 0 {
		return StagedArtifact{}, fmt.Errorf("artifact is empty")
	}
	if contentType == "" {
		contentType = "application/gzip"
	}
	sum := sha256.Sum256(artifact)
	digest := hex.EncodeToString(sum[:])
	key, err := ControlPlaneArtifactKey(digest)
	if err != nil {
		return StagedArtifact{}, err
	}
	if err := c.m1.Put(ctx, key, artifact, contentType); err != nil {
		return StagedArtifact{}, fmt.Errorf("upload control-plane artifact: %w", err)
	}
	cleanFilename := ""
	if filename != "" {
		cleanFilename = filepath.Base(filename)
	}
	return StagedArtifact{Key: key, SHA256: digest, Size: int64(len(artifact)), ContentType: contentType, Filename: cleanFilename}, nil
}

// VerifyStagedArtifact re-reads the server-side object and validates its key,
// byte length, and digest immediately before it can become a desired release.
func (c *Client) VerifyStagedArtifact(ctx context.Context, artifact StagedArtifact) error {
	expectedKey, err := ControlPlaneArtifactKey(artifact.SHA256)
	if err != nil {
		return err
	}
	if artifact.Key != expectedKey {
		return fmt.Errorf("staged artifact key is not canonical for digest %s", artifact.SHA256)
	}
	data, err := c.m1.GetBytes(ctx, artifact.Key)
	if err != nil {
		return fmt.Errorf("read staged artifact: %w", err)
	}
	if int64(len(data)) != artifact.Size {
		return fmt.Errorf("staged artifact size mismatch: expected %d, got %d", artifact.Size, len(data))
	}
	sum := sha256.Sum256(data)
	if actual := hex.EncodeToString(sum[:]); actual != artifact.SHA256 {
		return fmt.Errorf("staged artifact digest mismatch: expected %s, got %s", artifact.SHA256, actual)
	}
	return nil
}

// PublishStagedRelease makes a previously staged immutable artifact desired by
// a System. It does not accept provider credentials or a local filesystem path.
func (c *Client) PublishStagedRelease(ctx context.Context, system System, input StagedReleaseInput) (ReleaseManifest, error) {
	if err := system.Validate(); err != nil {
		return ReleaseManifest{}, err
	}
	if len(input.Command) == 0 || input.PublicPort < 1 || input.PublicPort > 65535 || !strings.HasPrefix(input.HealthPath, "/") {
		return ReleaseManifest{}, fmt.Errorf("release requires a command, absolute health path, and valid public port")
	}
	if input.Artifact.Key == "" || input.Artifact.Size < 1 {
		return ReleaseManifest{}, fmt.Errorf("release requires a valid staged artifact")
	}
	if err := c.VerifyStagedArtifact(ctx, input.Artifact); err != nil {
		return ReleaseManifest{}, err
	}
	manifest := ReleaseManifest{
		SchemaVersion: "v1", System: system.Metadata.Name, Version: input.Artifact.SHA256[:12],
		ArtifactKey: input.Artifact.Key, ArtifactSHA: input.Artifact.SHA256,
		Command: append([]string(nil), input.Command...), Environment: cloneStrings(input.Environment),
		HealthPath: input.HealthPath, PublicPort: input.PublicPort, Replicas: 1, RequestedAt: time.Now().UTC(),
	}
	if err := c.m1.PutJSON(ctx, releaseKey(system, manifest.Version), manifest); err != nil {
		return ReleaseManifest{}, fmt.Errorf("persist release manifest: %w", err)
	}
	if err := c.m1.PutJSON(ctx, desiredKey(system), manifest); err != nil {
		return ReleaseManifest{}, fmt.Errorf("set desired release: %w", err)
	}
	return manifest, nil
}

func (c *Client) PublishRelease(ctx context.Context, system System, input PublishReleaseInput) (ReleaseManifest, error) {
	manifest, err := c.StageRelease(ctx, system, input)
	if err != nil {
		return ReleaseManifest{}, err
	}
	if err := c.m1.PutJSON(ctx, desiredKey(system), manifest); err != nil {
		return ReleaseManifest{}, fmt.Errorf("set desired release: %w", err)
	}
	return manifest, nil
}

// StageRelease stores an immutable content-addressed release without changing
// desired production state. Change planning uses this before authorization.
func (c *Client) StageRelease(ctx context.Context, system System, input PublishReleaseInput) (ReleaseManifest, error) {
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
		Environment: input.Environment, HealthPath: input.HealthPath, PublicPort: input.PublicPort, Replicas: 1, RequestedAt: time.Now().UTC(),
	}
	if err := c.m1.PutJSON(ctx, releaseKey(system, version), manifest); err != nil {
		return ReleaseManifest{}, fmt.Errorf("persist release manifest: %w", err)
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
	return ApplyResult{}, fmt.Errorf("node gateway enrollment is required; account-wide m1 credentials cannot be installed on a tenant host")
}

type NodeBootstrapConfig struct {
	GatewayURL      string
	EnrollmentID    string
	EnrollmentToken string
}

// BootstrapSystemHostViaGateway installs only a one-time enrollment
// capability. Provider credentials remain in the control plane.
func (c *Client) BootstrapSystemHostViaGateway(ctx context.Context, system System, nodeBinary []byte, gateway NodeBootstrapConfig) (ApplyResult, error) {
	if err := system.Validate(); err != nil {
		return ApplyResult{}, err
	}
	parsed, err := url.Parse(strings.TrimSpace(gateway.GatewayURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ApplyResult{}, fmt.Errorf("node gateway URL must be an absolute HTTPS URL")
	}
	if gateway.EnrollmentID == "" || !strings.HasPrefix(gateway.EnrollmentToken, "ce_") {
		return ApplyResult{}, fmt.Errorf("valid one-time node enrollment is required")
	}
	host := system.Spec.Constraints.Host
	if host.Count != 1 {
		return ApplyResult{}, fmt.Errorf("node runtime experiment currently requires exactly one host")
	}
	runtimePlan, err := CompileRuntimePlan(system)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := c.m1.PutJSON(ctx, strings.TrimRight(system.Spec.M1.Prefix, "/")+"/runtime-plan.json", runtimePlan); err != nil {
		return ApplyResult{}, fmt.Errorf("persist runtime plan: %w", err)
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
	bootstrap, err := renderNodeBootstrap(system, url, digest, publicPort, gateway)
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

// RecoverSystemHostExposure is the explicit operator-recovery path for an
// exact, durable public-endpoint intent that previously escalated after an
// ambiguous provider response. Ordinary reconciliation remains lookup-only
// and cannot reopen a terminal intent. A human-governed retry may call this
// method to recover a now-visible exact policy or idempotently finish that
// same named/owned policy.
func (c *Client) RecoverSystemHostExposure(ctx context.Context, system System) (State, error) {
	state, err := c.SystemHostStatus(ctx, system)
	if err != nil {
		return State{}, err
	}
	result := ApplyResult{State: state}
	if err := c.exposeSystemHostMode(ctx, system, &result, true); err != nil {
		return State{}, err
	}
	return result.State, nil
}

func (c *Client) exposeSystemHost(ctx context.Context, system System, result *ApplyResult) error {
	return c.exposeSystemHostMode(ctx, system, result, false)
}

func (c *Client) exposeSystemHostMode(ctx context.Context, system System, result *ApplyResult, recoverEscalated bool) error {
	port, err := systemPublicPort(system)
	if err != nil {
		return err
	}
	spec := SystemHostSpec(system, "true")
	key := stateKey(spec)
	var current State
	foundState, etag, err := c.m1.GetJSONVersion(ctx, key, &current)
	if err != nil {
		return fmt.Errorf("read public endpoint intent state: %w", err)
	}
	if !foundState {
		return fmt.Errorf("public endpoint host state is missing")
	}
	result.State = current
	recoverableEscalation := recoverEscalated && result.State.Phase == "escalated" && result.State.ExposureIntent != nil && result.State.ExposureIntent.Phase == "escalated" && result.State.ExposureIntent.MutationUnresolved
	if (result.State.Phase != "ready" && !recoverableEscalation) || len(result.State.Resources) != 1 || result.State.Resources[0].Status == "DELETED" {
		return fmt.Errorf("public endpoint currently requires exactly one host")
	}
	serverID := result.State.Resources[0].ID
	name, ownership := systemEndpointPolicyIdentity(system)
	intent := result.State.ExposureIntent
	createCapable := false
	if intent == nil {
		now := time.Now().UTC()
		intent = &ExposureIntent{OperationID: "expose-" + newID(), ServerID: serverID, Name: name, Ownership: ownership, Protocol: "tcp", Port: port, Phase: "creating", MutationUnresolved: true, CreatedAt: now, AttemptedAt: &now}
		result.State.ExposureIntent = intent
		result.State.Failure = ""
		if _, written, writeErr := c.m1.PutJSONIfMatch(ctx, key, etag, result.State); writeErr != nil {
			return fmt.Errorf("persist public endpoint intent: %w", writeErr)
		} else if !written {
			return fmt.Errorf("public endpoint intent was concurrently claimed")
		}
		createCapable = true
	} else if intent.ServerID != serverID || intent.Name != name || intent.Ownership != ownership || intent.Protocol != "tcp" || intent.Port != port {
		cause := fmt.Errorf("durable public endpoint intent does not match the requested exposure")
		return c.escalateExposure(context.WithoutCancel(ctx), key, result, intent.OperationID, cause)
	}
	operationID := intent.OperationID
	matchingPolicies := 0
	var existingPolicy NetworkPolicy
	for _, existing := range result.State.NetworkPolicies {
		if existing.Name == name && existing.Ownership == ownership && existing.Protocol == "tcp" && existing.Port == port {
			matchingPolicies++
			existingPolicy = existing
		}
	}
	if matchingPolicies > 1 {
		cause := &compute.DuplicateManagedResourceError{Kind: "network policy state", Identity: name, Count: matchingPolicies}
		return c.escalateExposure(context.WithoutCancel(ctx), key, result, operationID, cause)
	}
	if matchingPolicies == 1 {
		return c.completeExposureMode(ctx, key, result, operationID, compute.SecurityPolicy{ID: existingPolicy.ID, PortID: existingPolicy.PortID, RuleID: existingPolicy.RuleID, Port: existingPolicy.Port}, recoverableEscalation)
	}
	request := compute.ManagedTCPExposureRequest{ServerID: serverID, Name: name, Ownership: ownership, Port: port}
	if !createCapable {
		if intent.Phase == "escalated" && recoverEscalated {
			policy, recovered, lookupErr := c.compute.FindManagedTCPExposure(ctx, request)
			if lookupErr == nil && recovered {
				return c.completeExposureMode(ctx, key, result, operationID, policy, true)
			}
			if compute.IsDuplicateManagedResource(lookupErr) {
				return lookupErr
			}
			// The exact policy may be absent or partially present (for example,
			// its rule exists but the timed-out port attachment does not). This
			// path is reachable only from an explicit human retry. The provider
			// ensure is scoped by the persisted name and ownership digest and is
			// itself duplicate/ambiguity detecting.
			policy, ensureErr := c.compute.ExposeManagedTCP(ctx, request)
			if ensureErr != nil {
				return c.escalateExposure(context.WithoutCancel(ctx), key, result, operationID, ensureErr)
			}
			return c.completeExposureMode(ctx, key, result, operationID, policy, true)
		}
		if intent.Phase != "creating" {
			return fmt.Errorf("public endpoint intent is in terminal phase %s", intent.Phase)
		}
		policy, recovered, lookupErr := c.compute.FindManagedTCPExposure(ctx, request)
		if lookupErr != nil {
			return c.escalateExposure(context.WithoutCancel(ctx), key, result, operationID, lookupErr)
		}
		if !recovered {
			cause := &compute.AmbiguousManagedResourceError{Kind: "network policy", Identity: name, Cause: fmt.Errorf("persisted in-flight exposure has no visible exact policy")}
			return c.escalateExposure(context.WithoutCancel(ctx), key, result, operationID, cause)
		}
		return c.completeExposure(ctx, key, result, operationID, policy)
	}
	policy, err := c.compute.ExposeManagedTCP(ctx, request)
	if err != nil {
		if compute.IsDuplicateManagedResource(err) || compute.IsAmbiguousManagedResource(err) {
			return c.escalateExposure(context.WithoutCancel(ctx), key, result, operationID, err)
		}
		if persistErr := c.recordExposureFailure(context.WithoutCancel(ctx), key, result, operationID, err); persistErr != nil {
			return fmt.Errorf("%v; persist interrupted exposure: %w", err, persistErr)
		}
		return err
	}
	return c.completeExposure(ctx, key, result, operationID, policy)
}

func (c *Client) mutateExposureState(ctx context.Context, key, operationID string, mutate func(*State, *ExposureIntent) (bool, error)) (State, error) {
	for retries := 0; retries < 8; retries++ {
		var current State
		found, etag, err := c.m1.GetJSONVersion(ctx, key, &current)
		if err != nil {
			return State{}, err
		}
		if !found || current.ExposureIntent == nil || current.ExposureIntent.OperationID != operationID || current.Phase == "destroying" || current.Phase == "destroyed" || current.ExposureIntent.Phase == "destroying" || current.ExposureIntent.Phase == "destroyed" {
			return State{}, &lifecycleFenceError{message: fmt.Sprintf("public endpoint operation %s lost its lifecycle fence", operationID)}
		}
		changed, err := mutate(&current, current.ExposureIntent)
		if err != nil {
			return State{}, err
		}
		if !changed {
			return current, nil
		}
		_, written, err := c.m1.PutJSONIfMatch(ctx, key, etag, current)
		if err != nil {
			return State{}, err
		}
		if written {
			return current, nil
		}
	}
	return State{}, fmt.Errorf("public endpoint operation %s state changed continuously during transition", operationID)
}

func (c *Client) completeExposure(ctx context.Context, key string, result *ApplyResult, operationID string, policy compute.SecurityPolicy) error {
	return c.completeExposureMode(ctx, key, result, operationID, policy, false)
}

func (c *Client) completeExposureMode(ctx context.Context, key string, result *ApplyResult, operationID string, policy compute.SecurityPolicy, recoverEscalated bool) error {
	persisted, err := c.mutateExposureState(ctx, key, operationID, func(current *State, intent *ExposureIntent) (bool, error) {
		mapped := NetworkPolicy{ID: policy.ID, PortID: policy.PortID, RuleID: policy.RuleID, Name: intent.Name, Ownership: intent.Ownership, Protocol: "tcp", Port: policy.Port}
		recovering := recoverEscalated && current.Phase == "escalated" && intent.Phase == "escalated" && intent.MutationUnresolved
		if (!recovering && current.Phase != "ready") || (!recovering && intent.Phase != "creating" && intent.Phase != "ready") {
			return false, &lifecycleFenceError{message: fmt.Sprintf("public endpoint operation %s cannot complete in phase %s", operationID, current.Phase)}
		}
		for _, existing := range current.NetworkPolicies {
			if existing.ID == mapped.ID {
				if intent.Phase == "ready" {
					return false, nil
				}
				intent.Phase = "ready"
				intent.MutationUnresolved = false
				current.Failure = ""
				return true, nil
			}
		}
		current.NetworkPolicies = append(current.NetworkPolicies, mapped)
		current.Phase = "ready"
		intent.Phase = "ready"
		intent.MutationUnresolved = false
		current.Failure = ""
		return true, nil
	})
	if err != nil {
		if isLifecycleFenceError(err) && policy.ID != "" {
			if cleanupErr := c.cleanupExposureAfterFence(context.WithoutCancel(ctx), key, operationID, policy.ID); cleanupErr != nil {
				return fmt.Errorf("%v; cleanup stale network policy %s: %w", err, policy.ID, cleanupErr)
			}
		}
		return err
	}
	result.State = persisted
	if result.ReceiptKey != "" {
		if err := c.m1.PutJSON(ctx, result.ReceiptKey, result); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) cleanupExposureAfterFence(ctx context.Context, key, operationID, policyID string) error {
	var claimed State
	for retries := 0; retries < 8; retries++ {
		var current State
		found, etag, err := c.m1.GetJSONVersion(ctx, key, &current)
		if err != nil {
			return err
		}
		if !found || current.ExposureIntent == nil || current.ExposureIntent.OperationID != operationID {
			return nil
		}
		if !current.ExposureIntent.MutationUnresolved || current.ExposureIntent.Phase == "destroyed" {
			return nil
		}
		// A live Destroy owns provider reconciliation while the state is
		// destroying. Only claim cleanup after that owner has escalated.
		if current.Phase != "escalated" {
			return nil
		}
		current.Phase = "cleanup"
		current.ExposureIntent.Phase = "cleaning"
		_, written, err := c.m1.PutJSONIfMatch(ctx, key, etag, current)
		if err != nil {
			return err
		}
		if written {
			claimed = current
			break
		}
	}
	if claimed.Phase != "cleanup" {
		return fmt.Errorf("public endpoint cleanup could not acquire its lifecycle fence")
	}
	deleteErr := c.compute.DeleteSecurityPolicy(ctx, policyID)
	for retries := 0; retries < 8; retries++ {
		var current State
		found, etag, err := c.m1.GetJSONVersion(ctx, key, &current)
		if err != nil {
			return err
		}
		if !found || current.Phase != "cleanup" || current.ExposureIntent == nil || current.ExposureIntent.OperationID != operationID || current.ExposureIntent.Phase != "cleaning" {
			return &lifecycleFenceError{message: "public endpoint cleanup lost its lifecycle fence"}
		}
		current.Phase = "escalated"
		current.ExposureIntent.Phase = "destroyed"
		current.ExposureIntent.MutationUnresolved = false
		current.NetworkPolicies = removeNetworkPolicy(current.NetworkPolicies, policyID)
		if deleteErr != nil {
			current.ExposureIntent.Phase = "escalated"
			current.ExposureIntent.MutationUnresolved = true
			current.Failure = fmt.Sprintf("cleanup stale network policy %s: %v", policyID, deleteErr)
		}
		_, written, err := c.m1.PutJSONIfMatch(ctx, key, etag, current)
		if err != nil {
			return err
		}
		if written {
			return deleteErr
		}
	}
	return fmt.Errorf("public endpoint cleanup state changed continuously")
}

func removeNetworkPolicy(policies []NetworkPolicy, id string) []NetworkPolicy {
	filtered := policies[:0]
	for _, policy := range policies {
		if policy.ID != id {
			filtered = append(filtered, policy)
		}
	}
	return filtered
}

func (c *Client) recordExposureFailure(ctx context.Context, key string, result *ApplyResult, operationID string, cause error) error {
	persisted, err := c.mutateExposureState(ctx, key, operationID, func(current *State, intent *ExposureIntent) (bool, error) {
		if current.Phase != "ready" || intent.Phase != "creating" {
			return false, &lifecycleFenceError{message: fmt.Sprintf("public endpoint operation %s cannot record failure", operationID)}
		}
		current.Failure = cause.Error()
		return true, nil
	})
	if err == nil {
		result.State = persisted
	}
	return err
}

func (c *Client) escalateExposure(ctx context.Context, key string, result *ApplyResult, operationID string, cause error) error {
	persisted, err := c.mutateExposureState(ctx, key, operationID, func(current *State, intent *ExposureIntent) (bool, error) {
		if current.Phase == "escalated" && intent.Phase == "escalated" {
			return false, nil
		}
		if current.Phase != "ready" || intent.Phase != "creating" {
			return false, &lifecycleFenceError{message: fmt.Sprintf("public endpoint operation %s cannot escalate", operationID)}
		}
		current.Phase = "escalated"
		intent.Phase = "escalated"
		current.Failure = cause.Error()
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("%v; persist escalated exposure: %w", cause, err)
	}
	result.State = persisted
	return cause
}

func systemEndpointPolicyIdentity(system System) (string, string) {
	prefix := strings.TrimRight(system.Spec.M1.Prefix, "/")
	digest := sha256.Sum256([]byte(prefix + "\x00" + system.Metadata.Name))
	encoded := hex.EncodeToString(digest[:])
	base := system.Metadata.Name
	if len(base) > 32 {
		base = base[:32]
	}
	return "canter-" + base + "-" + encoded[:16], "sha256:" + encoded
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
		observed.DesiredReplicas = 0
		observed.ReadyReplicas = 0
		observed.ReplicaPIDs = nil
		observed.Healthy = false
		observed.Message = "compute host destroyed"
		for index := range observed.Services {
			observed.Services[index].Phase = "stopped"
			observed.Services[index].Endpoint = ""
		}
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
		if service.Networking == "public" && service.Readiness.Protocol == "http" && service.Readiness.Port > 0 {
			return service.Readiness.Port, nil
		}
	}
	return 0, fmt.Errorf("system has no HTTP service readiness port")
}

func renderNodeBootstrap(system System, nodeURL, digest string, publicPort int, gateway NodeBootstrapConfig) (string, error) {
	if err := system.Validate(); err != nil {
		return "", err
	}
	parsed, err := url.Parse(strings.TrimSpace(gateway.GatewayURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("node gateway URL must be an absolute HTTPS URL")
	}
	if gateway.EnrollmentID == "" || !strings.HasPrefix(gateway.EnrollmentToken, "ce_") {
		return "", fmt.Errorf("valid one-time node enrollment is required")
	}
	systemArg, err := systemdQuoteArg(system.Metadata.Name)
	if err != nil {
		return "", err
	}
	gatewayArg, err := systemdQuoteArg(strings.TrimRight(gateway.GatewayURL, "/"))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`set -eu
curl -fsSL --proto '=https' --tlsv1.2 %s -o /usr/local/bin/canter-node
printf '%%s  %%s\n' %s /usr/local/bin/canter-node | sha256sum --check --status
chmod 0755 /usr/local/bin/canter-node
install -d -m 0750 /etc/canter /var/lib/canter-node
install -m 0600 /dev/null /run/canter-enroll.conf
printf 'header = "Authorization: Bearer %%s"\n' %s > /run/canter-enroll.conf
curl -fsS --proto '=https' --tlsv1.2 --request POST --config /run/canter-enroll.conf %s | python3 -c 'import json,sys; value=json.load(sys.stdin)["nodeToken"]; assert value.startswith("cn_"); print(value)' > /etc/canter/node.token
rm -f /run/canter-enroll.conf
chmod 0600 /etc/canter/node.token
cat > /etc/systemd/system/canter-node.service <<'EOF'
[Unit]
Description=Canter application reconciler
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/canter-node --system %s --gateway %s --token-file /etc/canter/node.token --public-port %d
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now canter-node.service
for attempt in $(seq 1 30); do systemctl is-active --quiet canter-node.service && exit 0; sleep 1; done
systemctl status --no-pager canter-node.service || true
exit 1`, shellQuote(nodeURL), shellQuote(digest), shellQuote(gateway.EnrollmentToken), shellQuote(strings.TrimRight(gateway.GatewayURL, "/")+"/v1/node/enrollments/"+url.PathEscape(gateway.EnrollmentID)+"/exchange"), systemArg, gatewayArg, publicPort), nil
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

// systemdQuoteArg emits one literal ExecStart argument. Percent is doubled to
// prevent systemd specifier expansion even if a future validated value permits
// it; control characters are rejected rather than normalized.
func systemdQuoteArg(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("systemd argument contains control characters")
	}
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "%", "%%")
	return `"` + value + `"`, nil
}
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
