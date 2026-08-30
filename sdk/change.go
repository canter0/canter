package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/canter0/canter/internal/provider/m1"
)

type Change struct {
	SchemaVersion string            `json:"schemaVersion"`
	ID            string            `json:"id"`
	System        string            `json:"system"`
	Summary       string            `json:"summary"`
	Phase         string            `json:"phase"`
	Digest        string            `json:"digest"`
	Plan          ChangePlan        `json:"plan"`
	Authorization *Authorization    `json:"authorization,omitempty"`
	Operations    []ChangeOperation `json:"operations"`
	Evidence      []ChangeEvidence  `json:"evidence,omitempty"`
	Residuals     []string          `json:"residuals,omitempty"`
	Failure       string            `json:"failure,omitempty"`
	CreatedAt     time.Time         `json:"createdAt"`
	UpdatedAt     time.Time         `json:"updatedAt"`
	CompletedAt   *time.Time        `json:"completedAt,omitempty"`
}

type ChangePlan struct {
	BaseVersion  string             `json:"baseVersion"`
	Release      ReleaseManifest    `json:"release"`
	Migration    *Migration         `json:"migration,omitempty"`
	Verification ChangeVerification `json:"verification"`
	Impact       ChangeImpact       `json:"impact"`
}

type ChangeImpact struct {
	AffectedServices      []string `json:"affectedServices"`
	Availability          string   `json:"availability"`
	Data                  string   `json:"data"`
	MonthlyCostDeltaCents int64    `json:"monthlyCostDeltaCents"`
}

type Migration struct {
	ID            string `json:"id"`
	Service       string `json:"service"`
	Class         string `json:"class"`
	SQL           string `json:"sql"`
	Digest        string `json:"digest"`
	Reversibility string `json:"reversibility"`
}

type ChangeVerification struct {
	Method         string `json:"method"`
	Path           string `json:"path"`
	ExpectedStatus int    `json:"expectedStatus"`
	BodyContains   string `json:"bodyContains,omitempty"`
}

type Authorization struct {
	Digest       string    `json:"digest"`
	AuthorizedAt time.Time `json:"authorizedAt"`
}

