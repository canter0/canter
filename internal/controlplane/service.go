package controlplane

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/canter0/canter/sdk"
)

// Engine is the existing real execution boundary. HTTP and MCP adapters call
// this interface; they never shell out to the CLI or implement provider logic.
type Engine interface {
	InspectSystem(context.Context, sdk.System) (sdk.SystemView, error)
	DraftChangeRequest(context.Context, sdk.System, sdk.ChangeRequest) (sdk.Change, error)
	InspectChange(context.Context, sdk.System, string) (sdk.Change, error)
	AuthorizeChange(context.Context, sdk.System, string, string) (sdk.Change, error)
	ApplyChange(context.Context, sdk.System, string) (sdk.Change, error)
}

type Service struct {
	Store          *Store
	Engine         Engine
	NodeGateway    NodeGatewayEngine
	NodeGatewayURL string
}

func (s *Service) Bootstrap(ctx context.Context, p Principal) (Bootstrap, error) {
	if p.Installation == nil || p.Session == nil {
		return Bootstrap{}, ErrUnauthorized
	}
	workspace, err := s.Store.Workspace(ctx, p.WorkspaceID)
	if err != nil {
		return Bootstrap{}, err
	}
	systems, err := s.Store.ListSystems(ctx, p.WorkspaceID)
	if err != nil {
		return Bootstrap{}, err
	}
	changes, err := s.Store.ListChanges(ctx, p.WorkspaceID)
	if err != nil {
		return Bootstrap{}, err
	}
	pending := make([]ChangeIndex, 0, len(changes))
	for _, c := range changes {
		if c.Phase != "committed" && c.Phase != "rejected" && c.Phase != "reverted" {
			pending = append(pending, c)
		}
	}
	deployments, err := s.Store.ListInitialDeployments(ctx, p.WorkspaceID)
	if err != nil {
		return Bootstrap{}, err
	}
	return Bootstrap{ProtocolVersion: "v1", Installation: *p.Installation, Session: *p.Session, Workspace: workspace, Systems: systems, Changes: changes, PendingChanges: pending, InitialDeployments: deployments, Capabilities: initialDeploymentCapabilities(p.WorkspaceID), Incidents: []any{}}, nil
}

