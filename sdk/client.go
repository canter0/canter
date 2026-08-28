package sdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	model   model.Client
	compute *compute.Client
	m1      *m1.Client
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

type State struct {
	Sandbox     string     `json:"sandbox"`
	Phase       string     `json:"phase"`
	Class       string     `json:"class"`
	Image       string     `json:"image"`
	Resources   []Resource `json:"resources"`
	Attempts    []Attempt  `json:"attempts,omitempty"`
	ProofKeys   []string   `json:"proofKeys"`
	CreatedAt   time.Time  `json:"createdAt"`
	DestroyedAt *time.Time `json:"destroyedAt,omitempty"`
}

type Attempt struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Failure string `json:"failure"`
}

type Resource struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
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
	found, err := c.m1.GetOptional(ctx, stateKey, &prior)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("read existing sandbox state: %w", err)
	}
	if found && prior.Phase != "destroyed" {
		return ApplyResult{}, fmt.Errorf("sandbox %q already has live state; inspect it with canter status", spec.Metadata.Name)
	}
	plan, err := c.Plan(ctx, spec)
	if err != nil {
		return ApplyResult{}, err
	}
	shape, imageID, networkIDs, err := c.compute.Resolve(ctx, spec.Spec.Compute.Class, spec.Spec.Compute.Image)
	if err != nil {
		return ApplyResult{}, err
	}
	opID := newID()
	networkIDs = rotate(networkIDs, opID)
	state := State{Sandbox: spec.Metadata.Name, Phase: "creating", Class: spec.Spec.Compute.Class, Image: spec.Spec.Compute.Image, CreatedAt: time.Now().UTC()}
	for i := 0; i < spec.Spec.Compute.Replicas; i++ {
		proofKey := fmt.Sprintf("%s/proofs/%s-%d.json", strings.TrimRight(spec.Spec.M1.Prefix, "/"), opID, i+1)
		proofURL, err := c.m1.PresignPut(ctx, proofKey, 15*time.Minute)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("create m1 proof URL: %w", err)
		}
		name := fmt.Sprintf("canter-%s-%d", spec.Metadata.Name, i+1)
		userData := bootScript(spec, proofURL)
		ready := false
		for _, networkID := range networkIDs {
			server, err := c.compute.Create(ctx, name, spec.Metadata.Name, shape.ID, imageID, networkID, userData)
			if err != nil {
				return ApplyResult{}, err
			}
			state.Resources = append(state.Resources, Resource{ID: server.ID, Name: name, Status: server.Status})
			state.ProofKeys = append(state.ProofKeys, proofKey)
			if err := c.m1.PutJSON(ctx, stateKey, state); err != nil {
				return ApplyResult{}, fmt.Errorf("compute created but state persistence failed; resource id %s: %w", server.ID, err)
			}
			active, waitErr := c.compute.WaitActive(ctx, server.ID)
			if waitErr == nil {
				state.Resources[len(state.Resources)-1].Status = active.Status
				ready = true
				break
			}
			if !compute.IsNetworkExhausted(waitErr) {
				return ApplyResult{}, fmt.Errorf("wait for compute %s: %w", server.ID, waitErr)
			}
			if err := c.compute.Delete(ctx, server.ID); err != nil {
				return ApplyResult{}, fmt.Errorf("network allocation failed and cleanup of compute %s also failed: %w", server.ID, err)
			}
			state.Attempts = append(state.Attempts, Attempt{ID: server.ID, Status: "DELETED", Failure: "network capacity exhausted"})
			state.Resources = state.Resources[:len(state.Resources)-1]
			state.ProofKeys = state.ProofKeys[:len(state.ProofKeys)-1]
			if err := c.m1.PutJSON(ctx, stateKey, state); err != nil {
				return ApplyResult{}, err
			}
		}
		if !ready {
			return ApplyResult{}, fmt.Errorf("all eligible compute networks exhausted their address capacity")
		}
		if err := waitForProof(ctx, c.m1, proofKey); err != nil {
			return ApplyResult{}, err
		}
	}
	state.Phase = "ready"
	if err := c.m1.PutJSON(ctx, stateKey, state); err != nil {
		return ApplyResult{}, err
	}
	receiptKey := fmt.Sprintf("%s/receipts/%s.json", strings.TrimRight(spec.Spec.M1.Prefix, "/"), opID)
	result := ApplyResult{OperationID: opID, Plan: plan, State: state, ReceiptKey: receiptKey, ElapsedMS: time.Since(started).Milliseconds()}
	if err := c.m1.PutJSON(ctx, receiptKey, result); err != nil {
		return ApplyResult{}, err
	}
	return result, nil
}

func (c *Client) Status(ctx context.Context, spec Spec) (State, error) {
	var state State
	if err := c.m1.Get(ctx, stateKey(spec), &state); err != nil {
		return State{}, err
	}
	if state.Phase == "destroyed" {
		return state, nil
	}
	for i := range state.Resources {
		server, err := c.compute.Server(ctx, state.Resources[i].ID)
		if err != nil {
			return State{}, err
		}
		state.Resources[i].Status = server.Status
	}
	return state, nil
}

func (c *Client) Destroy(ctx context.Context, spec Spec) (State, error) {
	var state State
	key := stateKey(spec)
	if err := c.m1.Get(ctx, key, &state); err != nil {
		return State{}, err
	}
	if state.Phase == "destroyed" {
		return state, nil
	}
	for _, resource := range state.Resources {
		if err := c.compute.Delete(ctx, resource.ID); err != nil {
			return State{}, fmt.Errorf("delete compute %s: %w", resource.ID, err)
		}
	}
	now := time.Now().UTC()
	state.Phase = "destroyed"
	state.DestroyedAt = &now
	for i := range state.Resources {
		state.Resources[i].Status = "DELETED"
	}
	if err := c.m1.PutJSON(ctx, key, state); err != nil {
		return State{}, err
	}
	receiptKey := fmt.Sprintf("%s/receipts/destroy-%s.json", strings.TrimRight(spec.Spec.M1.Prefix, "/"), newID())
	if err := c.m1.PutJSON(ctx, receiptKey, state); err != nil {
		return State{}, err
	}
	return state, nil
}

func stateKey(spec Spec) string { return strings.TrimRight(spec.Spec.M1.Prefix, "/") + "/state.json" }

func bootScript(spec Spec, proofURL string) string {
	return "#!/bin/sh\nset -eu\n" + spec.Spec.Compute.Bootstrap + "\n" +
		fmt.Sprintf("printf '{\"sandbox\":\"%s\",\"status\":\"booted\",\"hostname\":\"%%s\"}' \"$(hostname)\" | curl -fsS -X PUT -H 'Content-Type: application/json' --data-binary @- '%s'\n", spec.Metadata.Name, proofURL)
}

type objectChecker interface {
	Exists(context.Context, string) bool
}

func waitForProof(ctx context.Context, store objectChecker, key string) error {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		if store.Exists(ctx, key) {
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