type ChangeOperation struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	Description   string     `json:"description"`
	Reversibility string     `json:"reversibility"`
	Compensation  string     `json:"compensation,omitempty"`
	Phase         string     `json:"phase"`
	Attempts      int        `json:"attempts"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	CompletedAt   *time.Time `json:"completedAt,omitempty"`
	Failure       string     `json:"failure,omitempty"`
}

type ChangeEvidence struct {
	OperationID string    `json:"operationId"`
	Kind        string    `json:"kind"`
	Statement   string    `json:"statement"`
	ObservedAt  time.Time `json:"observedAt"`
}

type DraftChangeInput struct {
	Summary       string              `json:"summary"`
	Release       PublishReleaseInput `json:"release"`
	MigrationPath string              `json:"migrationPath,omitempty"`
	MigrationID   string              `json:"migrationId,omitempty"`
	Database      string              `json:"database,omitempty"`
	Verification  ChangeVerification  `json:"verification"`
}

var environmentName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)

func (c *Client) DraftChange(ctx context.Context, system System, input DraftChangeInput) (Change, error) {
	if err := system.Validate(); err != nil {
		return Change{}, err
	}
	if strings.TrimSpace(input.Summary) == "" {
		return Change{}, fmt.Errorf("change summary is required")
	}
	for key := range input.Release.Environment {
		if !environmentName.MatchString(key) {
			return Change{}, fmt.Errorf("invalid environment name %q", key)
		}
	}
	observed, err := c.ReleaseStatus(ctx, system)
	if err != nil {
		return Change{}, fmt.Errorf("read starting release: %w", err)
	}
	if observed.Phase != "running" || !observed.Healthy || observed.RunningVersion == "" {
		return Change{}, fmt.Errorf("change requires a healthy running base release")
	}
	release, err := c.StageRelease(ctx, system, input.Release)
	if err != nil {
		return Change{}, err
	}
	verification := input.Verification
	if verification.Method == "" {
		verification.Method = http.MethodGet
	}
	if verification.ExpectedStatus == 0 {
		verification.ExpectedStatus = http.StatusOK
	}
	if verification.Method != http.MethodGet || !strings.HasPrefix(verification.Path, "/") || verification.ExpectedStatus < 100 || verification.ExpectedStatus > 599 {
		return Change{}, fmt.Errorf("v0 verification requires GET, an absolute path, and a valid expected status")
	}
	plan := ChangePlan{BaseVersion: observed.RunningVersion, Release: release, Verification: verification, Impact: ChangeImpact{AffectedServices: []string{"web"}, Availability: "rolling replacement; no downtime expected", Data: "none", MonthlyCostDeltaCents: 0}}
	operations := []ChangeOperation{{ID: "01-precondition", Kind: "state.assert", Description: "assert the approved base release is still healthy", Reversibility: "read-only", Phase: "pending"}}
	if input.MigrationPath != "" {
		migrationSQL, err := os.ReadFile(input.MigrationPath)
		if err != nil {
			return Change{}, err
		}
		if !safeName.MatchString(input.MigrationID) || !safeName.MatchString(input.Database) {
			return Change{}, fmt.Errorf("migration requires safe migration and database service names")
		}
		if err := validateExpandMigration(string(migrationSQL)); err != nil {
			return Change{}, err
		}
		sum := sha256.Sum256(migrationSQL)
		plan.Migration = &Migration{ID: input.MigrationID, Service: input.Database, Class: "expand-only", SQL: string(migrationSQL), Digest: hex.EncodeToString(sum[:]), Reversibility: "retained-on-rollback"}
		plan.Impact.AffectedServices = append([]string{input.Database}, plan.Impact.AffectedServices...)
		plan.Impact.Data = "backward-compatible schema expansion retained on release rollback"
		operations = append(operations, ChangeOperation{ID: "02-migration", Kind: "database.expand-migration", Description: "apply backward-compatible database expansion " + input.MigrationID, Reversibility: "irreversible-safe-residue", Compensation: "retain the compatible schema expansion and report it", Phase: "pending"})
	}
	operations = append(operations,
		ChangeOperation{ID: "03-release", Kind: "release.set-desired", Description: "set desired release to " + release.Version, Reversibility: "compensatable", Compensation: "restore desired release " + observed.RunningVersion, Phase: "pending"},
		ChangeOperation{ID: "04-health", Kind: "release.wait-healthy", Description: "wait for the proposed release to become healthy", Reversibility: "read-only", Phase: "pending"},
		ChangeOperation{ID: "05-verify", Kind: "http.verify", Description: "verify the approved application outcome", Reversibility: "read-only", Phase: "pending"},
	)
	digest, err := digestChange(plan, operations)
	if err != nil {
		return Change{}, err
	}
	now := time.Now().UTC()
	change := Change{SchemaVersion: "v1", ID: "change-" + newID(), System: system.Metadata.Name, Summary: strings.TrimSpace(input.Summary), Phase: "drafted", Digest: digest, Plan: plan, Operations: operations, CreatedAt: now, UpdatedAt: now}
	if err := c.saveChange(ctx, system, &change); err != nil {
		return Change{}, err
	}
	return change, nil
}

func (c *Client) InspectChange(ctx context.Context, system System, id string) (Change, error) {
	if !safeName.MatchString(id) {
		return Change{}, fmt.Errorf("invalid change id")
	}
	var change Change
	if err := c.m1.Get(ctx, changeKey(system, id), &change); err != nil {
		return Change{}, err
	}
	if change.SchemaVersion != "v1" || change.System != system.Metadata.Name {
		return Change{}, fmt.Errorf("change does not belong to system")
	}
	return change, nil
}

func (c *Client) AuthorizeChange(ctx context.Context, system System, id, digest string) (Change, error) {
	change, err := c.InspectChange(ctx, system, id)
	if err != nil {
		return Change{}, err
	}
	if change.Phase != "drafted" {
		return Change{}, fmt.Errorf("change in phase %s cannot be authorized", change.Phase)
	}
	actual, err := digestChange(change.Plan, change.Operations)
	if err != nil {
		return Change{}, err
	}
	if digest == "" || digest != change.Digest || digest != actual {
		return Change{}, fmt.Errorf("authorization digest does not match the immutable change plan")
	}
	now := time.Now().UTC()
	change.Authorization = &Authorization{Digest: digest, AuthorizedAt: now}
	change.Phase = "authorized"
	change.UpdatedAt = now
	if err := c.saveChange(ctx, system, &change); err != nil {
		return Change{}, err
	}
	return change, nil
}

func (c *Client) ApplyChange(ctx context.Context, system System, id string) (Change, error) {
	change, err := c.InspectChange(ctx, system, id)
	if err != nil {
		return Change{}, err
	}
	if change.Phase == "committed" || change.Phase == "rejected" || change.Phase == "reverted" || change.Phase == "escalated" {
		return change, nil
	}
	if change.Phase != "authorized" && change.Phase != "applying" && change.Phase != "verifying" && change.Phase != "compensating" {
		return change, fmt.Errorf("change in phase %s cannot be applied", change.Phase)
	}
	actual, err := digestChange(change.Plan, change.Operations)
	if err != nil || change.Authorization == nil || change.Authorization.Digest != change.Digest || actual != change.Digest {
		return change, fmt.Errorf("authorized change digest is no longer valid")
	}
	lease, err := c.acquireChangeLease(ctx, system, change.ID)
	if err != nil {
		return change, err
	}
	defer lease.Close()
	if change.Phase == "compensating" {
		if err := c.compensateChange(ctx, system, &change, lease); err != nil {
			return change, err
		}
		return change, nil
	}
	for index := range change.Operations {
		operation := &change.Operations[index]
		if operation.Phase == "completed" {
			continue
		}
		if err := lease.Assert(ctx); err != nil {
			return change, err
		}
		now := time.Now().UTC()
		change.Phase = "applying"
		if operation.Kind == "http.verify" {
			change.Phase = "verifying"
		}
		operation.Phase = "running"
		operation.Attempts++
		operation.StartedAt = &now
		operation.Failure = ""
		if err := c.saveChange(ctx, system, &change); err != nil {
			return change, err
		}
		statement, executeErr := c.executeChangeOperation(ctx, system, &change, operation, lease)
		if executeErr != nil {
			operation.Phase = "failed"
			operation.Failure = executeErr.Error()
			change.Failure = fmt.Sprintf("%s: %v", operation.ID, executeErr)
			_ = c.saveChange(ctx, system, &change)
			recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), time.Minute)
			compensationErr := c.compensateChange(recoveryCtx, system, &change, lease)
			recoveryCancel()
			if compensationErr != nil {
				return change, fmt.Errorf("change failed: %v; compensation failed: %w", executeErr, compensationErr)
			}
			return change, fmt.Errorf("change %s after failure: %w", change.Phase, executeErr)
		}
		completed := time.Now().UTC()
		operation.Phase = "completed"
		operation.CompletedAt = &completed
		change.Evidence = append(change.Evidence, ChangeEvidence{OperationID: operation.ID, Kind: operation.Kind, Statement: statement, ObservedAt: completed})
		if err := c.saveChange(ctx, system, &change); err != nil {
			return change, err
		}
	}
	now := time.Now().UTC()
	change.Phase = "committed"
	change.CompletedAt = &now
	change.UpdatedAt = now
	change.Failure = ""
	if err := c.saveChange(ctx, system, &change); err != nil {
		return change, err
	}
	return change, nil
}

func (c *Client) executeChangeOperation(ctx context.Context, system System, change *Change, operation *ChangeOperation, lease *changeLeaseGuard) (string, error) {
	switch operation.Kind {
	case "state.assert":
		observed, err := c.ReleaseStatus(ctx, system)
		if err != nil {
			return "", err
		}
		if observed.Phase != "running" || !observed.Healthy || observed.RunningVersion != change.Plan.BaseVersion {
			return "", fmt.Errorf("production moved after planning: expected healthy %s, observed phase=%s version=%s healthy=%t", change.Plan.BaseVersion, observed.Phase, observed.RunningVersion, observed.Healthy)
		}
		return "base release remained healthy and unchanged", nil
	case "database.expand-migration":
		if change.Plan.Migration == nil {
			return "", fmt.Errorf("migration operation has no migration plan")
		}
		action := RuntimeAction{SchemaVersion: "v1", ID: change.ID + "-" + operation.ID, System: system.Metadata.Name, Service: change.Plan.Migration.Service, Kind: operation.Kind, Parameters: map[string]string{"migrationId": change.Plan.Migration.ID, "digest": change.Plan.Migration.Digest, "sql": change.Plan.Migration.SQL}, LeaseKey: lease.key, FencingToken: lease.Token(), RequestedAt: time.Now().UTC()}
		if err := c.m1.PutJSON(ctx, runtimeActionRequestKey(system), action); err != nil {
			return "", err
		}
		result, err := c.waitRuntimeAction(ctx, system, action.ID)
		if err != nil {
			return "", err
		}
		statement := result.Message
		if result.Duplicate {
			statement += "; idempotency ledger prevented duplicate execution"
		}
		return statement, nil
	case "release.set-desired":
		if err := lease.Assert(ctx); err != nil {
			return "", err
		}
		if err := c.m1.PutJSON(ctx, desiredKey(system), change.Plan.Release); err != nil {
			return "", err
		}
		return "desired release set to exact artifact " + change.Plan.Release.ArtifactSHA, nil
	case "release.wait-healthy":
		observed, err := c.waitReleaseVersion(ctx, system, change.Plan.Release.Version, 45*time.Second)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("release %s healthy on node %s pid %d", observed.RunningVersion, observed.Node, observed.PID), nil
	case "http.verify":
		state, err := c.SystemHostStatus(ctx, system)
		if err != nil {
			return "", fmt.Errorf("resolve verification endpoint: %w", err)
		}
		if len(state.Resources) != 1 || state.Resources[0].Address == "" {
			return "", fmt.Errorf("verification requires exactly one addressed host")
		}
		verification := change.Plan.Verification
		request, err := http.NewRequestWithContext(ctx, verification.Method, "http://"+net.JoinHostPort(state.Resources[0].Address, fmt.Sprint(change.Plan.Release.PublicPort))+verification.Path, nil)
		if err != nil {
			return "", err
		}
		response, err := (&http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}).Do(request)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			return "", err
		}
		if response.StatusCode != verification.ExpectedStatus || (verification.BodyContains != "" && !strings.Contains(string(body), verification.BodyContains)) {
			return "", fmt.Errorf("verification failed: status=%d body=%q", response.StatusCode, firstN(strings.TrimSpace(string(body)), 512))
		}
		bodyDigest := sha256.Sum256(body)
		return fmt.Sprintf("%s returned %d, matched the approved response contract, and produced body sha256:%s", verification.Path, response.StatusCode, hex.EncodeToString(bodyDigest[:])), nil
	default:
		return "", fmt.Errorf("unsupported change operation %q", operation.Kind)
	}
}

func (c *Client) compensateChange(ctx context.Context, system System, change *Change, lease *changeLeaseGuard) error {
	if !operationCompleted(change.Operations, "database.expand-migration") && !operationReached(change.Operations, "release.set-desired") {
		now := time.Now().UTC()
		change.Phase = "rejected"
		change.CompletedAt = &now
		return c.saveChange(ctx, system, change)
	}
	change.Phase = "compensating"
	_ = c.saveChange(ctx, system, change)
	if change.Plan.Migration != nil && operationCompleted(change.Operations, "database.expand-migration") {
		change.Residuals = appendUnique(change.Residuals, "expand-only migration "+change.Plan.Migration.ID+" remains applied and backward-compatible")
	}
	if operationReached(change.Operations, "release.set-desired") {
		if err := lease.Assert(ctx); err != nil {
			change.Phase = "escalated"
			_ = c.saveChange(ctx, system, change)
			return err
		}
		var base ReleaseManifest
		if err := c.m1.Get(ctx, releaseKey(system, change.Plan.BaseVersion), &base); err != nil {
			change.Phase = "escalated"
			_ = c.saveChange(ctx, system, change)
			return err
		}
		if err := c.m1.PutJSON(ctx, desiredKey(system), base); err != nil {
			change.Phase = "escalated"
			_ = c.saveChange(ctx, system, change)
			return err
		}
		if _, err := c.waitReleaseVersion(ctx, system, base.Version, 45*time.Second); err != nil {
			change.Phase = "escalated"
			_ = c.saveChange(ctx, system, change)
			return err
		}
		change.Evidence = append(change.Evidence, ChangeEvidence{OperationID: "compensation-release", Kind: "release.restore", Statement: "restored healthy base release " + base.Version, ObservedAt: time.Now().UTC()})
	}
	now := time.Now().UTC()
	change.Phase = "reverted"
	change.CompletedAt = &now
	return c.saveChange(ctx, system, change)
}

func (c *Client) waitRuntimeAction(ctx context.Context, system System, id string) (RuntimeActionResult, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		var result RuntimeActionResult
		found, err := c.m1.GetOptional(ctx, runtimeActionResultKey(system, id), &result)
		if err != nil {
			return RuntimeActionResult{}, err
		}
		if found {
			if result.Phase != "completed" {
				return result, fmt.Errorf("runtime action failed: %s", result.Message)
			}
			return result, nil
		}
		select {
		case <-ctx.Done():
			return RuntimeActionResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Client) waitReleaseVersion(ctx context.Context, system System, version string, timeout time.Duration) (ObservedRelease, error) {
	deadline := time.NewTimer(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		observed, err := c.ReleaseStatus(ctx, system)
		if err != nil {
			return ObservedRelease{}, err
		}
		if observed.Phase == "running" && observed.Healthy && observed.RunningVersion == version {
			return observed, nil
		}
		if observed.Phase == "release-failed" && observed.DesiredVersion == version {
			return ObservedRelease{}, fmt.Errorf("candidate release failed: %s", observed.Message)
		}
		select {
		case <-ctx.Done():
			return ObservedRelease{}, ctx.Err()
		case <-deadline.C:
			return ObservedRelease{}, fmt.Errorf("release %s did not become healthy within %s", version, timeout)
		case <-ticker.C:
		}
	}
}

func validateExpandMigration(sql string) error {
	clean := stripSQLComments(sql)
	if strings.TrimSpace(clean) == "" {
		return fmt.Errorf("migration is empty")
	}
	upper := strings.ToUpper(clean)
	for _, forbidden := range []string{" DROP ", "DELETE ", "TRUNCATE ", "UPDATE ", "RENAME ", "ALTER COLUMN", "SET NOT NULL", "CREATE OR REPLACE", "GRANT ", "REVOKE "} {
		if strings.Contains(" "+upper+" ", forbidden) {
			return fmt.Errorf("expand-only migration contains forbidden operation %q", strings.TrimSpace(forbidden))
		}
	}
	for _, raw := range strings.Split(clean, ";") {
		statement := strings.TrimSpace(raw)
		if statement == "" {
			continue
		}
		normalized := strings.Join(strings.Fields(strings.ToUpper(statement)), " ")
		allowed := (strings.HasPrefix(normalized, "ALTER TABLE ") && strings.Contains(normalized, " ADD COLUMN IF NOT EXISTS ")) ||
			strings.HasPrefix(normalized, "CREATE TABLE IF NOT EXISTS ") ||
			strings.HasPrefix(normalized, "CREATE INDEX IF NOT EXISTS ") || strings.HasPrefix(normalized, "CREATE UNIQUE INDEX IF NOT EXISTS ")
		if !allowed {
			return fmt.Errorf("migration statement is outside the expand-only v0 language: %q", firstN(statement, 120))
		}
	}
	return nil
}

func stripSQLComments(sql string) string {
	lines := strings.Split(sql, "\n")
	for index, line := range lines {
		if comment := strings.Index(line, "--"); comment >= 0 {
			lines[index] = line[:comment]
		}
	}
	return strings.Join(lines, "\n")
}

func digestChange(plan ChangePlan, operations []ChangeOperation) (string, error) {
	type authorizedOperation struct {
		ID            string `json:"id"`
		Kind          string `json:"kind"`
		Description   string `json:"description"`
		Reversibility string `json:"reversibility"`
		Compensation  string `json:"compensation,omitempty"`
	}
	program := make([]authorizedOperation, 0, len(operations))
	for _, operation := range operations {
		program = append(program, authorizedOperation{ID: operation.ID, Kind: operation.Kind, Description: operation.Description, Reversibility: operation.Reversibility, Compensation: operation.Compensation})
	}
	payload, err := json.Marshal(struct {
		Plan       ChangePlan            `json:"plan"`
		Operations []authorizedOperation `json:"operations"`
	}{Plan: plan, Operations: program})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Client) saveChange(ctx context.Context, system System, change *Change) error {
	change.UpdatedAt = time.Now().UTC()
	return c.m1.PutJSON(ctx, changeKey(system, change.ID), change)
}

func operationReached(operations []ChangeOperation, kind string) bool {
	for _, operation := range operations {
		if operation.Kind == kind && (operation.Phase == "running" || operation.Phase == "completed" || operation.Phase == "failed") {
			return true
		}
	}
	return false
}

func operationCompleted(operations []ChangeOperation, kind string) bool {
	for _, operation := range operations {
		if operation.Kind == kind && operation.Phase == "completed" {
			return true
		}
	}
	return false
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func firstN(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func changeKey(system System, id string) string {
	return strings.TrimRight(system.Spec.M1.Prefix, "/") + "/changes/" + id + ".json"
}

func changeLeaseKey(system System) string {
	return strings.TrimRight(system.Spec.M1.Prefix, "/") + "/changes/execution.lease.json"
}

func runtimeActionRequestKey(system System) string {
	return strings.TrimRight(system.Spec.M1.Prefix, "/") + "/runtime-actions/request.json"
}

func runtimeActionResultKey(system System, id string) string {
	return strings.TrimRight(system.Spec.M1.Prefix, "/") + "/runtime-actions/results/" + id + ".json"
}

const changeLeaseDuration = 20 * time.Second

type changeLeaseGuard struct {
	store  *m1.Client
	key    string
	mu     sync.Mutex
	lease  ChangeLease
	etag   string
	err    error
	cancel context.CancelFunc
}

func (c *Client) acquireChangeLease(ctx context.Context, system System, changeID string) (*changeLeaseGuard, error) {
	key := changeLeaseKey(system)
	for attempt := 0; attempt < 5; attempt++ {
		var prior ChangeLease
		found, etag, err := c.m1.GetJSONVersion(ctx, key, &prior)
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		lease := ChangeLease{SchemaVersion: "v1", ChangeID: changeID, Holder: newID(), FencingToken: newID(), ExpiresAt: now.Add(changeLeaseDuration)}
		var written bool
		if !found {
			etag, written, err = c.m1.PutJSONIfAbsent(ctx, key, lease)
		} else if !prior.ExpiresAt.After(now) {
			etag, written, err = c.m1.PutJSONIfMatch(ctx, key, etag, lease)
		} else {
			return nil, fmt.Errorf("system is already applying change %s with holder %s until %s", prior.ChangeID, prior.Holder, prior.ExpiresAt.Format(time.RFC3339))
		}
		if err != nil {
			return nil, err
		}
		if !written {
			continue
		}
		heartbeatCtx, cancel := context.WithCancel(context.Background())
		guard := &changeLeaseGuard{store: c.m1, key: key, lease: lease, etag: etag, cancel: cancel}
		go guard.heartbeat(heartbeatCtx)
		return guard, nil
	}
	return nil, fmt.Errorf("change lease was contended repeatedly")
}

func (g *changeLeaseGuard) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.mu.Lock()
			g.lease.ExpiresAt = time.Now().UTC().Add(changeLeaseDuration)
			etag, written, err := g.store.PutJSONIfMatch(ctx, g.key, g.etag, g.lease)
			if err != nil || !written {
				if err == nil {
					err = errors.New("change lease fencing token was superseded")
				}
				g.err = err
				g.mu.Unlock()
				return
			}
			g.etag = etag
			g.mu.Unlock()
		}
	}
}

func (g *changeLeaseGuard) Assert(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	var current ChangeLease
	found, _, err := g.store.GetJSONVersion(ctx, g.key, &current)
	if err != nil {
		return err
	}
	if !found || current.FencingToken != g.lease.FencingToken || !current.ExpiresAt.After(time.Now().UTC()) {
		return fmt.Errorf("change execution lost its lease")
	}
	return nil
}

func (g *changeLeaseGuard) Token() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lease.FencingToken
}

func (g *changeLeaseGuard) Close() {
	g.cancel()
	g.mu.Lock()
	defer g.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g.lease.ExpiresAt = time.Now().UTC()
	if etag, written, err := g.store.PutJSONIfMatch(ctx, g.key, g.etag, g.lease); err == nil && written {
		g.etag = etag
	}
}