func initialDeploymentCapabilities(workspaceID string) map[string]any {
	prefix := "/v1/workspaces/" + workspaceID
	return map[string]any{
		"compute": map[string]any{
			"semantics":        "provider-neutral ordered capacity classes; provider identities, flavor IDs, and credentials remain private",
			"hostClasses":      sdk.SupportedHostClasses(),
			"defaultHostClass": "c1",
		},
		"standingPolicies": map[string]any{
			"semantics": "only a signed-in human can create or revoke an immutable, expiring envelope; an allowed agent may ask Canter to evaluate one exact Change digest, and Canter either records an automatic policy authorization and queues it or performs no mutation and requires human approval",
			"bounds":    []string{"exact agent installation IDs", "System and workspace revisions", "affected services", "operation kinds", "availability", "data impact", "operation reversibility", "per-service inclusive replica ranges", "maximum additional monthly cost", "maximum operation count", "expiry"},
			"http": map[string]map[string]string{
				"list":      {"method": "GET", "path": prefix + "/systems/{system}/policies"},
				"create":    {"method": "POST", "path": prefix + "/systems/{system}/policies", "principal": "human"},
				"revoke":    {"method": "POST", "path": prefix + "/systems/{system}/policies/{policyId}/revoke", "principal": "human"},
				"evaluate":  {"method": "POST", "path": prefix + "/systems/{system}/changes/{changeId}/apply-under-policy", "principal": "agent"},
				"execution": {"method": "GET", "path": prefix + "/systems/{system}/changes/{changeId}/execution"},
			},
			"mcp": map[string][]string{"tools": {"canter_list_standing_policies", "canter_apply_change_under_policy", "canter_inspect_change_execution"}},
		},
		"replicaScaling": map[string]any{
			"semantics": "a typed Change may adjust one process-isolated public HTTP application service within already allocated host memory; Canter derives the current count, validates host fit, keeps healthy replicas serving, load-balances only ready replicas, and restores the prior count after failure or a node-enforced optional lease expiry even when the agent is offline",
			"request":   map[string]any{"kind": "Change", "spec.scale": map[string]any{"service": "exact System service name", "replicas": "positive integer bounded by current host capacity", "forSeconds": "optional 60 to 86400 second lease"}},
			"limits":    []string{"one existing host only", "no implicit VM provisioning", "process-isolated public HTTP applications only", "lease expiry is visible in System observed state; a separate child execution record is not yet emitted"},
			"mcp":       map[string]string{"tool": "canter_draft_change"},
		},
		"changeApproval": map[string]any{
			"semantics":        "agent requests a short-lived route bound to one drafted Change digest; a signed-in human consumes it once to authorize and enqueue only that exact Change",
			"expiresInSeconds": int(changeApprovalCapabilityLifetime.Seconds()),
			"http": map[string]string{
				"method": "POST", "path": prefix + "/systems/{system}/changes/{changeId}/approval-links", "principal": "agent",
			},
			"mcp": map[string]string{"tool": "canter_request_change_approval"},
		},
		"initialDeployment": map[string]any{
			"workflow": map[string]any{
				"agent":      "upload and draft, then inspect the durable proposal and execution; agents cannot authorize or apply an initial deployment",
				"human":      "the review surface records authorization of the exact digest, then separately enqueues that unchanged proposal for server-owned execution",
				"webAction":  "the default human review action performs both explicit ledger transitions as approve + start deployment",
				"correction": "an immutable legacy proposal that failed on an unsupported class cannot be retried; when its ledger proves no runtime mutation occurred, an agent may draft a corrected proposal for the same System name and artifact, and Canter transfers the original beta usage reservation",
			},
			"systemM1Prefix": "server-derived from workspace and System name; any safe client suggestion is replaced before digesting",
			"artifact": map[string]any{
				"format": "tar.gz", "contentType": "application/gzip", "maxBytes": 64 << 20, "maxExpandedBytes": 512 << 20,
				"requirements": []string{"regular files and directories only", "at least one regular file", "no absolute paths, parent traversal, links, or special entries"},
			},
			"command": map[string]any{
				"form": "argv", "semantics": "command[0] must be a ./-prefixed executable regular file inside the uploaded artifact; remaining entries are passed as exact arguments without a shell",
			},
			"injectedEnvironment": map[string]any{
				"PORT":                   "internal candidate HTTP port; the application must listen on it",
				"CANTER_RELEASE_VERSION": "immutable content-derived release version",
				"serviceBindingPattern":  "CANTER_SERVICE_<UPPERCASE_SERVICE_NAME>_URL",
			},
			"constraints": map[string]any{"hostCount": 1, "hostClasses": sdk.SupportedHostClasses(), "publicHTTPServices": 1, "authorization": "human approval of exact digest required"},
			"http": map[string]any{
				"uploadArtifact": map[string]string{"method": "POST", "path": prefix + "/artifacts"},
				"draft":          map[string]string{"method": "POST", "path": prefix + "/initial-deployments"},
				"list":           map[string]string{"method": "GET", "path": prefix + "/initial-deployments"},
				"inspect":        map[string]string{"method": "GET", "path": prefix + "/initial-deployments/{deploymentId}"},
				"authorize":      map[string]string{"method": "POST", "path": prefix + "/initial-deployments/{deploymentId}/authorize", "principal": "human"},
				"apply":          map[string]string{"method": "POST", "path": prefix + "/initial-deployments/{deploymentId}/apply", "principal": "human"},
				"execution":      map[string]string{"method": "GET", "path": "/v1/initial-deployment-executions/{executionId}"},
			},
			"mcp": map[string]any{
				"url":   "/mcp",
				"tools": []string{"canter_upload_artifact", "canter_draft_initial_deployment", "canter_list_initial_deployments", "canter_inspect_initial_deployment", "canter_inspect_initial_deployment_execution"},
			},
		},
	}
}

// PutSystem is the control-plane ingress for System contracts. The client may
// omit the M1 prefix or send a safe standalone-SDK prefix, but the durable
// contract always receives the workspace-scoped server value.
func (s *Service) PutSystem(ctx context.Context, workspaceID string, system sdk.System) (SystemRecord, error) {
	canonical, err := canonicalizeSystemForWorkspace(workspaceID, system)
	if err != nil {
		return SystemRecord{}, err
	}
	return s.Store.PutSystem(ctx, workspaceID, canonical)
}

