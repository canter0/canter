package sdk

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/canter0/canter/internal/model"
	"github.com/canter0/canter/internal/provider/compute"
	"github.com/canter0/canter/internal/provider/m1"
)

type Client struct {
	model                 modelProvider
	compute               computeProvider
	m1                    objectStore
	createReconcileGrace  time.Duration
	managedLookupInterval time.Duration
}

type modelProvider interface {
	Probe(context.Context) model.ProbeResult
	Compile(context.Context, string, any) (string, int64, error)
}

type computeProvider interface {
	Probe(context.Context) compute.ProbeResult
	Resolve(context.Context, string, string) (compute.Shape, string, []string, error)
	CreateManaged(context.Context, compute.ManagedServerRequest) (compute.Server, error)
	FindManagedServers(context.Context, string, string, string) ([]compute.Server, error)
	Server(context.Context, string) (compute.Server, error)
	WaitActive(context.Context, string) (compute.Server, error)
	Delete(context.Context, string) error
	ExposeManagedTCP(context.Context, compute.ManagedTCPExposureRequest) (compute.SecurityPolicy, error)
	FindManagedTCPExposure(context.Context, compute.ManagedTCPExposureRequest) (compute.SecurityPolicy, bool, error)
	DeleteSecurityPolicy(context.Context, string) error
}

type objectStore interface {
	Probe(context.Context) m1.ProbeResult
	Put(context.Context, string, []byte, string) error
	PutJSON(context.Context, string, any) error
	PutJSONIfAbsent(context.Context, string, any) (string, bool, error)
	PutJSONIfMatch(context.Context, string, string, any) (string, bool, error)
	Get(context.Context, string, any) error
	GetBytes(context.Context, string) ([]byte, error)
	GetJSONVersion(context.Context, string, any) (bool, string, error)
	GetOptional(context.Context, string, any) (bool, error)
	PresignPut(context.Context, string, time.Duration) (string, error)
	PresignGet(context.Context, string, time.Duration) (string, error)
}

type ProbeReport struct {
	StartedAt time.Time           `json:"startedAt"`
	ElapsedMS int64               `json:"elapsedMs"`
	Model     model.ProbeResult   `json:"model"`
	Compute   compute.ProbeResult `json:"compute"`
	M1        m1.ProbeResult      `json:"m1"`
}

func (r ProbeReport) OK() bool { return r.Model.OK && r.Compute.OK && r.M1.OK }

type Checkpoint struct {
	ID        string    `json:"id"`
	Sandbox   string    `json:"sandbox"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"createdAt"`
	Verified  bool      `json:"verified"`
}

type BootProof struct {
	Sandbox       string `json:"sandbox"`
	Status        string `json:"status"`
	ExitCode      int    `json:"exitCode"`
	Hostname      string `json:"hostname"`
	MessageBase64 string `json:"messageBase64,omitempty"`
}

type State struct {
	Sandbox         string          `json:"sandbox"`
	Phase           string          `json:"phase"`
	Class           string          `json:"class"`
	Image           string          `json:"image"`
	Resources       []Resource      `json:"resources"`
	Attempts        []Attempt       `json:"attempts,omitempty"`
	ProofKeys       []string        `json:"proofKeys"`
	CreatedAt       time.Time       `json:"createdAt"`
	DestroyedAt     *time.Time      `json:"destroyedAt,omitempty"`
	Failure         string          `json:"failure,omitempty"`
	NetworkPolicies []NetworkPolicy `json:"networkPolicies,omitempty"`
	CreationIntent  *CreationIntent `json:"creationIntent,omitempty"`
	ExposureIntent  *ExposureIntent `json:"exposureIntent,omitempty"`
}

type CreationIntent struct {
	OperationID  string                  `json:"operationId"`
	DesiredClass string                  `json:"desiredClass"`
	DesiredImage string                  `json:"desiredImage"`
	ShapeID      string                  `json:"shapeId"`
	ImageID      string                  `json:"imageId"`
	NetworkIDs   []string                `json:"networkIds"`
	Resources    []ComputeResourceIntent `json:"resources"`
	Phase        string                  `json:"phase"`
	CreatedAt    time.Time               `json:"createdAt"`
}

type ComputeResourceIntent struct {
	Replica           int        `json:"replica"`
	Name              string     `json:"name"`
	ProofKey          string     `json:"proofKey"`
	NetworkIndex      int        `json:"networkIndex"`
	ResourceID        string     `json:"resourceId,omitempty"`
	Phase             string     `json:"phase"`
	AttemptID         string     `json:"attemptId,omitempty"`
	CreateAttemptedAt *time.Time `json:"createAttemptedAt,omitempty"`
	ReconcileUntil    *time.Time `json:"reconcileUntil,omitempty"`
}

type ExposureIntent struct {
	OperationID        string     `json:"operationId"`
	ServerID           string     `json:"serverId"`
	Name               string     `json:"name"`
	Ownership          string     `json:"ownership"`
	Protocol           string     `json:"protocol"`
	Port               int        `json:"port"`
	Phase              string     `json:"phase"`
	MutationUnresolved bool       `json:"mutationUnresolved,omitempty"`
	CreatedAt          time.Time  `json:"createdAt"`
	AttemptedAt        *time.Time `json:"attemptedAt,omitempty"`
}

type NetworkPolicy struct {
	ID        string `json:"id"`
	PortID    string `json:"portId"`
	RuleID    string `json:"ruleId"`
	Name      string `json:"name,omitempty"`
	Ownership string `json:"ownership,omitempty"`
	Protocol  string `json:"protocol"`
	Port      int    `json:"port"`
}

type Attempt struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Failure string `json:"failure"`
}

type Resource struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Address string `json:"address,omitempty"`
}

type ApplyResult struct {
	OperationID string `json:"operationId"`
	Plan        Plan   `json:"plan"`
	State       State  `json:"state"`
	ReceiptKey  string `json:"receiptKey"`
	ElapsedMS   int64  `json:"elapsedMs"`
}

func NewFromEnv() (*Client, error) {
	computeClient, err := compute.NewFromEnv()
	if err != nil {
		return nil, err
	}
	m1Client, err := m1.NewFromEnv()
	if err != nil {
		return nil, err
	}
	return &Client{model: model.Client{}, compute: computeClient, m1: m1Client}, nil
}

func (c *Client) Probe(ctx context.Context) ProbeReport {
	started := time.Now()
	report := ProbeReport{StartedAt: started}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); report.Model = c.model.Probe(ctx) }()
	go func() { defer wg.Done(); report.Compute = c.compute.Probe(ctx) }()
	go func() { defer wg.Done(); report.M1 = c.m1.Probe(ctx) }()
	wg.Wait()
	report.ElapsedMS = time.Since(started).Milliseconds()
	return report
}

