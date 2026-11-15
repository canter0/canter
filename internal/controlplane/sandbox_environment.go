package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

// SandboxRepository is a public HTTPS repository checked out at setup.
type SandboxRepository struct {
	URL string `json:"url"`
	Ref string `json:"ref,omitempty"`
}

// SandboxEnvironment is the immutable creation recipe: repository checkout,
// setup command, nonsecret env defaults, and allowed secret references.
type SandboxEnvironment struct {
	Repository          *SandboxRepository `json:"repository,omitempty"`
	SetupCommand        string             `json:"setupCommand,omitempty"`
	SetupTimeoutSeconds int                `json:"setupTimeoutSeconds,omitempty"`
	Env                 map[string]string  `json:"env,omitempty"`
	SecretRefs          map[string]string  `json:"secretRefs,omitempty"`
}

// SandboxSetup is the durable setup state on a sandbox — fixed safe fields
// only, never raw RPC errors or resolved secret values.
type SandboxSetup struct {
	Status    string     `json:"status,omitempty"` // pending,running,succeeded,failed
	JobID     string     `json:"jobId,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	ExitCode  *int       `json:"exitCode,omitempty"`
	Error     string     `json:"error,omitempty"`
}

var sandboxSecretID = regexp.MustCompile(`^sec_[A-Za-z0-9_-]{1,124}$`)

// normalizeSandboxEnvironment validates and canonicalizes a create-time
// environment recipe. restoring is true when the request carries a
// snapshot/template source — repository checkout and setup are then a
// conflict because restores preserve files and never rerun destructive setup.
func normalizeSandboxEnvironment(input *SandboxEnvironment, restoring bool) (*SandboxEnvironment, error) {
	if input == nil {
		return nil, nil
	}
	out := &SandboxEnvironment{}
	if input.Repository != nil {
		if restoring {
			return nil, fmt.Errorf("%w: restored sandboxes cannot run repository checkout or setup", ErrConflict)
		}
		raw := strings.TrimSpace(input.Repository.URL)
		if raw == "" || len(raw) > 2048 {
			return nil, fmt.Errorf("%w: repository URL is required and must be at most 2048 bytes", ErrConflict)
		}
		if strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return nil, fmt.Errorf("%w: repository URL contains control characters", ErrConflict)
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" ||
			parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery ||
			parsed.Fragment != "" || strings.ContainsRune(raw, '#') {
			return nil, fmt.Errorf("%w: repository must be a public HTTPS URL without credentials, query, or fragment", ErrConflict)
		}
		ref := strings.TrimSpace(input.Repository.Ref)
		if len(ref) > 256 || strings.HasPrefix(ref, "-") || strings.IndexFunc(ref, func(r rune) bool { return r < 0x20 || r == 0x7f || r == ' ' || r == '\t' }) >= 0 {
			return nil, fmt.Errorf("%w: invalid repository ref", ErrConflict)
		}
		out.Repository = &SandboxRepository{URL: raw, Ref: ref}
	}
	if input.SetupCommand != "" {
		if restoring {
			return nil, fmt.Errorf("%w: restored sandboxes cannot run repository checkout or setup", ErrConflict)
		}
		if len(input.SetupCommand) > 32768 || strings.ContainsRune(input.SetupCommand, 0) {
			return nil, fmt.Errorf("%w: setup command must be at most 32KiB without NUL bytes", ErrConflict)
		}
		out.SetupCommand = input.SetupCommand
	}
	if out.Repository != nil || out.SetupCommand != "" {
		timeout := input.SetupTimeoutSeconds
		if timeout == 0 {
			timeout = 1800
		}
		if timeout < 1 || timeout > 3600 {
			return nil, fmt.Errorf("%w: setup timeout must be 1-3600 seconds", ErrConflict)
		}
		out.SetupTimeoutSeconds = timeout
	} else if input.SetupTimeoutSeconds != 0 {
		return nil, fmt.Errorf("%w: setup timeout requires a repository or setup command", ErrConflict)
	}
	if len(input.Env)+len(input.SecretRefs) > 64 {
		return nil, fmt.Errorf("%w: environment and secret references allow at most 64 combined entries", ErrConflict)
	}
	if len(input.Env) > 0 {
		if err := validateSandboxEnv(input.Env); err != nil {
			return nil, err
		}
		out.Env = input.Env
	}
	if len(input.SecretRefs) > 0 {
		for key, id := range input.SecretRefs {
			if !sandboxEnvKey.MatchString(key) {
				return nil, fmt.Errorf("%w: invalid secret environment name %q", ErrConflict, key)
			}
			if !sandboxSecretID.MatchString(id) {
				return nil, fmt.Errorf("%w: invalid secret reference for %q", ErrConflict, key)
			}
			if _, ok := input.Env[key]; ok {
				return nil, fmt.Errorf("%w: environment name %q cannot be both a value and a secret reference", ErrConflict, key)
			}
		}
		out.SecretRefs = input.SecretRefs
	}
	if out.Repository == nil && out.SetupCommand == "" && len(out.Env) == 0 && len(out.SecretRefs) == 0 {
		return nil, nil
	}
	return out, nil
}

// sandboxSetupJobID is the fixed guest job ID for the initial setup of a
// sandbox generation — same-ID retries prove at-most-once execution.
func sandboxSetupJobID(sandboxID string, generation int) string {
	return fmt.Sprintf("job_setup_%s_%d", sandboxID, generation)
}

// sandboxSetupCommand builds the quoted setup command: fixed /workspace/project
// checkout for a repository, then the verbatim setup command. No secrets or
// credentials are ever interpolated into the command text.
func sandboxSetupCommand(environment *SandboxEnvironment) string {
	if environment == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("set -eu\nexport GIT_TERMINAL_PROMPT=0\n")
	if environment.Repository != nil {
		ref := environment.Repository.Ref
		if ref == "" {
			ref = "HEAD"
		}
		b.WriteString("test ! -e /workspace/project\n")
		b.WriteString("git -c protocol.file.allow=never -c protocol.ext.allow=never init /workspace/project\n")
		fmt.Fprintf(&b, "git -C /workspace/project remote add origin %s\n", shellQuote(environment.Repository.URL))
		fmt.Fprintf(&b, "git -C /workspace/project -c protocol.file.allow=never -c protocol.ext.allow=never -c credential.helper= fetch --depth=1 origin %s\n", shellQuote(ref))
		b.WriteString("git -C /workspace/project -c core.hooksPath=/dev/null checkout --detach FETCH_HEAD\n")
		b.WriteString("cd /workspace/project\n")
	} else {
		b.WriteString("cd /workspace\n")
	}
	if environment.SetupCommand != "" {
		b.WriteString(environment.SetupCommand)
		b.WriteString("\n")
	}
	return b.String()
}

// shellQuote wraps a value in single quotes with the '\” escape.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// sandboxEnvJSON serializes environment/setup for the jsonb columns; nil
// environment persists {} rather than null.
func sandboxEnvJSON(environment *SandboxEnvironment) ([]byte, error) {
	if environment == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(environment)
}

func sandboxSetupJSON(setup SandboxSetup) ([]byte, error) {
	return json.Marshal(setup)
}

// validateSandboxSecretRefsTx validates that every referenced secret exists in
// this workspace as a stored, sandbox-allowed, unrevoked secret; each value
// decrypts, contains no NUL, and fits within 8192 bytes. The combined
// environment — nonsecret keys/values plus secret keys and each target's
// plaintext — is bounded by the shared environment size. Unique secret IDs
// are decrypted once in sorted order; plaintext is cleared on every path.
func (h *HTTPServer) validateSandboxSecretRefsTx(ctx context.Context, tx pgx.Tx, workspace string, environment *SandboxEnvironment) error {
	if environment == nil {
		return nil
	}
	total := 0
	for key, value := range environment.Env {
		total += len(key) + len(value)
	}
	for key := range environment.SecretRefs {
		total += len(key)
	}
	if len(environment.SecretRefs) == 0 {
		if total > 32768 {
			return fmt.Errorf("%w: combined environment exceeds the value size limit", ErrConflict)
		}
		return nil
	}
	seen := map[string]int{}
	for _, id := range environment.SecretRefs {
		seen[id] = 0
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		var keyID string
		var ciphertext []byte
		err := tx.QueryRow(ctx, `SELECT key_id,ciphertext FROM workspace_secrets WHERE workspace_id=$1 AND id=$2 AND purpose='stored' AND sandbox_allowed AND revoked_at IS NULL FOR UPDATE`, workspace, id).Scan(&keyID, &ciphertext)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: a selected secret is not available for sandbox use", ErrForbidden)
		}
		if err != nil {
			return err
		}
		plain, err := h.config.Secrets.open(keyID, ciphertext, secretBinding(workspace, id, "stored"))
		if err != nil {
			return fmt.Errorf("%w: a selected secret could not be decrypted", ErrForbidden)
		}
		if len(plain) > 8192 {
			clear(plain)
			return fmt.Errorf("%w: a selected secret exceeds the value size limit", ErrConflict)
		}
		if bytes.IndexByte(plain, 0) >= 0 {
			clear(plain)
			return fmt.Errorf("%w: a selected secret contains an invalid NUL byte", ErrConflict)
		}
		seen[id] = len(plain)
		clear(plain)
	}
	for _, id := range environment.SecretRefs {
		total += seen[id]
	}
	if total > 32768 {
		return fmt.Errorf("%w: combined environment exceeds the value size limit", ErrConflict)
	}
	return nil
}

func sandboxSetupPending(setup SandboxSetup) bool {
	return setup.Status == "pending" || setup.Status == "running"
}

func sandboxJobTerminal(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "timed_out", "interrupted":
		return true
	}
	return false
}

// sandboxJobEnvironment merges the sandbox recipe environment for one job:
// saved nonsecret env, the chosen secret references (nil input refs inherit,
// {} clears, nonempty replaces), then explicit job env — which may not
// collide with a chosen secret key. Chosen refs are resolved freshly.
func (h *HTTPServer) sandboxJobEnvironment(ctx context.Context, v Sandbox, input SandboxExecInput, actor sdk.ActorRef) (map[string]string, error) {
	environment := v.Environment
	var refs map[string]string
	switch {
	case input.SecretRefs == nil:
		if environment != nil {
			refs = environment.SecretRefs
		}
	case len(input.SecretRefs) == 0:
		refs = nil
	default:
		if len(input.SecretRefs) > 64 {
			return nil, fmt.Errorf("%w: at most 64 secret references", ErrConflict)
		}
		for key, id := range input.SecretRefs {
			if !sandboxEnvKey.MatchString(key) || !sandboxSecretID.MatchString(id) {
				return nil, fmt.Errorf("%w: invalid secret reference for %q", ErrConflict, key)
			}
		}
		refs = input.SecretRefs
	}
	env := map[string]string{}
	if environment != nil {
		for key, value := range environment.Env {
			env[key] = value
		}
	}
	counted := map[string]struct{}{}
	for key := range env {
		counted[key] = struct{}{}
	}
	for key := range refs {
		counted[key] = struct{}{}
	}
	for key := range input.Env {
		counted[key] = struct{}{}
	}
	if len(counted) > 64 {
		return nil, fmt.Errorf("%w: at most 64 environment entries", ErrConflict)
	}
	if len(refs) > 0 {
		for key := range input.Env {
			if _, chosen := refs[key]; chosen {
				return nil, fmt.Errorf("%w: environment name %q cannot be both a job value and a secret reference", ErrConflict, key)
			}
		}
		resolved, err := h.resolveSandboxSecretRefs(ctx, v.WorkspaceID, refs, actor)
		if err != nil {
			return nil, err
		}
		defer func() {
			for k := range resolved {
				delete(resolved, k)
			}
		}()
		for key, value := range resolved {
			env[key] = value
		}
	}
	for key, value := range input.Env {
		env[key] = value
	}
	if err := validateSandboxEnv(env); err != nil {
		clear(env)
		return nil, err
	}
	return env, nil
}

// resolveSandboxSecretRefs resolves chosen secret refs under one transaction
// in sorted ID order: decrypt, audit secret.used, update last_used_at.
// Resolved values are returned for job injection only and must be cleared by
// the caller after dispatch; they never enter responses or error chains.
func (h *HTTPServer) resolveSandboxSecretRefs(ctx context.Context, workspace string, refs map[string]string, actor sdk.ActorRef) (map[string]string, error) {
	resolved := map[string]string{}
	fail := func(err error) (map[string]string, error) {
		for k := range resolved {
			delete(resolved, k)
		}
		return nil, err
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback(ctx)
	unique := map[string]string{}
	// Match grant/rotation lock order; audit insertion also needs the parent
	// workspace's FK lock, so acquiring it after secret rows can deadlock.
	var lockedWorkspace string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR KEY SHARE`, workspace).Scan(&lockedWorkspace); err != nil {
		return fail(err)
	}
	for key, id := range refs {
		unique[id] = key
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	values := map[string][]byte{}
	defer func() {
		for _, plain := range values {
			clear(plain)
		}
	}()
	for _, id := range ids {
		var keyID string
		var ciphertext []byte
		err := tx.QueryRow(ctx, `SELECT key_id,ciphertext FROM workspace_secrets WHERE workspace_id=$1 AND id=$2 AND purpose='stored' AND sandbox_allowed AND revoked_at IS NULL FOR UPDATE`, workspace, id).Scan(&keyID, &ciphertext)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail(fmt.Errorf("%w: a selected secret is not available for sandbox use", ErrForbidden))
		}
		if err != nil {
			return fail(err)
		}
		plain, err := h.config.Secrets.open(keyID, ciphertext, secretBinding(workspace, id, "stored"))
		if err != nil {
			return fail(fmt.Errorf("%w: a selected secret could not be decrypted", ErrForbidden))
		}
		if len(plain) > 8192 || bytes.IndexByte(plain, 0) >= 0 {
			clear(plain)
			return fail(fmt.Errorf("%w: a selected secret contains invalid content", ErrConflict))
		}
		values[id] = plain
	}
	for key, id := range refs {
		resolved[key] = string(values[id])
	}
	for _, plain := range values {
		clear(plain)
	}
	for _, id := range ids {
		if err = secretAudit(ctx, tx, workspace, actor, "secret.used", id); err != nil {
			return fail(err)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE workspace_secrets SET last_used_at=now() WHERE workspace_id=$1 AND id=ANY($2)`, workspace, ids); err != nil {
		return fail(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fail(err)
	}
	return resolved, nil
}

const setupInterruptMessage = "Setup was interrupted before it finished; the sandbox is retained for inspection."

// updateSandboxSetup writes the durable setup state under the sandbox row,
// guarded by generation, phase ready, and no sleep/delete request so a
// sleep/delete race cannot resurrect or publish a stale outcome.
func (d *SandboxDispatcher) updateSandboxSetup(ctx context.Context, v *Sandbox, setup SandboxSetup) error {
	raw, err := sandboxSetupJSON(setup)
	if err != nil {
		return err
	}
	tx, err := d.Server.service.Store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx,
		`UPDATE sandboxes SET setup=$3,next_attempt_at=now()+interval '3 seconds',updated_at=now()
WHERE workspace_id=$1 AND id=$2 AND generation=$4 AND phase='ready'
AND NOT sleep_requested AND NOT delete_requested
AND COALESCE(setup->>'status','')=$5 AND COALESCE(setup->>'jobId','')=$6`,
		v.WorkspaceID, v.ID, raw, v.Generation, v.Setup.Status, v.Setup.JobID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("%w: sandbox changed during setup reconcile", ErrConflict)
	}
	// The launched setup job is recorded in the same transaction so its
	// stop/log endpoints resolve before any RPC acknowledgement.
	if setup.JobID != "" {
		if _, err := tx.Exec(ctx,
			`INSERT INTO sandbox_jobs(sandbox_id,job_id,started_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`,
			v.ID, setup.JobID, d.Server.service.Store.now()); err != nil {
			return err
		}
		if setup.Status == "succeeded" || setup.Status == "failed" {
			if _, err := tx.Exec(ctx, `UPDATE sandbox_jobs SET finished_at=now() WHERE sandbox_id=$1 AND job_id=$2 AND finished_at IS NULL`, v.ID, setup.JobID); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	v.Setup = setup
	return nil
}

// reconcileSandboxSetup drives the durable setup state machine: launch the
// fixed job once, poll its outcome, apply the tombstone on timeout, and only
// record confirmed terminal outcomes. Uses the raw trusted client — setup is
// an internal operation, never a user mutation.
func (d *SandboxDispatcher) reconcileSandboxSetup(ctx context.Context, v *Sandbox) error {
	if v.Environment == nil || (v.Environment.Repository == nil && v.Environment.SetupCommand == "") {
		if v.Setup.Status == "pending" {
			return d.updateSandboxSetup(ctx, v, SandboxSetup{Status: "failed", Error: "Setup recipe is unavailable."})
		}
		return ErrConflict
	}
	operation, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	client, err := d.sandboxDirectClient(*v)
	if err != nil {
		return errors.New("sandbox setup connection could not be established")
	}
	defer client.Close()
	if v.Setup.Status == "pending" {
		capabilities, err := client.Probe(operation)
		if err != nil {
			return nil
		}
		if capabilities.WorkflowProtocol < 1 {
			return d.updateSandboxSetup(ctx, v, SandboxSetup{Status: "failed", Error: "Sandbox tools do not support setup."})
		}
		now := d.Server.service.Store.now()
		if err := d.updateSandboxSetup(ctx, v, SandboxSetup{Status: "running", JobID: sandboxSetupJobID(v.ID, v.Generation), StartedAt: &now}); err != nil {
			return err
		}
	}
	if v.Setup.Status != "running" {
		return nil
	}
	if v.Setup.JobID != sandboxSetupJobID(v.ID, v.Generation) {
		return ErrConflict
	}
	live, err := d.setupStillLive(operation, *v)
	if err != nil || !live {
		return firstErr(err, ErrConflict)
	}
	expired := v.Setup.StartedAt == nil || d.Server.service.Store.now().Sub(*v.Setup.StartedAt) >= time.Duration(v.Environment.SetupTimeoutSeconds)*time.Second+90*time.Second
	if expired {
		job, err := client.StopJob(operation, v.Setup.JobID)
		if err != nil {
			return nil
		}
		return d.finishSandboxSetup(ctx, v, job, setupInterruptMessage)
	}
	job, lookupErr := client.Job(operation, v.Setup.JobID)
	if lookupErr == nil && job.ID == v.Setup.JobID && sandboxJobTerminal(job.Status) {
		return d.finishSandboxSetup(ctx, v, job, "")
	}
	if !sandboxJobNotFound(lookupErr) {
		return nil
	}
	env, envErr := d.Server.sandboxJobEnvironment(operation, *v, SandboxExecInput{}, sdk.ActorRef{Kind: "system", ID: "sandbox-setup"})
	if envErr != nil {
		// A prior uncertain StartJob may arrive late. Seal its ID before
		// releasing the setup guard, even if Job currently says not_found.
		job, err := client.StopJob(operation, v.Setup.JobID)
		if err != nil {
			return nil
		}
		return d.finishSandboxSetup(ctx, v, job, "Setup environment could not be resolved.")
	}
	defer clear(env)
	live, err = d.setupStillLive(operation, *v)
	if err != nil || !live {
		return firstErr(err, ErrConflict)
	}
	job, err = client.StartJob(operation, wrencomputer.JobRequest{
		ID: v.Setup.JobID, Command: sandboxSetupCommand(v.Environment), Directory: "/workspace",
		Env: env, TimeoutSeconds: v.Environment.SetupTimeoutSeconds, Sandbox: true,
	})
	if err != nil {
		return nil
	} // query the same ID next time; never expose raw errors
	return d.finishSandboxSetup(ctx, v, job, "")
}

func sandboxJobNotFound(err error) bool {
	var remote *wrencomputer.Error
	return errors.Is(err, ErrNotFound) || (errors.As(err, &remote) && remote.Code == "not_found")
}

func (d *SandboxDispatcher) setupStillLive(ctx context.Context, v Sandbox) (bool, error) {
	var live bool
	err := d.Server.service.Store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandboxes
WHERE workspace_id=$1 AND id=$2 AND generation=$3 AND phase='ready'
AND NOT sleep_requested AND NOT delete_requested AND setup->>'status'='running' AND setup->>'jobId'=$4)`,
		v.WorkspaceID, v.ID, v.Generation, v.Setup.JobID).Scan(&live)
	return live, err
}

func (d *SandboxDispatcher) finishSandboxSetup(ctx context.Context, v *Sandbox, job wrencomputer.Job, message string) error {
	if job.ID != v.Setup.JobID || !sandboxJobTerminal(job.Status) {
		return nil
	}
	setup := SandboxSetup{Status: "failed", JobID: v.Setup.JobID, StartedAt: v.Setup.StartedAt, ExitCode: job.ExitCode, Error: message}
	if job.Status == "completed" && job.ExitCode != nil && *job.ExitCode == 0 {
		setup.Status, setup.Error = "succeeded", ""
	} else if setup.Error == "" {
		setup.Error = "Setup did not complete successfully."
	}
	return d.updateSandboxSetup(ctx, v, setup)
}

// stopSandboxSetup is the narrow trusted exception for cancelling the CURRENT
// active setup job only — reloads under the sandbox row lock, verifies
// generation/phase/flags and the matching active setup job, then performs a
// bounded raw StopJob. Ordinary writer authorization runs before this.
func (h *HTTPServer) stopSandboxSetup(ctx context.Context, v Sandbox, jobID string, actor sdk.ActorRef) (wrencomputer.Job, error) {
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := h.service.Store.pool.Begin(operation)
	if err != nil {
		return wrencomputer.Job{}, err
	}
	defer tx.Rollback(context.WithoutCancel(operation))
	fresh, err := scanSandbox(tx.QueryRow(operation, `SELECT `+sandboxColumns+` FROM sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, v.WorkspaceID, v.ID))
	if err != nil {
		return wrencomputer.Job{}, err
	}
	if fresh.Generation != v.Generation || fresh.Phase != "ready" || fresh.SleepRequested || fresh.DeleteRequested ||
		fresh.Setup.Status != "running" || fresh.Setup.JobID != jobID || jobID != sandboxSetupJobID(fresh.ID, fresh.Generation) {
		return wrencomputer.Job{}, ErrConflict
	}
	if err := tx.Commit(operation); err != nil {
		return wrencomputer.Job{}, err
	}
	d := &SandboxDispatcher{Server: h}
	client, err := d.sandboxDirectClient(fresh)
	if err != nil {
		return wrencomputer.Job{}, errors.New("setup stop connection could not be established")
	}
	defer client.Close()
	job, err := client.StopJob(operation, jobID)
	if err != nil {
		return wrencomputer.Job{}, fmt.Errorf("setup stop could not be confirmed; inspect job %s before retrying", jobID)
	}
	if job.ID != jobID || (!sandboxJobTerminal(job.Status) && job.Status != "starting" && job.Status != "running") {
		return wrencomputer.Job{}, errors.New("setup stop returned an invalid receipt")
	}
	if err := d.finishSandboxSetup(ctx, &fresh, job, setupInterruptMessage); err != nil {
		return wrencomputer.Job{}, err
	}
	_ = h.service.Store.Audit(ctx, v.WorkspaceID, actor, "sandbox.job_stopped", v.ID, map[string]any{"jobId": jobID})
	return job, nil
}