func (s *Service) InspectSystem(ctx context.Context, workspaceID, name string) (SystemView, error) {
	record, err := s.Store.GetSystem(ctx, workspaceID, name)
	if err != nil {
		return SystemView{}, err
	}
	var internal sdk.SystemView
	if s.Engine == nil {
		internal, err = sdk.CompileSystemView(record.Contract)
	} else {
		internal, err = s.Engine.InspectSystem(ctx, record.Contract)
	}
	if err != nil {
		return SystemView{}, err
	}
	return publicSystemView(internal), nil
}

func publicSystemView(internal sdk.SystemView) SystemView {
	view := SystemView{
		SchemaVersion: internal.SchemaVersion,
		Contract:      internal.Contract,
		Graph:         internal.Graph,
		Bindings:      internal.Bindings,
		Release:       internal.Release,
	}
	if internal.Host != nil {
		host := &HostObservation{Phase: internal.Host.Phase, Class: internal.Host.Class, Count: len(internal.Host.Resources), RequiresOperator: internal.Host.Phase == "escalated" || internal.Host.Failure != ""}
		if host.Class == "" {
			host.Class = internal.Contract.Spec.Constraints.Host.Class
		}
		if host.Count == 0 && internal.Host.Phase != "destroyed" {
			host.Count = internal.Contract.Spec.Constraints.Host.Count
		}
		for index, resource := range internal.Host.Resources {
			host.Resources = append(host.Resources, ComputeResourceObservation{Name: fmt.Sprintf("compute-%d", index+1), Status: resource.Status})
		}
		if intent := internal.Host.ExposureIntent; intent != nil {
			host.Exposure = &ExposureObservation{Phase: intent.Phase, Protocol: intent.Protocol, Port: intent.Port, Managed: true, MutationUnresolved: intent.MutationUnresolved}
		}
		view.Host = host
	}
	for _, service := range internal.Contract.Spec.Services {
		_, maximum, capacityErr := sdk.ScaleCapacity(internal.Contract, service.Name)
		if capacityErr != nil {
			continue
		}
		capacity := &ApplicationCapacityObservation{Service: service.Name, Mode: "existing-host", DeclaredBaseline: service.Instances, MaximumReplicas: maximum, DesiredReplicas: service.Instances, ReadyReplicas: 0}
		if internal.Release != nil {
			capacity.DesiredReplicas = internal.Release.Release.DesiredReplicas
			capacity.ReadyReplicas = internal.Release.Release.ReadyReplicas
			if capacity.DesiredReplicas < 1 {
				capacity.DesiredReplicas = service.Instances
			}
			if capacity.ReadyReplicas < 1 && internal.Release.Release.PID > 0 {
				capacity.ReadyReplicas = 1
			}
		}
		view.ApplicationCapacity = capacity
		break
	}
	// Engine errors can contain provider endpoints, request IDs, or resource
	// identities. Public readers need the semantic condition, not the backend
	// diagnostic string.
	for range internal.Issues {
		view.Issues = append(view.Issues, "System observation requires operator inspection")
	}
	return view
}

func (s *Service) DraftChange(ctx context.Context, workspaceID, systemName string, request sdk.ChangeRequest, actor sdk.ActorRef) (sdk.Change, error) {
	if s.Engine == nil {
		return sdk.Change{}, fmt.Errorf("execution engine is unavailable")
	}
	record, err := s.Store.GetSystem(ctx, workspaceID, systemName)
	if err != nil {
		return sdk.Change{}, err
	}
	workspace, err := s.Store.Workspace(ctx, workspaceID)
	if err != nil {
		return sdk.Change{}, err
	}
	engineCtx := sdk.WithActor(ctx, actor)
	engineCtx = sdk.WithChangeBaseRevision(engineCtx, sdk.ChangeBaseRevision{WorkspaceID: workspaceID, WorkspaceRevision: workspace.Revision, SystemRevision: record.Revision})
	change, err := s.Engine.DraftChangeRequest(engineCtx, record.Contract, request)
	if err != nil {
		return sdk.Change{}, err
	}
	if err = s.Store.RecordChange(ctx, workspaceID, change); err != nil {
		return sdk.Change{}, fmt.Errorf("change drafted in engine but control-plane mirror failed: %w", err)
	}
	_ = s.Store.Audit(ctx, workspaceID, actor, "change.drafted", change.ID, map[string]any{"system": systemName, "digest": change.Digest})
	return change, nil
}