type Plan struct {
	SchemaVersion string      `json:"schemaVersion"`
	Summary       string      `json:"summary"`
	Operations    []Operation `json:"operations"`
	Model         string      `json:"model"`
	LatencyMS     int64       `json:"latencyMs"`
}

type Operation struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Class    string `json:"class,omitempty"`
	Image    string `json:"image,omitempty"`
	Replicas int    `json:"replicas,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
}

func (c *Client) Plan(ctx context.Context, spec Spec) (Plan, error) {
	if err := spec.Validate(); err != nil {
		return Plan{}, err
	}
	b, _ := json.Marshal(spec)
	prompt := `You are Canter's intent compiler. Convert the supplied validated sandbox spec into JSON only.
The exact schema is {"schemaVersion":"v1","summary":"...","operations":[...]}. Emit exactly two operations in this order:
1. {"kind":"m1.ensure","name":metadata.name,"prefix":spec.m1.prefix}
2. {"kind":"compute.ensure","name":metadata.name,"class":spec.compute.class,"image":spec.compute.image,"replicas":spec.compute.replicas}
Do not add provider names, credentials, IDs, commands, fields, or markdown. Spec: ` + string(b)
	var plan Plan
	modelName, latency, err := c.model.Compile(ctx, prompt, &plan)
	if err != nil {
		return Plan{}, err
	}
	plan.Model = modelName
	plan.LatencyMS = latency
	if err := validatePlan(spec, plan); err != nil {
		return Plan{}, fmt.Errorf("model plan rejected: %w", err)
	}
	return plan, nil
}

func validatePlan(spec Spec, plan Plan) error {
	if plan.SchemaVersion != "v1" || len(plan.Operations) != 2 {
		return fmt.Errorf("unexpected plan shape")
	}
	want := []Operation{
		{Kind: "m1.ensure", Name: spec.Metadata.Name, Prefix: spec.Spec.M1.Prefix},
		{Kind: "compute.ensure", Name: spec.Metadata.Name, Class: spec.Spec.Compute.Class, Image: spec.Spec.Compute.Image, Replicas: spec.Spec.Compute.Replicas},
	}
	for i := range want {
		if plan.Operations[i] != want[i] {
			return fmt.Errorf("operation %d differs from the validated spec", i+1)
		}
	}
	return nil
}

func (c *Client) Checkpoint(ctx context.Context, sandbox, prefix, message string) (Checkpoint, error) {
	if !safeName.MatchString(sandbox) {
		return Checkpoint{}, fmt.Errorf("invalid sandbox name")
	}
	if prefix == "" || strings.Contains(prefix, "..") || strings.HasPrefix(prefix, "/") {
		return Checkpoint{}, fmt.Errorf("invalid m1 prefix")
	}
	cp := Checkpoint{ID: newID(), Sandbox: sandbox, Message: message, CreatedAt: time.Now().UTC()}
	key := strings.TrimRight(prefix, "/") + "/checkpoints/" + cp.ID + ".json"
	if err := c.m1.PutJSON(ctx, key, cp); err != nil {
		return Checkpoint{}, err
	}
	var readback Checkpoint
	if err := c.m1.Get(ctx, key, &readback); err != nil {
		return Checkpoint{}, fmt.Errorf("checkpoint write could not be read back: %w", err)
	}
	if readback.ID != cp.ID || readback.Message != cp.Message {
		return Checkpoint{}, fmt.Errorf("checkpoint readback did not match write")
	}
	cp.Verified = true
	return cp, nil
}

func (c *Client) Apply(ctx context.Context, spec Spec) (ApplyResult, error) {
	started := time.Now()
	if err := spec.Validate(); err != nil {
		return ApplyResult{}, err
	}
	stateKey := stateKey(spec)
	var prior State
	found, etag, err := c.m1.GetJSONVersion(ctx, stateKey, &prior)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("read existing sandbox state: %w", err)
	}
	if found && prior.Phase != "destroyed" && prior.Phase != "creating" {
		return ApplyResult{}, fmt.Errorf("sandbox %q already has live state in phase %s; inspect it with canter status", spec.Metadata.Name, prior.Phase)
	}
	plan, err := c.Plan(ctx, spec)
	if err != nil {
		return ApplyResult{}, err
	}
	state := prior
	if !found || prior.Phase == "destroyed" {
		state, err = c.createIntent(ctx, spec, stateKey, found, etag)
		if err != nil {
			return ApplyResult{}, err
		}
	} else if err := validateCreationIntent(spec, state); err != nil {
		return ApplyResult{}, err
	}
	if err := c.reconcileCreation(ctx, spec, stateKey, &state); err != nil {
		return ApplyResult{OperationID: state.CreationIntent.OperationID, Plan: plan, State: state, ElapsedMS: time.Since(started).Milliseconds()}, err
	}
	opID := state.CreationIntent.OperationID
	receiptKey := fmt.Sprintf("%s/receipts/%s.json", strings.TrimRight(spec.Spec.M1.Prefix, "/"), opID)
	result := ApplyResult{OperationID: opID, Plan: plan, State: state, ReceiptKey: receiptKey, ElapsedMS: time.Since(started).Milliseconds()}
	if err := c.m1.PutJSON(ctx, receiptKey, result); err != nil {
		return ApplyResult{}, err
	}
	return result, nil
}

func (c *Client) createIntent(ctx context.Context, spec Spec, key string, replacing bool, etag string) (State, error) {
	shape, imageID, networkIDs, err := c.compute.Resolve(ctx, spec.Spec.Compute.Class, spec.Spec.Compute.Image)
	if err != nil {
		return State{}, err
	}
	opID := newID()
	networkIDs = rotate(networkIDs, opID)
	createdAt := time.Now().UTC()
	intent := &CreationIntent{
		OperationID: opID, DesiredClass: spec.Spec.Compute.Class, DesiredImage: spec.Spec.Compute.Image,
		ShapeID: shape.ID, ImageID: imageID, NetworkIDs: append([]string(nil), networkIDs...), Phase: "creating", CreatedAt: createdAt,
	}
	proofKeys := make([]string, 0, spec.Spec.Compute.Replicas)
	for i := 0; i < spec.Spec.Compute.Replicas; i++ {
		proofKey := fmt.Sprintf("%s/proofs/%s-%d.json", strings.TrimRight(spec.Spec.M1.Prefix, "/"), opID, i+1)
		intent.Resources = append(intent.Resources, ComputeResourceIntent{Replica: i + 1, Name: fmt.Sprintf("canter-%s-%d", spec.Metadata.Name, i+1), ProofKey: proofKey, Phase: "pending"})
		proofKeys = append(proofKeys, proofKey)
	}
	state := State{
		Sandbox: spec.Metadata.Name, Phase: "creating", Class: spec.Spec.Compute.Class, Image: spec.Spec.Compute.Image,
		ProofKeys: proofKeys, CreatedAt: createdAt, CreationIntent: intent,
	}
	// This write is the mutation boundary: no provider create may occur before
	// the complete deterministic intent is durable.
	var written bool
	if replacing {
		_, written, err = c.m1.PutJSONIfMatch(ctx, key, etag, state)
	} else {
		_, written, err = c.m1.PutJSONIfAbsent(ctx, key, state)
	}
	if err != nil {
		return State{}, fmt.Errorf("persist compute creation intent: %w", err)
	}
	if !written {
		return State{}, fmt.Errorf("sandbox %q creation intent was concurrently claimed; retry after inspecting current state", spec.Metadata.Name)
	}
	return state, nil
}

func validateCreationIntent(spec Spec, state State) error {
	intent := state.CreationIntent
	if intent == nil || intent.OperationID == "" || intent.ShapeID == "" || intent.ImageID == "" || len(intent.NetworkIDs) == 0 {
		return fmt.Errorf("sandbox %q has legacy or incomplete creating state; refusing unsafe replay", spec.Metadata.Name)
	}
	if state.Sandbox != spec.Metadata.Name || intent.DesiredClass != spec.Spec.Compute.Class || intent.DesiredImage != spec.Spec.Compute.Image || len(intent.Resources) != spec.Spec.Compute.Replicas {
		return fmt.Errorf("sandbox %q creation intent does not match the requested spec", spec.Metadata.Name)
	}
	for index, resource := range intent.Resources {
		wantName := fmt.Sprintf("canter-%s-%d", spec.Metadata.Name, index+1)
		exhausted := resource.NetworkIndex == len(intent.NetworkIDs)
		unsafeCreating := resource.Phase == "creating" && (resource.AttemptID == "" || resource.CreateAttemptedAt == nil || resource.ReconcileUntil == nil)
		if resource.Replica != index+1 || resource.Name != wantName || resource.ProofKey == "" || resource.NetworkIndex < 0 || resource.NetworkIndex > len(intent.NetworkIDs) || (exhausted && resource.Phase != "pending") || unsafeCreating {
			return fmt.Errorf("sandbox %q has an invalid resource creation intent", spec.Metadata.Name)
		}
	}
	return nil
}

func (c *Client) creationReconcileGrace() time.Duration {
	if c.createReconcileGrace > 0 {
		return c.createReconcileGrace
	}
	return 30 * time.Second
}

func (c *Client) lookupInterval() time.Duration {
	if c.managedLookupInterval > 0 {
		return c.managedLookupInterval
	}
	return 250 * time.Millisecond
}

func (c *Client) reconcileCreation(ctx context.Context, spec Spec, key string, state *State) error {
	operationID := state.CreationIntent.OperationID
	for index := range state.CreationIntent.Resources {
		resourceIntent := &state.CreationIntent.Resources[index]
		if resourceIntent.Phase == "ready" {
			continue
		}
		for resourceIntent.NetworkIndex < len(state.CreationIntent.NetworkIDs) {
			server, found, err := c.reconcileManagedServer(ctx, spec, state, resourceIntent)
			if err != nil {
				if compute.IsDuplicateManagedResource(err) {
					return c.escalateCreation(context.WithoutCancel(ctx), key, state, operationID, err)
				}
				_ = c.recordCreationFailure(context.WithoutCancel(ctx), key, state, operationID, err, false)
				return err
			}
			if !found && resourceIntent.Phase == "creating" && resourceIntent.ReconcileUntil != nil && time.Now().Before(*resourceIntent.ReconcileUntil) {
				server, found, err = c.waitForManagedServerVisibility(ctx, spec, state, resourceIntent, *resourceIntent.ReconcileUntil)
				if err != nil {
					if compute.IsDuplicateManagedResource(err) {
						return c.escalateCreation(context.WithoutCancel(ctx), key, state, operationID, err)
					}
					_ = c.recordCreationFailure(context.WithoutCancel(ctx), key, state, operationID, err, false)
					return err
				}
				if !found {
					cause := fmt.Errorf("compute create for %s remained ambiguous after reconciliation grace", resourceIntent.Name)
					return c.escalateCreation(context.WithoutCancel(ctx), key, state, operationID, cause)
				}
			}
			if !found && resourceIntent.Phase == "creating" && resourceIntent.ReconcileUntil != nil && !time.Now().Before(*resourceIntent.ReconcileUntil) {
				cause := fmt.Errorf("compute create for %s remained ambiguous after reconciliation grace", resourceIntent.Name)
				return c.escalateCreation(context.WithoutCancel(ctx), key, state, operationID, cause)
			}
			if !found {
				proofURL, err := c.m1.PresignPut(ctx, resourceIntent.ProofKey, 15*time.Minute)
				if err != nil {
					return fmt.Errorf("create m1 proof URL: %w", err)
				}
				acquired, acquireErr := c.acquireResourceCreateAttempt(ctx, key, state, index)
				if acquireErr != nil {
					return acquireErr
				}
				*state = acquired
				resourceIntent = &state.CreationIntent.Resources[index]
				request := compute.ManagedServerRequest{
					Name: resourceIntent.Name, Sandbox: spec.Metadata.Name, OperationID: operationID,
					FlavorID: state.CreationIntent.ShapeID, ImageID: state.CreationIntent.ImageID, NetworkID: state.CreationIntent.NetworkIDs[resourceIntent.NetworkIndex], UserData: bootScript(spec, proofURL),
				}
				reconcileUntil := *resourceIntent.ReconcileUntil
				created, createErr := c.compute.CreateManaged(ctx, request)
				matches, lookupErr := c.compute.FindManagedServers(ctx, spec.Metadata.Name, operationID, resourceIntent.Name)
				if lookupErr != nil {
					if createErr != nil {
						return fmt.Errorf("create compute failed (%v) and reconciliation failed: %w", createErr, lookupErr)
					}
					if created.ID == "" {
						return fmt.Errorf("reconcile created compute %s failed and provider returned no resource: %w", resourceIntent.Name, lookupErr)
					}
					server = created
				} else {
					switch len(matches) {
					case 0:
						if createErr != nil {
							server, found, lookupErr = c.waitForManagedServerVisibility(ctx, spec, state, resourceIntent, reconcileUntil)
							if lookupErr != nil {
								if compute.IsDuplicateManagedResource(lookupErr) {
									return c.escalateCreation(context.WithoutCancel(ctx), key, state, operationID, lookupErr)
								}
								return lookupErr
							}
							if !found {
								cause := fmt.Errorf("compute create for %s remained ambiguous after reconciliation grace: %w", resourceIntent.Name, createErr)
								return c.escalateCreation(context.WithoutCancel(ctx), key, state, operationID, cause)
							}
							break
						}
						if created.ID == "" {
							return fmt.Errorf("provider returned an empty compute resource")
						}
						server = created
					case 1:
						server = matches[0]
					default:
						return c.escalateCreation(context.WithoutCancel(ctx), key, state, operationID, &compute.DuplicateManagedResourceError{Kind: "compute resource", Identity: resourceIntent.Name, Count: len(matches)})
					}
				}
				attached, err := c.persistResourceAttachment(ctx, key, operationID, index, resourceIntent.AttemptID, server)
				if err != nil {
					if cleanupErr := c.cleanupComputeAfterFence(context.WithoutCancel(ctx), key, operationID, index, resourceIntent.AttemptID, server); cleanupErr != nil {
						return fmt.Errorf("%v; cleanup unrecorded compute %s: %w", err, server.ID, cleanupErr)
					}
					return err
				}
				*state = attached
				resourceIntent = &state.CreationIntent.Resources[index]
			} else if resourceIntent.Phase == "creating" {
				attached, attachErr := c.persistResourceAttachment(ctx, key, operationID, index, resourceIntent.AttemptID, server)
				if attachErr != nil {
					if cleanupErr := c.cleanupComputeAfterFence(context.WithoutCancel(ctx), key, operationID, index, resourceIntent.AttemptID, server); cleanupErr != nil {
						return fmt.Errorf("%v; cleanup stale compute %s: %w", attachErr, server.ID, cleanupErr)
					}
					return attachErr
				}
				*state = attached
				resourceIntent = &state.CreationIntent.Resources[index]
			}
			active, waitErr := c.compute.WaitActive(ctx, server.ID)
			if waitErr == nil {
				activeState, persistErr := c.persistResourceActive(ctx, key, operationID, index, resourceIntent.AttemptID, active)
				if persistErr != nil {
					if isLifecycleFenceError(persistErr) {
						if cleanupErr := c.cleanupComputeAfterFence(context.WithoutCancel(ctx), key, operationID, index, resourceIntent.AttemptID, active); cleanupErr != nil {
							return fmt.Errorf("%v; cleanup late active compute %s: %w", persistErr, server.ID, cleanupErr)
						}
					}
					return persistErr
				}
				*state = activeState
				resourceIntent = &state.CreationIntent.Resources[index]
				break
			}
			if !compute.IsNetworkExhausted(waitErr) {
				_ = c.recordCreationFailure(context.WithoutCancel(ctx), key, state, operationID, waitErr, false)
				return fmt.Errorf("wait for compute %s: %w", server.ID, waitErr)
			}
			if err := c.compute.Delete(ctx, server.ID); err != nil {
				return fmt.Errorf("network allocation failed and cleanup of compute %s also failed: %w", server.ID, err)
			}
			next, persistErr := c.persistExhaustedNetwork(ctx, key, operationID, index, resourceIntent.AttemptID, server.ID)
			if persistErr != nil {
				return persistErr
			}
			*state = next
			resourceIntent = &state.CreationIntent.Resources[index]
		}
		if resourceIntent.Phase != "active" {
			return fmt.Errorf("all eligible compute networks exhausted their address capacity")
		}
		if err := waitForProof(ctx, c.m1, resourceIntent.ProofKey); err != nil {
			terminal := !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
			if persistErr := c.recordCreationFailure(context.WithoutCancel(ctx), key, state, operationID, err, terminal); persistErr != nil {
				return fmt.Errorf("%v; persist interrupted creation: %w", err, persistErr)
			}
			return err
		}
		ready, persistErr := c.persistResourceReady(ctx, key, operationID, index, resourceIntent.AttemptID)
		if persistErr != nil {
			return persistErr
		}
		*state = ready
	}
	ready, err := c.persistCreationReady(ctx, key, operationID)
	if err != nil {
		return err
	}
	*state = ready
	return nil
}

func (c *Client) acquireResourceCreateAttempt(ctx context.Context, key string, expected *State, resourceIndex int) (State, error) {
	var current State
	found, etag, err := c.m1.GetJSONVersion(ctx, key, &current)
	if err != nil {
		return State{}, fmt.Errorf("read compute create attempt state: %w", err)
	}
	if !found || current.CreationIntent == nil || expected.CreationIntent == nil || current.CreationIntent.OperationID != expected.CreationIntent.OperationID || resourceIndex >= len(current.CreationIntent.Resources) {
		return State{}, fmt.Errorf("compute creation intent changed before attempt acquisition")
	}
	if current.Phase != "creating" || current.CreationIntent.Phase != "creating" {
		return State{}, &lifecycleFenceError{message: fmt.Sprintf("compute operation %s cannot acquire a create attempt in phase %s", current.CreationIntent.OperationID, current.Phase)}
	}
	resource := &current.CreationIntent.Resources[resourceIndex]
	if resource.Phase != "pending" {
		return State{}, fmt.Errorf("compute create attempt for %s was concurrently claimed in phase %s", resource.Name, resource.Phase)
	}
	attemptedAt := time.Now().UTC()
	reconcileUntil := attemptedAt.Add(c.creationReconcileGrace())
	resource.Phase = "creating"
	resource.AttemptID = newID()
	resource.CreateAttemptedAt = &attemptedAt
	resource.ReconcileUntil = &reconcileUntil
	_, written, err := c.m1.PutJSONIfMatch(ctx, key, etag, current)
	if err != nil {
		return State{}, fmt.Errorf("persist compute create attempt: %w", err)
	}
	if !written {
		return State{}, fmt.Errorf("compute create attempt for %s was concurrently claimed", resource.Name)
	}
	return current, nil
}

func (c *Client) reconcileManagedServer(ctx context.Context, spec Spec, state *State, intent *ComputeResourceIntent) (compute.Server, bool, error) {
	if intent.ResourceID != "" {
		server, err := c.compute.Server(ctx, intent.ResourceID)
		if err == nil {
			if server.Name != intent.Name || server.Metadata["canter.managed"] != "true" || server.Metadata["canter.sandbox"] != spec.Metadata.Name || server.Metadata["canter.operation"] != state.CreationIntent.OperationID || server.Metadata["canter.resource"] != intent.Name {
				return compute.Server{}, false, fmt.Errorf("compute resource %s no longer matches its durable creation intent", intent.ResourceID)
			}
			matches, lookupErr := c.compute.FindManagedServers(ctx, spec.Metadata.Name, state.CreationIntent.OperationID, intent.Name)
			if lookupErr != nil {
				return compute.Server{}, false, lookupErr
			}
			if len(matches) > 1 || (len(matches) == 1 && matches[0].ID != server.ID) {
				count := len(matches)
				if count < 2 {
					count = 2
				}
				return compute.Server{}, false, &compute.DuplicateManagedResourceError{Kind: "compute resource", Identity: intent.Name, Count: count}
			}
			return server, true, nil
		}
		if !compute.IsNotFound(err) {
			return compute.Server{}, false, fmt.Errorf("inspect intended compute %s: %w", intent.ResourceID, err)
		}
	}
	matches, err := c.compute.FindManagedServers(ctx, spec.Metadata.Name, state.CreationIntent.OperationID, intent.Name)
	if err != nil {
		return compute.Server{}, false, err
	}
	switch len(matches) {
	case 0:
		return compute.Server{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		return compute.Server{}, false, &compute.DuplicateManagedResourceError{Kind: "compute resource", Identity: intent.Name, Count: len(matches)}
	}
}

func (c *Client) waitForManagedServerVisibility(ctx context.Context, spec Spec, state *State, intent *ComputeResourceIntent, until time.Time) (compute.Server, bool, error) {
	ticker := time.NewTicker(c.lookupInterval())
	defer ticker.Stop()
	for {
		server, found, err := c.reconcileManagedServer(ctx, spec, state, intent)
		if err != nil || found {
			return server, found, err
		}
		remaining := time.Until(until)
		if remaining <= 0 {
			return compute.Server{}, false, nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return compute.Server{}, false, ctx.Err()
		case <-timer.C:
			return compute.Server{}, false, nil
		case <-ticker.C:
			if !timer.Stop() {
				<-timer.C
			}
		}
	}
}

type lifecycleFenceError struct{ message string }

func (e *lifecycleFenceError) Error() string { return e.message }

func isLifecycleFenceError(err error) bool {
	var target *lifecycleFenceError
	return errors.As(err, &target)
}

func (c *Client) mutateCreationState(ctx context.Context, key, operationID string, resourceIndex int, attemptID string, mutate func(*State, *ComputeResourceIntent) (bool, error)) (State, error) {
	for retries := 0; retries < 8; retries++ {
		var current State
		found, etag, err := c.m1.GetJSONVersion(ctx, key, &current)
		if err != nil {
			return State{}, err
		}
		if !found || current.CreationIntent == nil || current.CreationIntent.OperationID != operationID || current.Phase == "destroying" || current.Phase == "destroyed" || current.CreationIntent.Phase == "destroying" || current.CreationIntent.Phase == "destroyed" {
			return State{}, &lifecycleFenceError{message: fmt.Sprintf("compute operation %s lost its lifecycle fence", operationID)}
		}
		var resource *ComputeResourceIntent
		if resourceIndex >= 0 {
			if resourceIndex >= len(current.CreationIntent.Resources) {
				return State{}, &lifecycleFenceError{message: fmt.Sprintf("compute operation %s resource intent changed", operationID)}
			}
			resource = &current.CreationIntent.Resources[resourceIndex]
			if attemptID != "" && resource.AttemptID != attemptID {
				return State{}, &lifecycleFenceError{message: fmt.Sprintf("compute operation %s attempt %s lost its lifecycle fence", operationID, attemptID)}
			}
		}
		changed, err := mutate(&current, resource)
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
	return State{}, fmt.Errorf("compute operation %s state changed continuously during transition", operationID)
}

func (c *Client) persistResourceAttachment(ctx context.Context, key, operationID string, resourceIndex int, attemptID string, server compute.Server) (State, error) {
	state, err := c.mutateCreationState(ctx, key, operationID, resourceIndex, attemptID, func(current *State, resource *ComputeResourceIntent) (bool, error) {
		if current.Phase != "creating" || current.CreationIntent.Phase != "creating" {
			return false, &lifecycleFenceError{message: fmt.Sprintf("compute operation %s cannot attach in phase %s", operationID, current.Phase)}
		}
		if resource.Phase == "attached" || resource.Phase == "active" || resource.Phase == "ready" {
			if resource.ResourceID != server.ID {
				return false, &lifecycleFenceError{message: fmt.Sprintf("compute attempt %s is already attached to another resource", attemptID)}
			}
			return false, nil
		}
		if resource.Phase != "creating" || attemptID == "" {
			return false, &lifecycleFenceError{message: fmt.Sprintf("compute attempt %s cannot attach in phase %s", attemptID, resource.Phase)}
		}
		resource.ResourceID = server.ID
		resource.Phase = "attached"
		updateResource(current, server)
		return true, nil
	})
	if err != nil {
		return State{}, fmt.Errorf("persist attached compute %s: %w", server.ID, err)
	}
	return state, nil
}

func (c *Client) persistResourceActive(ctx context.Context, key, operationID string, resourceIndex int, attemptID string, server compute.Server) (State, error) {
	state, err := c.mutateCreationState(ctx, key, operationID, resourceIndex, attemptID, func(current *State, resource *ComputeResourceIntent) (bool, error) {
		if current.Phase != "creating" || current.CreationIntent.Phase != "creating" || resource.ResourceID != server.ID {
			return false, &lifecycleFenceError{message: fmt.Sprintf("compute operation %s cannot record active resource %s", operationID, server.ID)}
		}
		if resource.Phase == "active" || resource.Phase == "ready" {
			return false, nil
		}
		if resource.Phase != "attached" {
			return false, &lifecycleFenceError{message: fmt.Sprintf("compute attempt %s cannot become active from %s", attemptID, resource.Phase)}
		}
		resource.Phase = "active"
		updateResource(current, server)
		return true, nil
	})
	if err != nil {
		return State{}, fmt.Errorf("persist active compute %s: %w", server.ID, err)
	}
	return state, nil
}

func (c *Client) persistExhaustedNetwork(ctx context.Context, key, operationID string, resourceIndex int, attemptID, serverID string) (State, error) {
	state, err := c.mutateCreationState(ctx, key, operationID, resourceIndex, attemptID, func(current *State, resource *ComputeResourceIntent) (bool, error) {
		if current.Phase != "creating" || current.CreationIntent.Phase != "creating" || resource.ResourceID != serverID || (resource.Phase != "attached" && resource.Phase != "active") {
			return false, &lifecycleFenceError{message: fmt.Sprintf("compute attempt %s cannot record exhausted network", attemptID)}
		}
		current.Attempts = append(current.Attempts, Attempt{ID: serverID, Status: "DELETED", Failure: "network capacity exhausted"})
		removeResource(current, serverID)
		resource.ResourceID = ""
		resource.Phase = "pending"
		resource.AttemptID = ""
		resource.CreateAttemptedAt = nil
		resource.ReconcileUntil = nil
		resource.NetworkIndex++
		return true, nil
	})
	if err != nil {
		return State{}, fmt.Errorf("persist exhausted network attempt: %w", err)
	}
	return state, nil
}

func (c *Client) persistResourceReady(ctx context.Context, key, operationID string, resourceIndex int, attemptID string) (State, error) {
	state, err := c.mutateCreationState(ctx, key, operationID, resourceIndex, attemptID, func(current *State, resource *ComputeResourceIntent) (bool, error) {
		if current.Phase != "creating" || current.CreationIntent.Phase != "creating" {
			return false, &lifecycleFenceError{message: fmt.Sprintf("compute operation %s cannot record proof-ready resource", operationID)}
		}
		if resource.Phase == "ready" {
			return false, nil
		}
		if resource.Phase != "active" {
			return false, &lifecycleFenceError{message: fmt.Sprintf("compute attempt %s cannot become ready from %s", attemptID, resource.Phase)}
		}
		resource.Phase = "ready"
		return true, nil
	})
	if err != nil {
		return State{}, fmt.Errorf("persist proof-ready compute: %w", err)
	}
	return state, nil
}

func (c *Client) persistCreationReady(ctx context.Context, key, operationID string) (State, error) {
	state, err := c.mutateCreationState(ctx, key, operationID, -1, "", func(current *State, _ *ComputeResourceIntent) (bool, error) {
		if current.Phase == "ready" && current.CreationIntent.Phase == "ready" {
			return false, nil
		}
		if current.Phase != "creating" || current.CreationIntent.Phase != "creating" {
			return false, &lifecycleFenceError{message: fmt.Sprintf("compute operation %s cannot become ready from %s", operationID, current.Phase)}
		}
		for _, resource := range current.CreationIntent.Resources {
			if resource.Phase != "ready" {
				return false, &lifecycleFenceError{message: fmt.Sprintf("compute operation %s has a resource in phase %s", operationID, resource.Phase)}
			}
		}
		current.CreationIntent.Phase = "ready"
		current.Phase = "ready"
		current.Failure = ""
		return true, nil
	})
	if err != nil {
		return State{}, fmt.Errorf("persist ready sandbox: %w", err)
	}
	return state, nil
}

func (c *Client) recordCreationFailure(ctx context.Context, key string, state *State, operationID string, cause error, terminal bool) error {
	persisted, err := c.mutateCreationState(ctx, key, operationID, -1, "", func(current *State, _ *ComputeResourceIntent) (bool, error) {
		if current.Phase != "creating" || current.CreationIntent.Phase != "creating" {
			return false, &lifecycleFenceError{message: fmt.Sprintf("compute operation %s cannot record failure in phase %s", operationID, current.Phase)}
		}
		current.Failure = cause.Error()
		if terminal {
			current.Phase = "failed"
			current.CreationIntent.Phase = "failed"
		}
		return true, nil
	})
	if err == nil {
		*state = persisted
	}
	return err
}

func (c *Client) escalateCreation(ctx context.Context, key string, state *State, operationID string, cause error) error {
	persisted, err := c.mutateCreationState(ctx, key, operationID, -1, "", func(current *State, _ *ComputeResourceIntent) (bool, error) {
		if current.Phase == "escalated" && current.CreationIntent.Phase == "escalated" {
			return false, nil
		}
		if current.Phase != "creating" || current.CreationIntent.Phase != "creating" {
			return false, &lifecycleFenceError{message: fmt.Sprintf("compute operation %s cannot escalate in phase %s", operationID, current.Phase)}
		}
		current.Phase = "escalated"
		current.CreationIntent.Phase = "escalated"
		current.Failure = cause.Error()
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("%v; persist escalated creation: %w", cause, err)
	}
	*state = persisted
	return cause
}

func (c *Client) cleanupComputeAfterFence(ctx context.Context, key, operationID string, resourceIndex int, attemptID string, server compute.Server) error {
	var claimed bool
	var priorPhase string
	for retries := 0; retries < 8; retries++ {
		var current State
		found, etag, err := c.m1.GetJSONVersion(ctx, key, &current)
		if err != nil {
			return err
		}
		if !found || current.CreationIntent == nil || current.CreationIntent.OperationID != operationID || resourceIndex >= len(current.CreationIntent.Resources) {
			return nil
		}
		resource := &current.CreationIntent.Resources[resourceIndex]
		if resource.AttemptID != attemptID || resource.Phase == "destroyed" {
			return nil
		}
		// A live Destroy owns provider reconciliation while the state is
		// destroying. Only claim cleanup after that owner has escalated.
		if current.Phase != "escalated" {
			return nil
		}
		priorPhase = resource.Phase
		current.Phase = "cleanup"
		resource.Phase = "cleaning"
		resource.ResourceID = server.ID
		_, written, err := c.m1.PutJSONIfMatch(ctx, key, etag, current)
		if err != nil {
			return err
		}
		if written {
			claimed = true
			break
		}
	}
	if !claimed {
		return fmt.Errorf("compute cleanup could not acquire its lifecycle fence")
	}
	deleteErr := c.compute.Delete(ctx, server.ID)
	for retries := 0; retries < 8; retries++ {
		var current State
		found, etag, err := c.m1.GetJSONVersion(ctx, key, &current)
		if err != nil {
			return err
		}
		if !found || current.Phase != "cleanup" || current.CreationIntent == nil || current.CreationIntent.OperationID != operationID || resourceIndex >= len(current.CreationIntent.Resources) {
			return &lifecycleFenceError{message: "compute cleanup lost its lifecycle fence"}
		}
		resource := &current.CreationIntent.Resources[resourceIndex]
		if resource.AttemptID != attemptID || resource.Phase != "cleaning" || resource.ResourceID != server.ID {
			return &lifecycleFenceError{message: "compute cleanup resource fence changed"}
		}
		current.Phase = "escalated"
		resource.Phase = "destroyed"
		removeResource(&current, server.ID)
		if deleteErr != nil {
			resource.Phase = priorPhase
			current.Failure = fmt.Sprintf("cleanup stale compute %s: %v", server.ID, deleteErr)
		}
		_, written, err := c.m1.PutJSONIfMatch(ctx, key, etag, current)
		if err != nil {
			return err
		}
		if written {
			return deleteErr
		}
	}
	return fmt.Errorf("compute cleanup state changed continuously")
}

func updateResource(state *State, server compute.Server) {
	for index := range state.Resources {
		if state.Resources[index].ID == server.ID || state.Resources[index].Name == server.Name {
			state.Resources[index] = Resource{ID: server.ID, Name: server.Name, Status: server.Status, Address: server.IPv4()}
			return
		}
	}
	state.Resources = append(state.Resources, Resource{ID: server.ID, Name: server.Name, Status: server.Status, Address: server.IPv4()})
}

func removeResource(state *State, id string) {
	for index := range state.Resources {
		if state.Resources[index].ID == id {
			state.Resources = append(state.Resources[:index], state.Resources[index+1:]...)
			return
		}
	}
}

func (c *Client) Status(ctx context.Context, spec Spec) (State, error) {
	var state State
	if err := c.m1.Get(ctx, stateKey(spec), &state); err != nil {
		return State{}, err
	}
	if state.Phase == "destroyed" {
		return state, nil
	}
	if state.Phase == "cleanup" {
		return state, fmt.Errorf("sandbox cleanup is already in progress")
	}
	for i := range state.Resources {
		if state.Resources[i].Status == "DELETED" {
			continue
		}
		server, err := c.compute.Server(ctx, state.Resources[i].ID)
		if err != nil {
			return State{}, err
		}
		state.Resources[i].Status = server.Status
		state.Resources[i].Address = server.IPv4()
	}
	return state, nil
}

func (c *Client) Destroy(ctx context.Context, spec Spec) (State, error) {
	var state State
	key := stateKey(spec)
	found, etag, err := c.m1.GetJSONVersion(ctx, key, &state)
	if err != nil {
		return State{}, err
	}
	if !found {
		return State{}, fmt.Errorf("sandbox state is missing")
	}
	if state.Phase == "destroyed" {
		return state, nil
	}
	creationOperationID := ""
	if state.CreationIntent != nil {
		creationOperationID = state.CreationIntent.OperationID
	}
	exposureOperationID := ""
	exposureUnresolved := false
	if state.ExposureIntent != nil {
		exposureOperationID = state.ExposureIntent.OperationID
		exposureUnresolved = state.ExposureIntent.MutationUnresolved || state.ExposureIntent.Phase == "creating" || state.ExposureIntent.Phase == "destroying" || state.ExposureIntent.Phase == "cleaning"
	}
	if state.Phase != "destroying" {
		state.Phase = "destroying"
		state.Failure = ""
		if state.CreationIntent != nil {
			state.CreationIntent.Phase = "destroying"
		}
		if state.ExposureIntent != nil {
			state.ExposureIntent.Phase = "destroying"
			state.ExposureIntent.MutationUnresolved = exposureUnresolved
		}
		if _, written, writeErr := c.m1.PutJSONIfMatch(ctx, key, etag, state); writeErr != nil {
			return State{}, fmt.Errorf("record destroying state: %w", writeErr)
		} else if !written {
			return State{}, fmt.Errorf("sandbox lifecycle changed before destroy could acquire its fence")
		}
	}
	escalate := func(cause error) (State, error) {
		persisted, persistErr := c.mutateDestroyState(context.WithoutCancel(ctx), key, creationOperationID, exposureOperationID, func(current *State) (bool, error) {
			current.Phase = "escalated"
			current.Failure = cause.Error()
			if current.CreationIntent != nil {
				current.CreationIntent.Phase = "escalated"
			}
			if current.ExposureIntent != nil {
				current.ExposureIntent.Phase = "escalated"
			}
			return true, nil
		})
		if persistErr != nil {
			return state, fmt.Errorf("%v; persist interrupted destroy: %w", cause, persistErr)
		}
		state = persisted
		return state, cause
	}
	if state.CreationIntent == nil && len(state.Resources) > 0 {
		return escalate(fmt.Errorf("cannot safely destroy compute resources without a durable creation operation"))
	}
	policies := make(map[string]NetworkPolicy)
	if state.ExposureIntent == nil && len(state.NetworkPolicies) > 0 {
		return escalate(fmt.Errorf("cannot safely destroy network policies without a durable exposure operation"))
	}
	if state.ExposureIntent != nil {
		intent := state.ExposureIntent
		for _, policy := range state.NetworkPolicies {
			if policy.Name == intent.Name && policy.Ownership == intent.Ownership && policy.Protocol == intent.Protocol && policy.Port == intent.Port {
				policies[policy.ID] = policy
			}
		}
		request := compute.ManagedTCPExposureRequest{ServerID: intent.ServerID, Name: intent.Name, Ownership: intent.Ownership, Port: intent.Port}
		policy, recovered, lookupErr := c.compute.FindManagedTCPExposure(ctx, request)
		if lookupErr != nil {
			return escalate(fmt.Errorf("reconcile public endpoint before destroy: %w", lookupErr))
		}
		if recovered {
			policies[policy.ID] = NetworkPolicy{ID: policy.ID, PortID: policy.PortID, RuleID: policy.RuleID, Name: intent.Name, Ownership: intent.Ownership, Protocol: intent.Protocol, Port: policy.Port}
		} else if intent.MutationUnresolved && len(policies) == 0 {
			return escalate(fmt.Errorf("public endpoint operation %s is still in flight with no visible exact managed policy", exposureOperationID))
		}
	}
	if state.CreationIntent != nil {
		for index := range state.CreationIntent.Resources {
			resourceIntent := state.CreationIntent.Resources[index]
			servers, lookupErr := c.ownedServersForDestroy(ctx, spec, creationOperationID, resourceIntent)
			if lookupErr != nil {
				return escalate(lookupErr)
			}
			if resourceIntent.Phase != "destroyed" && len(servers) == 0 && (resourceIntent.Phase == "creating" || resourceIntent.ResourceID != "") {
				return escalate(fmt.Errorf("compute resource %s is unresolved with no visible exact managed resource", resourceIntent.Name))
			}
			for _, server := range servers {
				if deleteErr := c.compute.Delete(ctx, server.ID); deleteErr != nil {
					return escalate(fmt.Errorf("delete compute %s: %w", server.ID, deleteErr))
				}
			}
			persisted, persistErr := c.mutateDestroyState(ctx, key, creationOperationID, exposureOperationID, func(current *State) (bool, error) {
				if index >= len(current.CreationIntent.Resources) {
					return false, &lifecycleFenceError{message: "creation resources changed during destroy"}
				}
				current.CreationIntent.Resources[index].Phase = "destroyed"
				for resourceIndex := range current.Resources {
					if current.Resources[resourceIndex].Name == resourceIntent.Name {
						current.Resources[resourceIndex].Status = "DELETED"
					}
				}
				return true, nil
			})
			if persistErr != nil {
				return state, fmt.Errorf("persist deleted compute %s: %w", resourceIntent.Name, persistErr)
			}
			state = persisted
		}
	}
	for _, policy := range policies {
		if deleteErr := c.compute.DeleteSecurityPolicy(ctx, policy.ID); deleteErr != nil {
			return escalate(fmt.Errorf("delete network policy %s: %w", policy.ID, deleteErr))
		}
		persisted, persistErr := c.mutateDestroyState(ctx, key, creationOperationID, exposureOperationID, func(current *State) (bool, error) {
			filtered := current.NetworkPolicies[:0]
			for _, existing := range current.NetworkPolicies {
				if existing.ID != policy.ID {
					filtered = append(filtered, existing)
				}
			}
			current.NetworkPolicies = filtered
			return true, nil
		})
		if persistErr != nil {
			return state, fmt.Errorf("persist deleted network policy %s: %w", policy.ID, persistErr)
		}
		state = persisted
	}
	if state.ExposureIntent != nil && state.ExposureIntent.Phase != "destroyed" {
		persisted, persistErr := c.mutateDestroyState(ctx, key, creationOperationID, exposureOperationID, func(current *State) (bool, error) {
			current.ExposureIntent.Phase = "destroyed"
			current.ExposureIntent.MutationUnresolved = false
			return true, nil
		})
		if persistErr != nil {
			return state, fmt.Errorf("persist deleted public endpoint: %w", persistErr)
		}
		state = persisted
	}
	persisted, persistErr := c.mutateDestroyState(ctx, key, creationOperationID, exposureOperationID, func(current *State) (bool, error) {
		now := time.Now().UTC()
		current.Phase = "destroyed"
		current.DestroyedAt = &now
		current.Failure = ""
		if current.CreationIntent != nil {
			current.CreationIntent.Phase = "destroyed"
		}
		if current.ExposureIntent != nil {
			current.ExposureIntent.Phase = "destroyed"
			current.ExposureIntent.MutationUnresolved = false
		}
		return true, nil
	})
	if persistErr != nil {
		return state, persistErr
	}
	state = persisted
	receiptKey := fmt.Sprintf("%s/receipts/destroy-%s.json", strings.TrimRight(spec.Spec.M1.Prefix, "/"), newID())
	if err := c.m1.PutJSON(ctx, receiptKey, state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (c *Client) mutateDestroyState(ctx context.Context, key, creationOperationID, exposureOperationID string, mutate func(*State) (bool, error)) (State, error) {
	for retries := 0; retries < 8; retries++ {
		var current State
		found, etag, err := c.m1.GetJSONVersion(ctx, key, &current)
		if err != nil {
			return State{}, err
		}
		if !found || current.Phase != "destroying" || (creationOperationID != "" && (current.CreationIntent == nil || current.CreationIntent.OperationID != creationOperationID)) || (exposureOperationID != "" && (current.ExposureIntent == nil || current.ExposureIntent.OperationID != exposureOperationID)) {
			return State{}, &lifecycleFenceError{message: "destroy operation lost its lifecycle fence"}
		}
		changed, err := mutate(&current)
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
	return State{}, fmt.Errorf("destroy state changed continuously during transition")
}

func (c *Client) ownedServersForDestroy(ctx context.Context, spec Spec, operationID string, intent ComputeResourceIntent) ([]compute.Server, error) {
	matches, err := c.compute.FindManagedServers(ctx, spec.Metadata.Name, operationID, intent.Name)
	if err != nil {
		return nil, fmt.Errorf("reconcile compute %s before destroy: %w", intent.Name, err)
	}
	owned := make(map[string]compute.Server, len(matches)+1)
	for _, server := range matches {
		owned[server.ID] = server
	}
	if intent.ResourceID != "" {
		server, inspectErr := c.compute.Server(ctx, intent.ResourceID)
		if inspectErr == nil {
			if server.Name != intent.Name || server.Metadata["canter.managed"] != "true" || server.Metadata["canter.sandbox"] != spec.Metadata.Name || server.Metadata["canter.operation"] != operationID || server.Metadata["canter.resource"] != intent.Name {
				return nil, fmt.Errorf("compute resource %s does not belong to fenced operation %s", intent.ResourceID, operationID)
			}
			owned[server.ID] = server
		} else if !compute.IsNotFound(inspectErr) {
			return nil, fmt.Errorf("inspect compute %s before destroy: %w", intent.ResourceID, inspectErr)
		}
	}
	servers := make([]compute.Server, 0, len(owned))
	for _, server := range owned {
		servers = append(servers, server)
	}
	return servers, nil
}

func stateKey(spec Spec) string { return strings.TrimRight(spec.Spec.M1.Prefix, "/") + "/state.json" }

func bootScript(spec Spec, proofURL string) string {
	return "#!/bin/sh\nset +e\ncanter_log=/tmp/canter-bootstrap.log\ncanter_rc_file=/tmp/canter-bootstrap.rc\n(\n(\nset -eu\n" + spec.Spec.Compute.Bootstrap + "\n)\nprintf '%s' \"$?\" > \"$canter_rc_file\"\n) 2>&1 | tee \"$canter_log\"\n" +
		"canter_rc=$(cat \"$canter_rc_file\")\nif [ \"$canter_rc\" -eq 0 ]; then canter_status=booted; else canter_status=failed; fi\ncanter_message_b64=$(tail -n 1 \"$canter_log\" | base64 | tr -d '\\n')\n" +
		fmt.Sprintf("printf '{\"sandbox\":\"%s\",\"status\":\"%%s\",\"exitCode\":%%s,\"hostname\":\"%%s\",\"messageBase64\":\"%%s\"}' \"$canter_status\" \"$canter_rc\" \"$(hostname)\" \"$canter_message_b64\" | curl -fsS -X PUT -H 'Content-Type: application/json' --data-binary @- '%s'\nexit \"$canter_rc\"\n", spec.Metadata.Name, proofURL)
}

type proofStore interface {
	GetOptional(context.Context, string, any) (bool, error)
}

func waitForProof(ctx context.Context, store proofStore, key string) error {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		var proof BootProof
		found, err := store.GetOptional(ctx, key, &proof)
		if err != nil {
			return fmt.Errorf("read compute boot proof: %w", err)
		}
		if found {
			if proof.Status != "booted" || proof.ExitCode != 0 {
				message := "bootstrap reported failure"
				if decoded, decodeErr := base64.StdEncoding.DecodeString(proof.MessageBase64); decodeErr == nil && strings.TrimSpace(string(decoded)) != "" {
					message = strings.TrimSpace(string(decoded))
				}
				return fmt.Errorf("compute bootstrap failed on %s with exit code %d: %s", proof.Hostname, proof.ExitCode, message)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("compute became active but did not write boot proof before deadline: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func rotate(values []string, seed string) []string {
	if len(values) < 2 {
		return values
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	start := int(h.Sum32() % uint32(len(values)))
	out := append([]string(nil), values[start:]...)
	return append(out, values[:start]...)
}
