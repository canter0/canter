package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/canter0/canter/internal/computeclass"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

// InitialDeploymentEngine is the real provider-backed boundary used only by
// the server. Remote agents upload bytes and submit intent; they never receive
// m1 or compute credentials.
type InitialDeploymentEngine interface {
	StageControlPlaneArtifact(context.Context, []byte, string, string) (sdk.StagedArtifact, error)
	VerifyStagedArtifact(context.Context, sdk.StagedArtifact) error
	BootstrapSystemHostViaGateway(context.Context, sdk.System, []byte, sdk.NodeBootstrapConfig) (sdk.ApplyResult, error)
	SystemHostStatus(context.Context, sdk.System) (sdk.State, error)
	ExposeSystemHost(context.Context, sdk.System) (sdk.State, error)
	PublishStagedRelease(context.Context, sdk.System, sdk.StagedReleaseInput) (sdk.ReleaseManifest, error)
	WaitPublicEndpoint(context.Context, sdk.System) (sdk.ReleaseView, error)
	VerifyPublicEndpoint(context.Context, sdk.System, string, sdk.ChangeVerification) (sdk.PublicEndpointObservation, error)
}

type initialDeploymentExposureRecoveryEngine interface {
	RecoverSystemHostExposure(context.Context, sdk.System) (sdk.State, error)
}

const (
	maxArtifactBytes         = 64 << 20
	maxExpandedArtifactBytes = 512 << 20
	maxArtifactEntries       = 4096
	maxArtifactPathBytes     = 512
	maxArtifactMetadataBytes = 16 << 10
)

type DraftInitialDeploymentInput struct {
	Summary        string                   `json:"summary"`
	System         sdk.System               `json:"system"`
	ArtifactSHA256 string                   `json:"artifactSha256"`
	Release        InitialDeploymentRelease `json:"release"`
	Verification   sdk.ChangeVerification   `json:"verification"`
}

func (s *Service) UploadDeploymentArtifact(ctx context.Context, workspaceID string, data []byte, filename, contentType string, actor sdk.ActorRef) (DeploymentArtifact, error) {
	entries, err := validateApplicationArtifact(data)
	if err != nil {
		return DeploymentArtifact{}, err
	}
	engine, ok := s.Engine.(InitialDeploymentEngine)
	if !ok {
		return DeploymentArtifact{}, fmt.Errorf("initial deployment engine is unavailable")
	}
	staged, err := engine.StageControlPlaneArtifact(ctx, data, filename, contentType)
	if err != nil {
		return DeploymentArtifact{}, err
	}
	record, err := s.Store.RecordDeploymentArtifact(ctx, workspaceID, staged, entries, actor)
	if err != nil {
		return DeploymentArtifact{}, fmt.Errorf("artifact staged but durable ownership record failed: %w", err)
	}
	_ = s.Store.Audit(ctx, workspaceID, actor, "artifact.uploaded", record.SHA256, map[string]any{"size": record.Size, "contentType": record.ContentType})
	return record, nil
}