func (s *Service) InspectChange(ctx context.Context, workspaceID, systemName, changeID string) (sdk.Change, error) {
	record, err := s.Store.GetSystem(ctx, workspaceID, systemName)
	if err != nil {
		return sdk.Change{}, err
	}
	if s.Engine == nil {
		return s.Store.GetRecordedChange(ctx, workspaceID, systemName, changeID)
	}
	change, err := s.Engine.InspectChange(ctx, record.Contract, changeID)
	if err != nil {
		return sdk.Change{}, err
	}
	_ = s.Store.RecordChange(ctx, workspaceID, change)
	return change, nil
}

func (s *Service) InspectChangeWithExecution(ctx context.Context, workspaceID, systemName, changeID string) (ChangeInspection, error) {
	change, err := s.InspectChange(ctx, workspaceID, systemName, changeID)
	if err != nil {
		return ChangeInspection{}, err
	}
	detail := ChangeInspection{Change: change}
	execution, err := s.Store.ExecutionForChange(ctx, workspaceID, systemName, changeID)
	if err == nil {
		detail.Execution = &execution
	} else if !errors.Is(err, ErrNotFound) {
		return ChangeInspection{}, err
	}
	return publicChangeInspection(detail), nil
}

func (s *Service) ApplyChangeUnderPolicy(ctx context.Context, workspaceID, systemName, changeID, digest string, principal Principal) (PolicyApplyResult, error) {
	if s.Engine == nil {
		return PolicyApplyResult{}, fmt.Errorf("execution engine is unavailable")
	}
	if principal.Installation == nil || principal.Session == nil || !principal.Installation.Authority.Draft {
		return PolicyApplyResult{}, ErrForbidden
	}
	record, err := s.Store.GetSystem(ctx, workspaceID, systemName)
	if err != nil {
		return PolicyApplyResult{}, err
	}
	workspace, err := s.Store.Workspace(ctx, workspaceID)
	if err != nil {
		return PolicyApplyResult{}, err
	}
	change, err := s.Engine.InspectChange(ctx, record.Contract, changeID)
	if err != nil {
		return PolicyApplyResult{}, err
	}
	if digest == "" || digest != change.Digest {
		return PolicyApplyResult{}, fmt.Errorf("%w: exact Change digest does not match", ErrConflict)
	}
	decision, policy, err := s.Store.EvaluateStandingPolicies(ctx, workspaceID, systemName, principal.Installation.ID, workspace.Revision, record.Revision, change)
	if err != nil {
		return PolicyApplyResult{}, err
	}
	result := PolicyApplyResult{Decision: decision, Policy: policy, Change: change}
	if decision.Outcome != "automatic" || policy == nil {
		_ = s.Store.Audit(ctx, workspaceID, principal.Actor, "change.policy-evaluated", change.ID, map[string]any{"digest": change.Digest, "decisionId": decision.ID, "outcome": decision.Outcome})
		return result, nil
	}
	if decision.ExecutionID != "" {
		execution, executionErr := s.Store.Execution(ctx, decision.ExecutionID)
		if executionErr != nil {
			return PolicyApplyResult{}, executionErr
		}
		result.Execution = &execution
		return result, nil
	}
	policyActor := sdk.ActorRef{Kind: "policy", ID: policy.ID, SessionID: decision.ID, DisplayName: policy.Name}
	if change.Phase == "drafted" {
		change, err = s.AuthorizeChange(ctx, workspaceID, systemName, changeID, digest, policyActor)
		if err != nil {
			decision, _ = s.Store.updatePolicyDecision(context.WithoutCancel(ctx), decision.ID, "failed", "", err.Error())
			result.Decision = decision
			return result, err
		}
	} else if change.Authorization == nil || change.Authorization.Digest != digest || change.Authorization.AuthorizedBy == nil || change.Authorization.AuthorizedBy.Kind != "policy" || change.Authorization.AuthorizedBy.ID != policy.ID {
		return PolicyApplyResult{}, fmt.Errorf("%w: Change was not authorized by the matched policy", ErrConflict)
	}
	decision, err = s.Store.updatePolicyDecision(ctx, decision.ID, "authorized", "", "")
	if err != nil {
		return PolicyApplyResult{}, err
	}
	execution, err := s.Store.EnqueueExecution(ctx, workspaceID, systemName, changeID, policyActor)
	if err != nil {
		decision, _ = s.Store.updatePolicyDecision(context.WithoutCancel(ctx), decision.ID, "failed", "", err.Error())
		result.Decision = decision
		return result, err
	}
	decision, err = s.Store.updatePolicyDecision(ctx, decision.ID, "queued", execution.ID, "")
	if err != nil {
		return PolicyApplyResult{}, err
	}
	result.Decision, result.Change, result.Execution = decision, change, &execution
	_ = s.Store.Audit(ctx, workspaceID, policyActor, "change.policy-authorized-and-queued", change.ID, map[string]any{"digest": digest, "decisionId": decision.ID, "policyId": policy.ID, "policyDigest": policy.Digest, "createdByAccount": policy.CreatedByAccount, "executionId": execution.ID, "evaluatedByInstallation": principal.Installation.ID})
	return result, nil
}

func (s *Service) AuthorizeChange(ctx context.Context, workspaceID, systemName, changeID, digest string, actor sdk.ActorRef) (sdk.Change, error) {
	if s.Engine == nil {
		return sdk.Change{}, fmt.Errorf("execution engine is unavailable")
	}
	record, err := s.Store.GetSystem(ctx, workspaceID, systemName)
	if err != nil {
		return sdk.Change{}, err
	}
	current, err := s.Engine.InspectChange(ctx, record.Contract, changeID)
	if err != nil {
		return sdk.Change{}, err
	}
	workspace, err := s.Store.Workspace(ctx, workspaceID)
	if err != nil {
		return sdk.Change{}, err
	}
	if base := current.Plan.BaseRevision; base.SystemRevision != 0 && (base.SystemRevision != record.Revision || base.WorkspaceRevision != workspace.Revision) {
		return sdk.Change{}, fmt.Errorf("%w: change base revision is stale", ErrConflict)
	}
	change, err := s.Engine.AuthorizeChange(sdk.WithActor(ctx, actor), record.Contract, changeID, digest)
	if err != nil {
		return sdk.Change{}, err
	}
	if err = s.Store.RecordChange(ctx, workspaceID, change); err != nil {
		return sdk.Change{}, err
	}
	_ = s.Store.Audit(ctx, workspaceID, actor, "change.authorized", change.ID, map[string]any{"system": systemName, "digest": digest})
	return change, nil
}

type Dispatcher struct {
	Store         *Store
	Engine        Engine
	WorkerID      string
	PollInterval  time.Duration
	LeaseDuration time.Duration
}

func (d *Dispatcher) Run(ctx context.Context) error {
	if d.Engine == nil {
		return fmt.Errorf("execution engine is unavailable")
	}
	if d.WorkerID == "" {
		d.WorkerID = "control-plane"
	}
	if d.PollInterval <= 0 {
		d.PollInterval = 500 * time.Millisecond
	}
	if d.LeaseDuration <= 0 {
		d.LeaseDuration = 30 * time.Second
	}
	ticker := time.NewTicker(d.PollInterval)
	defer ticker.Stop()
	for {
		execution, ok, err := d.Store.ClaimExecution(ctx, d.WorkerID, d.LeaseDuration)
		if err != nil {
			return err
		}
		if ok {
			d.runOne(ctx, execution)
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) runOne(parent context.Context, execution Execution) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(d.LeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := d.Store.RenewExecution(context.WithoutCancel(ctx), execution.ID, d.WorkerID, d.LeaseDuration); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	record, err := d.Store.GetSystem(ctx, execution.WorkspaceID, execution.SystemName)
	if err == nil {
		workspace, workspaceErr := d.Store.Workspace(ctx, execution.WorkspaceID)
		if workspaceErr != nil {
			err = workspaceErr
		}
		var change sdk.Change
		if err == nil {
			change, err = d.Engine.InspectChange(ctx, record.Contract, execution.ChangeID)
		}
		if err == nil {
			if base := change.Plan.BaseRevision; base.SystemRevision != 0 && (base.SystemRevision != record.Revision || base.WorkspaceRevision != workspace.Revision) {
				err = fmt.Errorf("%w: change base revision is stale", ErrConflict)
			}
		}
		if err == nil {
			change, err = d.Engine.ApplyChange(sdk.WithActor(ctx, sdk.ActorRef{Kind: "canter", ID: d.WorkerID}), record.Contract, execution.ChangeID)
		}
		if change.ID != "" {
			_ = d.Store.RecordChange(context.WithoutCancel(ctx), execution.WorkspaceID, change)
		}
	}
	cancel()
	wg.Wait()
	_ = d.Store.CompleteExecution(context.WithoutCancel(parent), execution.ID, d.WorkerID, err)
}