func validateApplicationArtifact(data []byte) ([]DeploymentArtifactEntry, error) {
	if len(data) == 0 || len(data) > maxArtifactBytes {
		return nil, fmt.Errorf("artifact must be between 1 byte and %d bytes", maxArtifactBytes)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("artifact must be a tar.gz bundle: %w", err)
	}
	defer gz.Close()
	gz.Multistream(false)
	expandedStream := &io.LimitedReader{R: gz, N: maxExpandedArtifactBytes + 1}
	reader := tar.NewReader(expandedStream)
	var expandedPayload int64
	var entries []DeploymentArtifactEntry
	totalEntries := 0
	seen := make(map[string]byte)
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, fmt.Errorf("read tar.gz artifact: %w", nextErr)
		}
		if totalEntries >= maxArtifactEntries {
			return nil, fmt.Errorf("artifact contains more than %d entries", maxArtifactEntries)
		}
		totalEntries++
		if len(header.Name) == 0 || len(header.Name) > maxArtifactPathBytes || strings.Contains(header.Name, "\\") {
			return nil, fmt.Errorf("artifact contains invalid path %q", header.Name)
		}
		clean := path.Clean(header.Name)
		canonicalName := header.Name
		if header.Typeflag == tar.TypeDir {
			canonicalName = strings.TrimSuffix(canonicalName, "/")
		}
		if clean != canonicalName || clean == "." || clean == ".." || path.IsAbs(header.Name) || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("artifact contains unsafe path %q", header.Name)
		}
		if _, duplicate := seen[clean]; duplicate {
			return nil, fmt.Errorf("artifact contains duplicate path %q", clean)
		}
		for ancestor := path.Dir(clean); ancestor != "." && ancestor != "/"; ancestor = path.Dir(ancestor) {
			if seen[ancestor] == tar.TypeReg {
				return nil, fmt.Errorf("artifact path %q descends from regular file %q", clean, ancestor)
			}
		}
		if header.Typeflag == tar.TypeReg {
			for existing := range seen {
				if strings.HasPrefix(existing, clean+"/") {
					return nil, fmt.Errorf("artifact regular file %q conflicts with child path %q", clean, existing)
				}
			}
		}
		metadataBytes := len(header.Uname) + len(header.Gname) + len(header.Linkname)
		if len(header.PAXRecords) > 32 || len(header.Xattrs) > 32 {
			return nil, fmt.Errorf("artifact header %q contains excessive metadata", clean)
		}
		for key, value := range header.PAXRecords {
			metadataBytes += len(key) + len(value)
		}
		for key, value := range header.Xattrs {
			metadataBytes += len(key) + len(value)
		}
		if metadataBytes > maxArtifactMetadataBytes || header.Mode < 0 || header.Mode&^int64(0o777) != 0 || header.Size < 0 {
			return nil, fmt.Errorf("artifact header %q exceeds safe metadata limits", clean)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return nil, fmt.Errorf("artifact directory %q declares data", clean)
			}
		case tar.TypeReg:
			entries = append(entries, DeploymentArtifactEntry{Path: clean, Mode: header.Mode & 0o777, Size: header.Size})
			expandedPayload += header.Size
			if expandedPayload > maxExpandedArtifactBytes {
				return nil, fmt.Errorf("artifact expands beyond %d bytes", maxExpandedArtifactBytes)
			}
			if _, copyErr := io.CopyN(io.Discard, reader, header.Size); copyErr != nil {
				return nil, fmt.Errorf("read artifact member %q: %w", header.Name, copyErr)
			}
		default:
			return nil, fmt.Errorf("artifact contains unsupported entry %q", header.Name)
		}
		seen[clean] = header.Typeflag
	}
	if maxExpandedArtifactBytes+1-expandedStream.N > maxExpandedArtifactBytes {
		return nil, fmt.Errorf("artifact expanded stream exceeds %d bytes", maxExpandedArtifactBytes)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("artifact contains no files")
	}
	return entries, nil
}

var deploymentEnvironmentName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)

func (s *Service) DraftInitialDeployment(ctx context.Context, workspaceID string, input DraftInitialDeploymentInput, actor sdk.ActorRef) (InitialDeployment, error) {
	canonical, err := canonicalizeSystemForWorkspace(workspaceID, input.System)
	if err != nil {
		return InitialDeployment{}, err
	}
	input.System = canonical
	if strings.TrimSpace(input.Summary) == "" {
		return InitialDeployment{}, fmt.Errorf("initial deployment summary is required")
	}
	if len(input.Release.Command) == 0 || input.Release.PublicPort < 1 || input.Release.PublicPort > 65535 || !strings.HasPrefix(input.Release.HealthPath, "/") {
		return InitialDeployment{}, fmt.Errorf("release requires a command, absolute health path, and valid public port")
	}
	for name := range input.Release.Environment {
		if !deploymentEnvironmentName.MatchString(name) || strings.HasPrefix(name, "CANTER_") {
			return InitialDeployment{}, fmt.Errorf("environment name %q is reserved or invalid", name)
		}
	}
	if input.Verification.Method == "" {
		input.Verification.Method = http.MethodGet
	}
	if input.Verification.ExpectedStatus == 0 {
		input.Verification.ExpectedStatus = http.StatusOK
	}
	if input.Verification.Method != http.MethodGet || !strings.HasPrefix(input.Verification.Path, "/") || input.Verification.ExpectedStatus < 100 || input.Verification.ExpectedStatus > 599 {
		return InitialDeployment{}, fmt.Errorf("verification requires GET, an absolute path, and a valid expected status")
	}
	replacesDeploymentID := ""
	if existing, err := s.Store.GetSystem(ctx, workspaceID, input.System.Metadata.Name); err == nil {
		existingRaw, marshalErr := json.Marshal(existing.Contract)
		if marshalErr != nil {
			return InitialDeployment{}, marshalErr
		}
		requestedRaw, marshalErr := json.Marshal(input.System)
		if marshalErr != nil {
			return InitialDeployment{}, marshalErr
		}
		existingMatches := bytes.Equal(existingRaw, requestedRaw)
		replacementSource, replacementErr := failedUnsupportedClassReplacementSource(ctx, s.Store, workspaceID, input.System.Metadata.Name, "", existing.Contract)
		if replacementErr != nil {
			return InitialDeployment{}, replacementErr
		}
		if !existingMatches && replacementSource == "" {
			return InitialDeployment{}, fmt.Errorf("%w: a different System contract already uses this name", ErrConflict)
		}
		if !existingMatches {
			replacesDeploymentID = replacementSource
		}
		if existingMatches {
			view, inspectErr := s.InspectSystem(ctx, workspaceID, input.System.Metadata.Name)
			if inspectErr != nil {
				return InitialDeployment{}, inspectErr
			}
			if view.Host != nil || view.Release != nil {
				return InitialDeployment{}, fmt.Errorf("%w: System already has observed runtime state; draft a Change instead", ErrConflict)
			}
			deployments, listErr := s.Store.ListInitialDeployments(ctx, workspaceID)
			if listErr != nil {
				return InitialDeployment{}, listErr
			}
			for _, deployment := range deployments {
				if deployment.System == input.System.Metadata.Name {
					return InitialDeployment{}, fmt.Errorf("%w: System already has an initial deployment proposal", ErrConflict)
				}
			}
		}
	} else if !errors.Is(err, ErrNotFound) {
		return InitialDeployment{}, err
	}
	artifact, _, err := s.Store.DeploymentArtifact(ctx, workspaceID, input.ArtifactSHA256)
	if err != nil {
		return InitialDeployment{}, err
	}
	if !strings.HasPrefix(input.Release.Command[0], "./") {
		return InitialDeployment{}, fmt.Errorf("release command must start with ./ and name an executable file inside the artifact")
	}
	commandPath := strings.TrimPrefix(input.Release.Command[0], "./")
	if path.Clean(commandPath) != commandPath || commandPath == "." {
		return InitialDeployment{}, fmt.Errorf("release command must start with ./ and name an executable file inside the artifact")
	}
	executable := false
	for _, entry := range artifact.Entries {
		if entry.Path == commandPath && entry.Mode&0o100 != 0 {
			executable = true
			break
		}
	}
	if !executable {
		return InitialDeployment{}, fmt.Errorf("release command %q is not an executable file in artifact %s", input.Release.Command[0], artifact.SHA256)
	}
	workspace, err := s.Store.Workspace(ctx, workspaceID)
	if err != nil {
		return InitialDeployment{}, err
	}
	plan := InitialDeploymentPlan{System: input.System, ArtifactSHA256: input.ArtifactSHA256, Release: input.Release, Verification: input.Verification, WorkspaceRevision: workspace.Revision, ReplacesDeploymentID: replacesDeploymentID}
	digest, err := digestInitialDeployment(plan)
	if err != nil {
		return InitialDeployment{}, err
	}
	id, _ := newID("dep_")
	now := s.Store.now()
	deployment := InitialDeployment{
		SchemaVersion: "v1", ID: id, WorkspaceID: workspaceID, System: input.System.Metadata.Name,
		Summary: strings.TrimSpace(input.Summary), Phase: "drafted", Digest: digest, DraftedBy: actor, Plan: plan,
		Operations: []InitialDeploymentOperation{
			{ID: "01-register-system", Kind: "system.register", Description: "register the exact approved System contract", Phase: "pending"},
			{ID: "02-bootstrap-host", Kind: "system-host.bootstrap", Description: "provision compute and install the Canter node runtime", Phase: "pending"},
			{ID: "03-publish-release", Kind: "release.publish-staged", Description: "publish the approved content-addressed artifact", Phase: "pending"},
			{ID: "04-wait-healthy", Kind: "release.wait-public", Description: "wait for the release and managed public endpoint", Phase: "pending"},
			{ID: "05-verify-public", Kind: "http.verify", Description: "run the exact approved public verification", Phase: "pending"},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.Store.CreateInitialDeployment(ctx, deployment); err != nil {
		return InitialDeployment{}, err
	}
	_ = s.Store.Audit(ctx, workspaceID, actor, "initial-deployment.drafted", id, map[string]any{"system": deployment.System, "digest": digest, "artifactSha256": input.ArtifactSHA256})
	return deployment, nil
}

// failedUnsupportedClassReplacementSource proves that the only prior attempt
// for this System stopped at class resolution, before a durable creation intent
// or provider mutation could exist. It is the sole case where a first-deploy
// proposal may replace an already registered contract under the same name.
func failedUnsupportedClassReplacementSource(ctx context.Context, store *Store, workspaceID, systemName, excludeDeploymentID string, existingContract sdk.System) (string, error) {
	deployments, err := store.ListInitialDeployments(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	source := ""
	for _, index := range deployments {
		if index.System != systemName || index.ID == excludeDeploymentID {
			continue
		}
		if index.Phase != "failed" {
			return "", nil
		}
		deployment, err := store.InitialDeployment(ctx, workspaceID, index.ID)
		if err != nil {
			return "", err
		}
		if !failedBeforeRuntimeMutationForUnsupportedClass(deployment) {
			return "", nil
		}
		existingRaw, existingErr := json.Marshal(existingContract)
		sourceRaw, sourceErr := json.Marshal(deployment.Plan.System)
		if existingErr != nil || sourceErr != nil {
			return "", errors.Join(existingErr, sourceErr)
		}
		if !bytes.Equal(existingRaw, sourceRaw) {
			return "", nil
		}
		if source == "" {
			source = deployment.ID
		}
	}
	return source, nil
}

func failedBeforeRuntimeMutationForUnsupportedClass(deployment InitialDeployment) bool {
	if deployment.Phase != "failed" || !operationSucceeded(deployment, "01-register-system") {
		return false
	}
	if computeclass.Validate(deployment.Plan.System.Spec.Constraints.Host.Class) == nil {
		return false
	}
	bootstrapFailed := false
	laterPending := 0
	for _, operation := range deployment.Operations {
		switch operation.ID {
		case "02-bootstrap-host":
			if operation.Phase != "failed" || !computeclass.IsSafePublicFailure(operation.Failure) {
				return false
			}
			bootstrapFailed = true
		case "03-publish-release", "04-wait-healthy", "05-verify-public":
			if operation.Phase != "pending" {
				return false
			}
			laterPending++
		}
	}
	return bootstrapFailed && laterPending == 3
}

func digestInitialDeployment(plan InitialDeploymentPlan) (string, error) {
	raw, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Service) AuthorizeInitialDeployment(ctx context.Context, workspaceID, deploymentID, digest string, actor sdk.ActorRef) (InitialDeployment, error) {
	deployment, err := s.Store.AuthorizeInitialDeployment(ctx, workspaceID, deploymentID, digest, actor)
	if err != nil {
		return InitialDeployment{}, err
	}
	_ = s.Store.Audit(ctx, workspaceID, actor, "initial-deployment.authorized", deploymentID, map[string]any{"system": deployment.System, "digest": digest})
	return deployment, nil
}

type InitialDeploymentDispatcher struct {
	Store         *Store
	Service       *Service
	Engine        InitialDeploymentEngine
	NodeBinary    []byte
	WorkerID      string
	PollInterval  time.Duration
	LeaseDuration time.Duration
	WaitTimeout   time.Duration
	// ObservationPollInterval controls only the retry cadence while the node's
	// first observed-release object does not exist yet. The overall bound is
	// still WaitTimeout and the execution lease context.
	ObservationPollInterval time.Duration
}

func (d *InitialDeploymentDispatcher) Run(ctx context.Context) error {
	if d.Engine == nil {
		return fmt.Errorf("initial deployment engine is unavailable")
	}
	if d.Store == nil || d.Service == nil || d.Service.Store != d.Store {
		return fmt.Errorf("initial deployment control-plane service is unavailable")
	}
	if d.WorkerID == "" {
		d.WorkerID = "control-plane/initial-deployment"
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
		execution, ok, err := d.Store.ClaimInitialDeploymentExecution(ctx, d.WorkerID, d.LeaseDuration)
		if err != nil {
			return err
		}
		if ok {
			if err := d.runOne(ctx, execution); err != nil {
				return fmt.Errorf("complete initial deployment execution %s: %w", execution.ID, err)
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (d *InitialDeploymentDispatcher) runOne(parent context.Context, execution InitialDeploymentExecution) error {
	ctx, cancel := context.WithCancel(parent)
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
				if err := d.Store.RenewInitialDeploymentExecution(context.WithoutCancel(ctx), execution.ID, d.WorkerID, execution.ClaimToken, d.LeaseDuration); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	err := d.execute(ctx, execution)
	cancel()
	wg.Wait()
	return d.Store.CompleteInitialDeploymentExecution(context.WithoutCancel(parent), execution.ID, d.WorkerID, execution.ClaimToken, err)
}

func (d *InitialDeploymentDispatcher) execute(ctx context.Context, execution InitialDeploymentExecution) error {
	deployment, err := d.Store.InitialDeployment(ctx, execution.WorkspaceID, execution.DeploymentID)
	if err != nil {
		return err
	}
	if deployment.Authorization == nil || deployment.Authorization.Digest != deployment.Digest {
		return fmt.Errorf("%w: deployment is not authorized for its exact digest", ErrConflict)
	}
	recomputedDigest, err := digestInitialDeployment(deployment.Plan)
	if err != nil {
		return err
	}
	if recomputedDigest != deployment.Digest {
		return fmt.Errorf("%w: deployment plan no longer matches its immutable digest", ErrConflict)
	}
	artifact, staged, err := d.Store.DeploymentArtifact(ctx, execution.WorkspaceID, deployment.Plan.ArtifactSHA256)
	if err != nil {
		return err
	}
	_ = artifact
	system := deployment.Plan.System
	workspace, err := d.Store.Workspace(ctx, execution.WorkspaceID)
	if err != nil {
		return err
	}
	existing, existingErr := d.Store.GetSystem(ctx, execution.WorkspaceID, system.Metadata.Name)
	existingMatches := false
	replacementAllowed := false
	if existingErr == nil {
		existingRaw, _ := json.Marshal(existing.Contract)
		approvedRaw, _ := json.Marshal(system)
		if string(existingRaw) != string(approvedRaw) {
			replacementSource, replacementErr := failedUnsupportedClassReplacementSource(ctx, d.Store, execution.WorkspaceID, system.Metadata.Name, deployment.ID, existing.Contract)
			err = replacementErr
			if err != nil {
				return err
			}
			replacementAllowed = replacementSource != "" && replacementSource == deployment.Plan.ReplacesDeploymentID
			if !replacementAllowed {
				return fmt.Errorf("%w: a different System contract now uses this name", ErrConflict)
			}
		} else {
			existingMatches = true
		}
	} else if !errors.Is(existingErr, ErrNotFound) {
		return existingErr
	}
	if !initialDeploymentWorkspaceRevisionValid(deployment, workspace.Revision, existingMatches) {
		return fmt.Errorf("%w: workspace changed after the initial deployment was drafted", ErrConflict)
	}
	if err = d.operation(ctx, execution, "01-register-system", func() (string, error) {
		existing, getErr := d.Store.GetSystem(ctx, execution.WorkspaceID, system.Metadata.Name)
		if getErr == nil {
			existingRaw, _ := json.Marshal(existing.Contract)
			approvedRaw, _ := json.Marshal(system)
			if string(existingRaw) != string(approvedRaw) {
				if !replacementAllowed {
					return "", fmt.Errorf("%w: a different System contract now uses this name", ErrConflict)
				}
				if _, putErr := d.Store.PutSystem(ctx, execution.WorkspaceID, system); putErr != nil {
					return "", putErr
				}
				return "failed pre-runtime System contract replaced by the corrected approved contract", nil
			}
			return "approved System contract already registered", nil
		}
		if !errors.Is(getErr, ErrNotFound) {
			return "", getErr
		}
		if _, putErr := d.Store.PutSystem(ctx, execution.WorkspaceID, system); putErr != nil {
			return "", putErr
		}
		return "approved System contract registered", nil
	}); err != nil {
		return err
	}
	if state, statusErr := d.Engine.SystemHostStatus(ctx, system); statusErr == nil && state.Phase == "destroyed" {
		if err := d.Store.ReopenDestroyedInitialDeploymentHost(ctx, execution.ID, d.WorkerID, execution.ClaimToken, execution.DeploymentID); err != nil {
			return fmt.Errorf("reopen destroyed System host after human retry: %w", err)
		}
	}
	if err = d.operation(ctx, execution, "02-bootstrap-host", func() (string, error) {
		if len(d.NodeBinary) == 0 {
			return "", fmt.Errorf("control plane has no Canter node binary configured")
		}
		state, statusErr := d.Engine.SystemHostStatus(ctx, system)
		if statusErr == nil && state.Phase == "ready" && len(state.Resources) == system.Spec.Constraints.Host.Count {
			if systemHostEndpointReady(system, state) {
				return "existing Canter System host and public endpoint are ready", nil
			}
			reconciled, exposeErr := d.Engine.ExposeSystemHost(ctx, system)
			if exposeErr != nil {
				return "", fmt.Errorf("reconcile existing System host exposure: %w", exposeErr)
			}
			if !systemHostEndpointReady(system, reconciled) {
				return "", fmt.Errorf("existing System host exposure did not reach durable ready state")
			}
			return "existing Canter System host public endpoint reconciled", nil
		}
		if statusErr == nil && state.Phase == "escalated" && len(state.Resources) == system.Spec.Constraints.Host.Count && state.ExposureIntent != nil && state.ExposureIntent.Phase == "escalated" && state.ExposureIntent.MutationUnresolved {
			priorFailed, historyErr := d.Store.HasPriorFailedInitialDeploymentExecution(ctx, execution.DeploymentID, execution.ID)
			if historyErr != nil {
				return "", fmt.Errorf("inspect prior deployment execution: %w", historyErr)
			}
			if !priorFailed {
				return "", fmt.Errorf("existing Canter System host exposure is escalated and requires an explicit human retry")
			}
			recoveryEngine, ok := d.Engine.(initialDeploymentExposureRecoveryEngine)
			if !ok {
				return "", fmt.Errorf("initial deployment engine cannot recover an escalated public endpoint")
			}
			reconciled, recoverErr := recoveryEngine.RecoverSystemHostExposure(ctx, system)
			if recoverErr != nil {
				return "", fmt.Errorf("recover existing System host exposure after human retry: %w", recoverErr)
			}
			if !systemHostEndpointReady(system, reconciled) {
				return "", fmt.Errorf("recovered System host exposure did not reach durable ready state")
			}
			return "existing Canter System host public endpoint recovered after explicit human retry", nil
		}
		if statusErr == nil && state.Phase != "destroyed" && len(state.Resources) > 0 {
			return "", fmt.Errorf("existing Canter System host is in non-ready phase %q and requires operator recovery", state.Phase)
		}
		if d.Service == nil || d.Service.Store != d.Store {
			return "", fmt.Errorf("initial deployment control-plane service is unavailable")
		}
		_, gateway, prepareErr := d.Service.PrepareNodeBootstrap(ctx, execution.WorkspaceID, system.Metadata.Name)
		if prepareErr != nil {
			return "", prepareErr
		}
		result, bootstrapErr := d.Engine.BootstrapSystemHostViaGateway(ctx, system, d.NodeBinary, gateway)
		if bootstrapErr != nil {
			return "", bootstrapErr
		}
		return fmt.Sprintf("Canter System host bootstrapped as operation %s", result.OperationID), nil
	}); err != nil {
		return err
	}
	release := sdk.ReleaseManifest{Version: staged.SHA256[:12]}
	if err = d.operation(ctx, execution, "03-publish-release", func() (string, error) {
		if verifyErr := d.Engine.VerifyStagedArtifact(ctx, staged); verifyErr != nil {
			return "", verifyErr
		}
		release, err = d.Engine.PublishStagedRelease(ctx, system, sdk.StagedReleaseInput{Artifact: staged, Command: deployment.Plan.Release.Command, Environment: deployment.Plan.Release.Environment, HealthPath: deployment.Plan.Release.HealthPath, PublicPort: deployment.Plan.Release.PublicPort})
		if err != nil {
			return "", err
		}
		return "desired release set to approved artifact " + staged.SHA256, nil
	}); err != nil {
		return err
	}
	waitTimeout := d.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 5 * time.Minute
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, waitTimeout)
	defer waitCancel()
	if err = d.operation(waitCtx, execution, "04-wait-healthy", func() (string, error) {
		view, waitErr := d.waitPublicEndpoint(waitCtx, system)
		if waitErr != nil {
			return "", waitErr
		}
		return "release healthy and public at " + view.PublicEndpoint.URL, nil
	}); err != nil {
		return err
	}
	return d.operation(ctx, execution, "05-verify-public", func() (string, error) {
		observation, verifyErr := d.Engine.VerifyPublicEndpoint(ctx, system, release.Version, deployment.Plan.Verification)
		if verifyErr != nil {
			return "", verifyErr
		}
		return fmt.Sprintf("HTTP %d verified at %s", observation.StatusCode, observation.URL), nil
	})
}

func initialDeploymentWorkspaceRevisionValid(deployment InitialDeployment, currentRevision int64, existingSystemMatches bool) bool {
	if currentRevision == deployment.Plan.WorkspaceRevision {
		return true
	}
	// Authorization remains bound to the original plan revision. Once that
	// exact System was durably registered by a succeeded governed operation,
	// replaying later operations does not re-authorize against the workspace's
	// current revision. The exact contract equality check remains mandatory.
	return existingSystemMatches && operationSucceeded(deployment, "01-register-system")
}

// waitPublicEndpoint treats an absent observed-release object as startup
// latency: the newly enrolled node has not published its first observation
// yet. Only that specific object-store condition is retried. Authentication,
// transport, decoding, and all other storage errors remain terminal.
func (d *InitialDeploymentDispatcher) waitPublicEndpoint(ctx context.Context, system sdk.System) (sdk.ReleaseView, error) {
	pollInterval := d.ObservationPollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	var last sdk.ReleaseView
	for {
		view, err := d.Engine.WaitPublicEndpoint(ctx, system)
		if err == nil {
			return view, nil
		}
		last = view
		if !isMissingObservedRelease(err) {
			return last, err
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("observed release was not published before the wait deadline: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

type objectStoreErrorCode interface {
	ErrorCode() string
}

func isMissingObservedRelease(err error) bool {
	var coded objectStoreErrorCode
	if !errors.As(err, &coded) {
		return false
	}
	switch coded.ErrorCode() {
	case "NoSuchKey", "NotFound":
		return true
	default:
		return false
	}
}

func systemHostEndpointReady(system sdk.System, state sdk.State) bool {
	intent := state.ExposureIntent
	if intent == nil || intent.OperationID == "" || intent.Name == "" || intent.Ownership == "" || intent.Phase != "ready" || intent.Protocol != "tcp" || len(state.Resources) != system.Spec.Constraints.Host.Count || len(state.Resources) != 1 || intent.ServerID != state.Resources[0].ID {
		return false
	}
	publicPort := 0
	for _, service := range system.Spec.Services {
		if service.Networking == "public" && service.Readiness.Protocol == "http" && service.Readiness.Port > 0 {
			if publicPort != 0 {
				return false
			}
			publicPort = service.Readiness.Port
		}
	}
	if publicPort == 0 || intent.Port != publicPort {
		return false
	}
	matches := 0
	for _, policy := range state.NetworkPolicies {
		if policy.ID != "" && policy.PortID != "" && policy.RuleID != "" && policy.Protocol == "tcp" && policy.Port == publicPort {
			matches++
		}
	}
	return matches == 1
}

func operationSucceeded(deployment InitialDeployment, operationID string) bool {
	for _, operation := range deployment.Operations {
		if operation.ID == operationID {
			return operation.Phase == "succeeded"
		}
	}
	return false
}

func (d *InitialDeploymentDispatcher) operation(ctx context.Context, execution InitialDeploymentExecution, operationID string, run func() (string, error)) error {
	shouldRun, err := d.Store.BeginInitialDeploymentOperation(ctx, execution.ID, d.WorkerID, execution.ClaimToken, execution.DeploymentID, operationID)
	if err != nil {
		return err
	}
	if !shouldRun {
		return nil
	}
	statement, err := run()
	if err != nil {
		finishErr := d.Store.FinishInitialDeploymentOperation(context.WithoutCancel(ctx), execution.ID, d.WorkerID, execution.ClaimToken, execution.DeploymentID, operationID, "failed", err.Error(), nil)
		if finishErr != nil {
			return fmt.Errorf("%v; persist failed operation: %w", err, finishErr)
		}
		return err
	}
	evidence := sdk.ChangeEvidence{OperationID: operationID, Kind: "control-plane.observation", Statement: statement, ObservedAt: time.Now().UTC()}
	return d.Store.FinishInitialDeploymentOperation(ctx, execution.ID, d.WorkerID, execution.ClaimToken, execution.DeploymentID, operationID, "succeeded", "", &evidence)
}

func scanInitialDeployment(row pgx.Row) (InitialDeployment, error) {
	var raw []byte
	var deployment InitialDeployment
	if err := row.Scan(&raw); err != nil {
		return deployment, err
	}
	return deployment, json.Unmarshal(raw, &deployment)
}

func scanInitialDeploymentWithDigest(row pgx.Row) (InitialDeployment, error) {
	var raw []byte
	var storedDigest string
	var deployment InitialDeployment
	if err := row.Scan(&raw, &storedDigest); err != nil {
		return deployment, err
	}
	if err := json.Unmarshal(raw, &deployment); err != nil {
		return deployment, err
	}
	if deployment.Digest != storedDigest {
		return deployment, fmt.Errorf("%w: deployment document digest differs from durable digest", ErrConflict)
	}
	return deployment, nil
}
