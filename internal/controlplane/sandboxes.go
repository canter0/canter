package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/pricing"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
)

type SandboxConfig struct {
	Enabled           bool
	SSHSourceCIDR     string
	FreeMax           int
	PaidMax           int
	FreeHoursPerMonth int
	IdleSleep         time.Duration
	PreviewDomain     string
	// ImageID pins sandboxes to a prebuilt provider image (CANTER_SANDBOX_IMAGE).
	ImageID string
}

// imageVersion is the build stamp baked into the pinned sandbox image.
func (c SandboxConfig) imageVersion() string {
	if c.ImageID == "" {
		return ""
	}
	return "1"
}

func (c SandboxConfig) freeMax() int {
	if c.FreeMax < 1 {
		return 5
	}
	return c.FreeMax
}
func (c SandboxConfig) paidMax() int {
	if c.PaidMax < 1 {
		return 20
	}
	return c.PaidMax
}
func (c SandboxConfig) freeSecondsPerMonth() int64 {
	if c.FreeHoursPerMonth < 1 {
		return 50 * 3600
	}
	return int64(c.FreeHoursPerMonth) * 3600
}
func (c SandboxConfig) idleSleep() time.Duration {
	if c.IdleSleep <= 0 {
		return 15 * time.Minute
	}
	return c.IdleSleep
}

type SandboxCreateInput struct {
	Name               string              `json:"name"`
	Size               string              `json:"size"`
	Labels             map[string]string   `json:"labels,omitempty"`
	IdleTimeoutSeconds *int                `json:"idleTimeoutSeconds,omitempty"`
	IdempotencyKey     string              `json:"idempotencyKey,omitempty"`
	SnapshotID         string              `json:"snapshotId,omitempty"`
	TemplateID         string              `json:"templateId,omitempty"`
	TemplateVersion    int                 `json:"templateVersion,omitempty"`
	Desktop            bool                `json:"desktop,omitempty"`
	Environment        *SandboxEnvironment `json:"environment,omitempty"`
}

var sandboxLabelKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,62}$`)

func validateSandboxLabels(labels map[string]string) error {
	if len(labels) > 16 {
		return fmt.Errorf("%w: at most 16 labels", ErrConflict)
	}
	for key, value := range labels {
		if !sandboxLabelKey.MatchString(key) {
			return fmt.Errorf("%w: invalid label key %q", ErrConflict, key)
		}
		if len(value) > 128 || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("%w: invalid label value for %q", ErrConflict, key)
		}
	}
	return nil
}

// effectiveIdle resolves the supplied idle setting; -1 means inherit the
// server default and is equivalent to omitting the field entirely.
func effectiveIdle(input *int) (int, error) {
	if input == nil {
		return -1, nil
	}
	v := *input
	if v != -1 && v != 0 && (v < 60 || v > 86400) {
		return 0, fmt.Errorf("%w: idleTimeoutSeconds must be -1, 0, or 60-86400", ErrConflict)
	}
	return v, nil
}

// sandboxRequestHash is the canonical request fingerprint for idempotent
// creation: trimmed name/size, normalized labels, effective idle (-1==omitted).
func sandboxRequestHash(input SandboxCreateInput, idle int) string {
	normalized := make([][2]string, 0, len(input.Labels))
	for key, value := range input.Labels {
		normalized = append(normalized, [2]string{key, value})
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i][0] < normalized[j][0] })
	canonical, _ := json.Marshal(struct {
		Name        string              `json:"n"`
		Size        string              `json:"s"`
		Labels      [][2]string         `json:"l"`
		Idle        int                 `json:"i"`
		Snapshot    string              `json:"snapshot,omitempty"`
		Template    string              `json:"template,omitempty"`
		Version     int                 `json:"version,omitempty"`
		Desktop     bool                `json:"desktop,omitempty"`
		Environment *SandboxEnvironment `json:"environment,omitempty"`
	}{strings.TrimSpace(input.Name), input.Size, normalized, idle, input.SnapshotID, input.TemplateID, input.TemplateVersion, input.Desktop, input.Environment})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

type sandboxSize struct {
	VCPU   int
	Memory int
	Disk   int
	Free   bool
}

var sandboxSizes = map[string]sandboxSize{
	"free-1x1": {VCPU: 1, Memory: 1024, Disk: 10, Free: true},
	"s-1x2":    {VCPU: 1, Memory: 2048, Disk: 20},
	"s-2x4":    {VCPU: 2, Memory: 4096, Disk: 40},
	"s-2x8":    {VCPU: 2, Memory: 8192, Disk: 80},
	"s-4x16":   {VCPU: 4, Memory: 16384, Disk: 160},
}

var sandboxSizeOrder = []string{"free-1x1", "s-1x2", "s-2x4", "s-2x8", "s-4x16"}

// sandboxSizeList describes this implementation's sizes in the list response.
func sandboxSizeList() []map[string]any {
	out := make([]map[string]any, 0, len(sandboxSizeOrder))
	for _, id := range sandboxSizeOrder {
		size := sandboxSizes[id]
		monthly, _ := pricing.EstimateComputeMonthlyCents(size.VCPU, size.Memory)
		out = append(out, map[string]any{
			"id": id, "vcpus": size.VCPU, "memoryMiB": size.Memory,
			"diskGiB": size.Disk, "free": size.Free,
			"monthlyCents": monthly,
		})
	}
	return out
}

type Sandbox struct {
	ID                 string                       `json:"id"`
	WorkspaceID        string                       `json:"workspaceId"`
	Name               string                       `json:"name"`
	Size               string                       `json:"size"`
	Tier               string                       `json:"tier"`
	Phase              string                       `json:"phase"`
	VCPUs              int                          `json:"vcpus"`
	MemoryMiB          int                          `json:"memoryMiB"`
	DiskGiB            int                          `json:"diskGiB"`
	MonthlyCents       int64                        `json:"monthlyCents"`
	Failure            string                       `json:"failure,omitempty"`
	DeleteRequested    bool                         `json:"deleteRequested"`
	SleepRequested     bool                         `json:"-"`
	CreatedByKind      string                       `json:"createdByKind"`
	CreatedByID        string                       `json:"createdById,omitempty"`
	CreatedByName      string                       `json:"createdByName,omitempty"`
	LastUsedAt         *time.Time                   `json:"lastUsedAt,omitempty"`
	AwakeSince         *time.Time                   `json:"awakeSince,omitempty"`
	HasCheckpoint      bool                         `json:"hasCheckpoint"`
	CreatedAt          time.Time                    `json:"createdAt"`
	UpdatedAt          time.Time                    `json:"updatedAt"`
	Plan               sdk.VPSPlan                  `json:"-"`
	Generation         int                          `json:"generation"`
	SSHPublicKey       string                       `json:"-"`
	SleepForcedAt      *time.Time                   `json:"-"`
	Notice             string                       `json:"notice,omitempty"`
	KeyVersion         string                       `json:"-"`
	KeyCiphertext      []byte                       `json:"-"`
	Address            string                       `json:"-"`
	HostFingerprint    string                       `json:"-"`
	CheckpointObject   sdk.ComputerCheckpointObject `json:"-"`
	CheckpointKey      string                       `json:"-"`
	CheckpointNonce    []byte                       `json:"-"`
	CheckpointManifest []byte                       `json:"-"`
	Labels             map[string]string            `json:"labels"`
	IdleTimeoutSeconds int                          `json:"idleTimeoutSeconds"`
	CurrentSnapshotID  string                       `json:"snapshotId,omitempty"`
	PendingSnapshotID  string                       `json:"-"`
	Desktop            bool                         `json:"desktop"`
	Environment        *SandboxEnvironment          `json:"environment,omitempty"`
	Setup              SandboxSetup                 `json:"setup"`
	observedPhase      string
}

const sandboxColumns = `id,workspace_id,name,size,tier,phase,plan,generation,ssh_public_key,ssh_key_version,ssh_key_ciphertext,COALESCE(host(address),''),COALESCE(host_fingerprint,''),monthly_cents,COALESCE(failure,''),delete_requested,sleep_requested,created_by_kind,COALESCE(created_by_id,''),COALESCE(created_by_name,''),last_used_at,awake_since,COALESCE(notice,''),sleep_forced_at,checkpoint_object_ref,checkpoint_key_version,checkpoint_nonce,checkpoint_manifest,created_at,updated_at,next_attempt_at,COALESCE(labels::text,'{}'),idle_timeout_seconds,COALESCE(current_snapshot_id,''),COALESCE(pending_snapshot_id,''),desktop,COALESCE(environment::text,'{}'),COALESCE(setup::text,'{}')`

func scanSandbox(row pgx.Row) (Sandbox, error) {
	var v Sandbox
	var plan, object, labels, environmentJSON, setupJSON []byte
	var next *time.Time
	err := row.Scan(&v.ID, &v.WorkspaceID, &v.Name, &v.Size, &v.Tier, &v.Phase, &plan, &v.Generation, &v.SSHPublicKey, &v.KeyVersion, &v.KeyCiphertext, &v.Address, &v.HostFingerprint, &v.MonthlyCents, &v.Failure, &v.DeleteRequested, &v.SleepRequested, &v.CreatedByKind, &v.CreatedByID, &v.CreatedByName, &v.LastUsedAt, &v.AwakeSince, &v.Notice, &v.SleepForcedAt, &object, &v.CheckpointKey, &v.CheckpointNonce, &v.CheckpointManifest, &v.CreatedAt, &v.UpdatedAt, &next, &labels, &v.IdleTimeoutSeconds, &v.CurrentSnapshotID, &v.PendingSnapshotID, &v.Desktop, &environmentJSON, &setupJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	v.observedPhase = v.Phase
	if len(plan) > 0 {
		if err = json.Unmarshal(plan, &v.Plan); err != nil {
			return v, err
		}
	}
	if err = json.Unmarshal(labels, &v.Labels); err != nil {
		return v, err
	}
	if len(environmentJSON) > 2 {
		var environment SandboxEnvironment
		if err = json.Unmarshal(environmentJSON, &environment); err != nil {
			return v, err
		}
		v.Environment = &environment
	}
	if len(setupJSON) > 0 {
		if err = json.Unmarshal(setupJSON, &v.Setup); err != nil {
			return v, err
		}
	}
	if len(object) > 0 {
		if err = json.Unmarshal(object, &v.CheckpointObject); err != nil {
			return v, err
		}
	}
	v.HasCheckpoint = v.CheckpointObject.Key != "" || v.CurrentSnapshotID != ""
	v.VCPUs, v.MemoryMiB, v.DiskGiB = v.Plan.Shape.VCPU, v.Plan.Shape.Memory, v.Plan.Shape.GB
	return v, nil
}

func (s *Store) sandbox(ctx context.Context, q artifactQueryer, workspace, id string) (Sandbox, error) {
	return scanSandbox(q.QueryRow(ctx, `SELECT `+sandboxColumns+` FROM sandboxes WHERE workspace_id=$1 AND id=$2`, workspace, id))
}

func (s *Store) Sandboxes(ctx context.Context, workspace string) ([]Sandbox, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+sandboxColumns+` FROM sandboxes WHERE workspace_id=$1 AND phase<>'deleted' ORDER BY created_at`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sandbox{}
	for rows.Next() {
		v, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) Sandbox(ctx context.Context, workspace, id string) (Sandbox, error) {
	return s.sandbox(ctx, s.pool, workspace, id)
}

func sandboxMonth(now time.Time) string {
	return now.UTC().Format("2006-01") + "-01"
}

// SandboxFreeUsage returns accrued free-tier awake seconds for the current UTC
// month, including not-yet-flushed time on running free sandboxes.
func (s *Store) SandboxFreeUsage(ctx context.Context, q artifactQueryer, workspace string, now time.Time) (int64, error) {
	var stored int64
	if err := q.QueryRow(ctx, `SELECT COALESCE(awake_seconds,0) FROM sandbox_free_usage WHERE workspace_id=$1 AND month=$2`, workspace, sandboxMonth(now)).Scan(&stored); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	var live int64
	utc := now.UTC()
	monthStart := time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
	if err := q.QueryRow(ctx, `SELECT COALESCE(sum(GREATEST(0,EXTRACT(EPOCH FROM $2-GREATEST(awake_since,$3::timestamptz)))),0)::bigint FROM sandboxes WHERE workspace_id=$1 AND tier='free' AND awake_since IS NOT NULL AND phase<>'deleted'`, workspace, now, monthStart).Scan(&live); err != nil {
		return 0, err
	}
	return stored + live, nil
}

// flushSandboxAwake moves one free sandbox's awake time into the monthly row
// for each UTC month it spans, and clears only its own watermark.
func flushSandboxAwake(ctx context.Context, tx pgx.Tx, workspace, id string, now time.Time) error {
	var since time.Time
	if err := tx.QueryRow(ctx, `SELECT awake_since FROM sandboxes WHERE workspace_id=$1 AND id=$2 AND tier='free' AND awake_since IS NOT NULL FOR UPDATE`, workspace, id).Scan(&since); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sandboxes SET awake_since=NULL WHERE workspace_id=$1 AND id=$2`, workspace, id); err != nil {
		return err
	}
	for start := since.UTC(); start.Before(now); {
		boundary := time.Date(start.Year(), start.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		end := boundary
		if now.Before(end) {
			end = now
		}
		// Match the existing whole-second rounding, independently of the
		// database session timezone.
		delta := int64((end.Sub(start) + time.Second/2) / time.Second)
		if delta > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO sandbox_free_usage(workspace_id,month,awake_seconds) VALUES($1,$2,$3) ON CONFLICT(workspace_id,month) DO UPDATE SET awake_seconds=sandbox_free_usage.awake_seconds+$3`, workspace, sandboxMonth(start), delta); err != nil {
				return err
			}
		}
		start = end.UTC()
	}
	return nil
}

func sandboxTier(size sandboxSize) string {
	if size.Free {
		return "free"
	}
	return "paid"
}

// CreateSandbox validates the product, role, tier policy, and capacity under
// the workspace row, then queues an allocation. account is empty only for
// installation callers already authorized by workspace write grants.
func (h *HTTPServer) CreateSandbox(ctx context.Context, workspace, account string, actor sdk.ActorRef, input SandboxCreateInput) (Sandbox, error) {
	cfg := h.config.Sandboxes
	if !cfg.Enabled {
		return Sandbox{}, fmt.Errorf("Sandboxes aren't available on this server yet.")
	}
	input.Size = strings.TrimSpace(input.Size)
	size, ok := sandboxSizes[input.Size]
	if !ok {
		return Sandbox{}, fmt.Errorf("%w: unknown sandbox size", ErrConflict)
	}
	if input.Desktop && (size.VCPU < 2 || size.Memory < 4096) {
		return Sandbox{}, fmt.Errorf("%w: a desktop requires at least 2 vCPUs and 4 GiB RAM", ErrConflict)
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 80 || strings.ContainsAny(name, "\x00\r\n") {
		return Sandbox{}, fmt.Errorf("%w: sandbox name must be 1-80 characters", ErrConflict)
	}
	if err := validateSandboxLabels(input.Labels); err != nil {
		return Sandbox{}, err
	}
	idle, err := effectiveIdle(input.IdleTimeoutSeconds)
	if err != nil {
		return Sandbox{}, err
	}
	input.SnapshotID = strings.TrimSpace(input.SnapshotID)
	input.TemplateID = strings.TrimSpace(input.TemplateID)
	for _, sid := range []string{input.SnapshotID, input.TemplateID} {
		if len(sid) > 128 || strings.ContainsAny(sid, "\x00\r\n\t") {
			return Sandbox{}, fmt.Errorf("%w: invalid source id", ErrConflict)
		}
	}
	if input.SnapshotID != "" && input.TemplateID != "" {
		return Sandbox{}, fmt.Errorf("%w: snapshot and template are mutually exclusive", ErrConflict)
	}
	if input.TemplateVersion < 0 || input.TemplateVersion > int(^uint32(0)>>1) {
		return Sandbox{}, fmt.Errorf("%w: invalid template version", ErrConflict)
	}
	if input.TemplateVersion != 0 && input.TemplateID == "" {
		return Sandbox{}, fmt.Errorf("%w: templateVersion requires a template", ErrConflict)
	}
	if input.IdempotencyKey != "" {
		if len(input.IdempotencyKey) < 8 || len(input.IdempotencyKey) > 128 {
			return Sandbox{}, fmt.Errorf("%w: idempotencyKey must be 8-128 printable ASCII characters", ErrConflict)
		}
		for _, c := range input.IdempotencyKey {
			if c < 0x20 || c > 0x7e {
				return Sandbox{}, fmt.Errorf("%w: idempotencyKey must be printable ASCII", ErrConflict)
			}
		}
	}
	engine, ok := h.service.Engine.(VPSEngine)
	if !ok {
		return Sandbox{}, fmt.Errorf("Sandboxes aren't available on this server yet.")
	}
	if h.config.Secrets == nil {
		return Sandbox{}, fmt.Errorf("Sandboxes aren't available on this server yet.")
	}
	tier := sandboxTier(size)
	if tier == "free" && idle != -1 {
		return Sandbox{}, fmt.Errorf("%w: free sandboxes keep the mandatory idle policy", ErrConflict)
	}
	now := h.service.Store.now()
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return Sandbox{}, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		return Sandbox{}, err
	}
	if account != "" {
		if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
			return Sandbox{}, err
		}
	}
	enabled, err := h.service.Store.productEnabled(ctx, tx, workspace, "sandboxes")
	if err != nil {
		return Sandbox{}, err
	}
	if !enabled {
		return Sandbox{}, fmt.Errorf("%w: Enable Sandboxes first", ErrConflict)
	}
	environment, err := normalizeSandboxEnvironment(input.Environment, input.SnapshotID != "" || input.TemplateID != "")
	if err != nil {
		return Sandbox{}, err
	}
	input.Environment = environment
	if input.IdempotencyKey != "" {
		keyHash := fmt.Sprintf("%x", sha256.Sum256([]byte(input.IdempotencyKey)))
		var priorHash, priorSandbox string
		err = tx.QueryRow(ctx, `SELECT request_hash,sandbox_id FROM sandbox_create_requests WHERE workspace_id=$1 AND key_hash=$2`, workspace, keyHash).Scan(&priorHash, &priorSandbox)
		switch {
		case err == nil:
			if priorHash != sandboxRequestHash(input, idle) {
				return Sandbox{}, fmt.Errorf("%w: idempotency key was already used with different parameters", ErrConflict)
			}
			existing, serr := h.service.Store.sandbox(ctx, tx, workspace, priorSandbox)
			if serr != nil {
				return Sandbox{}, serr
			}
			tx.Rollback(ctx)
			return existing, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return Sandbox{}, err
		}
	}
	limit := cfg.paidMax()
	if tier == "free" {
		limit = cfg.freeMax()
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM sandboxes WHERE workspace_id=$1 AND phase<>'deleted' AND tier=$2`, workspace, tier).Scan(&count); err != nil {
		return Sandbox{}, err
	}
	if count >= limit {
		return Sandbox{}, fmt.Errorf("%w: this organization already has its allowed %s sandboxes", ErrCapacity, tier)
	}
	if tier == "free" {
		used, err := h.service.Store.SandboxFreeUsage(ctx, tx, workspace, now)
		if err != nil {
			return Sandbox{}, err
		}
		if used >= cfg.freeSecondsPerMonth() {
			return Sandbox{}, fmt.Errorf("%w: this month's free sandbox hours are used up", ErrCapacity)
		}
	} else {
		var billing string
		if err = tx.QueryRow(ctx, `SELECT COALESCE(status,'') FROM workspace_billing WHERE workspace_id=$1`, workspace).Scan(&billing); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Sandbox{}, err
		}
		if billing != "active" {
			return Sandbox{}, fmt.Errorf("%w: paid sandboxes require active billing", ErrConflict)
		}
	}
	id, err := newID("sbx_")
	if err != nil {
		return Sandbox{}, err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Sandbox{}, err
	}
	defer clear(private)
	publicSSH, err := ssh.NewPublicKey(public)
	if err != nil {
		return Sandbox{}, err
	}
	publicKey := string(ssh.MarshalAuthorizedKey(publicSSH))
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return Sandbox{}, err
	}
	defer clear(privateDER)
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	defer clear(privatePEM)
	version, sealed, err := h.config.Secrets.seal(privatePEM, secretBinding(workspace, id, "sandbox-ssh"))
	if err != nil {
		return Sandbox{}, err
	}
	egress := "standard"
	if tier == "free" {
		egress = "restricted"
	}
	if environment != nil && len(environment.SecretRefs) > 0 {
		if err := h.validateSandboxSecretRefsTx(ctx, tx, workspace, environment); err != nil {
			return Sandbox{}, err
		}
	}
	environmentJSON, err := sandboxEnvJSON(environment)
	if err != nil {
		return Sandbox{}, err
	}
	setup := SandboxSetup{}
	if environment != nil && (environment.Repository != nil || environment.SetupCommand != "") {
		setup = SandboxSetup{Status: "pending"}
	}
	setupJSON, err := sandboxSetupJSON(setup)
	if err != nil {
		return Sandbox{}, err
	}
	bootstrap, err := wrencomputer.Bootstrap(wrencomputer.BootstrapConfig{SSHPublicKey: publicKey, SSHSourceCIDR: cfg.SSHSourceCIDR, Egress: egress, ImageVersion: cfg.imageVersion(), Sandbox: true, Desktop: input.Desktop})
	if err != nil {
		return Sandbox{}, err
	}
	request := sdk.VPSRequest{Name: "sandbox", VCPUs: size.VCPU, MemoryMiB: size.Memory, DiskGiB: size.Disk, SSHPublicKey: publicKey, SSHCIDR: cfg.SSHSourceCIDR, ForSeconds: 0, ImageID: cfg.ImageID}
	plan, err := engine.PlanVPS(ctx, workspace, id, request)
	if err != nil {
		return Sandbox{}, fmt.Errorf("no sandbox allocation is currently available")
	}
	if plan.Shape.VCPU != size.VCPU || plan.Shape.Memory != size.Memory || plan.Shape.GB != size.Disk || plan.Shape.ID == "" || plan.ImageID == "" || len(plan.Networks) == 0 {
		return Sandbox{}, fmt.Errorf("sandbox allocation exceeded its authorized bounds")
	}
	plan.Spec.Spec.Compute.Bootstrap = bootstrap
	plan.Spec.Spec.Intent = "Canter sandbox for " + id
	plan.Spec.Spec.M1.Prefix = "workspaces/" + workspace + "/sandboxes/" + id
	monthly, err := pricing.EstimateComputeMonthlyCents(plan.Shape.VCPU, plan.Shape.Memory)
	if err != nil {
		return Sandbox{}, err
	}
	if tier == "paid" {
		// A paid sandbox reserves one day of estimated usage; sleeping or
		// deleting releases it.
		reservation := max(int(1), int((monthly*86400+720*3600-1)/(720*3600)))
		var capLimit, reserved, spent int
		if err = tx.QueryRow(ctx, `SELECT limit_cents,reserved_cents,spent_cents FROM workspace_usage_caps WHERE workspace_id=$1 FOR UPDATE`, workspace).Scan(&capLimit, &reserved, &spent); err != nil {
			return Sandbox{}, err
		}
		if spent+reserved+reservation > capLimit {
			return Sandbox{}, fmt.Errorf("%w: workspace compute allowance is exhausted", ErrCapacity)
		}
		rid, err := newID("usrv_")
		if err != nil {
			return Sandbox{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO usage_reservations(id,workspace_id,subject_kind,subject_id,amount_cents,phase,created_at,updated_at) VALUES($1,$2,'sandbox',$3,$4,'reserved',$5,$5)`, rid, workspace, id, reservation, now); err != nil {
			return Sandbox{}, err
		}
		if _, err = tx.Exec(ctx, `UPDATE workspace_usage_caps SET reserved_cents=reserved_cents+$2,updated_at=$3 WHERE workspace_id=$1`, workspace, reservation, now); err != nil {
			return Sandbox{}, err
		}
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return Sandbox{}, err
	}
	kind, creatorID, creatorName := actor.Kind, actor.ID, actor.DisplayName
	if kind == "" {
		kind = "human"
	}
	if input.Labels == nil {
		input.Labels = map[string]string{}
	}
	labelsJSON, err := json.Marshal(input.Labels)
	if err != nil {
		return Sandbox{}, err
	}
	sourceID, err := h.sandboxCreationSourceTx(ctx, tx, workspace, input, size.Disk)
	if err != nil {
		return Sandbox{}, err
	}
	initialPhase := "queued"
	if sourceID != "" {
		initialPhase = "waking"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sandboxes(id,workspace_id,name,size,tier,phase,plan,ssh_public_key,ssh_key_version,ssh_key_ciphertext,monthly_cents,created_by_kind,created_by_id,created_by_name,awake_since,created_at,updated_at,next_attempt_at,labels,idle_timeout_seconds,current_snapshot_id,desktop,environment,setup) VALUES($1,$2,$3,$4,$5,$17,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14,$14,$14,$15,$16,NULLIF($18,''),$19,$20,$21)`, id, workspace, name, input.Size, tier, raw, publicKey, version, sealed, monthly, kind, creatorID, creatorName, now, labelsJSON, idle, initialPhase, sourceID, input.Desktop, environmentJSON, setupJSON); err != nil {
		return Sandbox{}, err
	}
	if input.IdempotencyKey != "" {
		keyHash := fmt.Sprintf("%x", sha256.Sum256([]byte(input.IdempotencyKey)))
		if _, err = tx.Exec(ctx, `INSERT INTO sandbox_create_requests(workspace_id,key_hash,request_hash,sandbox_id) VALUES($1,$2,$3,$4)`, workspace, keyHash, sandboxRequestHash(input, idle), id); err != nil {
			return Sandbox{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Sandbox{}, err
	}
	details := map[string]any{"name": name, "size": input.Size, "tier": tier, "desktop": input.Desktop}
	if sourceID != "" {
		details["snapshotId"] = sourceID
	}
	if input.TemplateID != "" {
		details["templateId"] = input.TemplateID
		details["templateVersion"] = input.TemplateVersion
	}
	_ = h.service.Store.Audit(ctx, workspace, actor, "sandbox.created", id, details)
	return h.service.Store.Sandbox(ctx, workspace, id)
}

// RequestSandboxSleep marks a ready sandbox for checkpointed shutdown.
func (h *HTTPServer) RequestSandboxSleep(ctx context.Context, workspace, id string, actor sdk.ActorRef) error {
	result, err := h.service.Store.pool.Exec(ctx, `UPDATE sandboxes SET phase=CASE WHEN phase='save_failed' THEN 'sleeping' ELSE phase END,sleep_requested=true,failure=CASE WHEN phase IN ('waking','queued','provisioning') THEN '' ELSE failure END,updated_at=now(),next_attempt_at=now() WHERE workspace_id=$1 AND id=$2 AND phase IN ('ready','provisioning','waking','queued','save_failed')`, workspace, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	_ = h.service.Store.Audit(ctx, workspace, actor, "sandbox.slept", id, map[string]any{})
	return nil
}

// RequestSandboxWake re-queues an asleep sandbox for restore provisioning.
// Waking replans the allocation under a new generation id because provider
// state for a destroyed VM cannot be recreated, and it re-applies the free
// hour and billing policy under the workspace lock.
func (h *HTTPServer) RequestSandboxWake(ctx context.Context, workspace, id, account string, actor sdk.ActorRef) error {
	cfg := h.config.Sandboxes
	engine, ok := h.service.Engine.(VPSEngine)
	if !ok || h.config.Secrets == nil {
		return fmt.Errorf("Sandboxes aren't available on this server yet.")
	}
	now := h.service.Store.now()
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		return err
	}
	if account != "" {
		if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
			return err
		}
	}
	v, err := sandboxForUpdate(ctx, tx, workspace, id)
	if err != nil {
		return err
	}
	if v.DeleteRequested {
		return ErrNotFound
	}
	resuming := false
	if v.Phase == "save_failed" && v.Address != "" {
		resuming = true
	} else if v.Phase != "asleep" {
		return ErrNotFound
	}
	if v.Tier == "free" {
		used, err := h.service.Store.SandboxFreeUsage(ctx, tx, workspace, now)
		if err != nil {
			return err
		}
		if used >= cfg.freeSecondsPerMonth() {
			return fmt.Errorf("%w: this month's free sandbox hours are used up", ErrCapacity)
		}
	} else {
		var billing string
		if err = tx.QueryRow(ctx, `SELECT COALESCE(status,'') FROM workspace_billing WHERE workspace_id=$1`, workspace).Scan(&billing); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if billing != "active" {
			return fmt.Errorf("%w: paid sandboxes require active billing", ErrConflict)
		}
		if !resuming {
			reservation := max(int(1), int((v.MonthlyCents*86400+720*3600-1)/(720*3600)))
			var capLimit, reserved, spent int
			if err = tx.QueryRow(ctx, `SELECT limit_cents,reserved_cents,spent_cents FROM workspace_usage_caps WHERE workspace_id=$1 FOR UPDATE`, workspace).Scan(&capLimit, &reserved, &spent); err != nil {
				return err
			}
			if spent+reserved+reservation > capLimit {
				return fmt.Errorf("%w: workspace compute allowance is exhausted", ErrCapacity)
			}
			rid, err := newID("usrv_")
			if err != nil {
				return err
			}
			inserted, err := tx.Exec(ctx, `INSERT INTO usage_reservations(id,workspace_id,subject_kind,subject_id,amount_cents,phase,created_at,updated_at)
				VALUES($1,$2,'sandbox',$3,$4,'reserved',$5,$5)
				ON CONFLICT(workspace_id,subject_kind,subject_id) DO UPDATE SET
				 amount_cents=EXCLUDED.amount_cents,phase='reserved',updated_at=EXCLUDED.updated_at
				WHERE usage_reservations.phase='released'`, rid, workspace, id, reservation, now)
			if err != nil {
				return err
			}
			if inserted.RowsAffected() != 1 {
				return fmt.Errorf("%w: sandbox reservation already active", ErrConflict)
			}
			if _, err = tx.Exec(ctx, `UPDATE workspace_usage_caps SET reserved_cents=reserved_cents+$2,updated_at=$3 WHERE workspace_id=$1`, workspace, reservation, now); err != nil {
				return err
			}
		}
	}
	if resuming {
		// RESUME the retained VM — no new allocation or paid reservation. The
		// generation must advance so a later sleep can't reuse a pre-repair
		// snapshot, and any interrupted pending save is abandoned.
		if v.PendingSnapshotID != "" {
			if _, err := tx.Exec(ctx, `UPDATE sandbox_snapshots SET state='failed',failure='superseded by resume',key_version='',key_ciphertext='',updated_at=now() WHERE workspace_id=$1 AND id=$2 AND state='creating'`, workspace, v.PendingSnapshotID); err != nil {
				return err
			}
		}
		result, err := tx.Exec(ctx, `UPDATE sandboxes SET phase='ready',generation=generation+1,pending_snapshot_id=NULL,sleep_requested=false,sleep_forced_at=NULL,failure='',next_attempt_at=now(),updated_at=now() WHERE workspace_id=$1 AND id=$2 AND phase='save_failed'`, workspace, id)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return ErrNotFound
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		_ = h.service.Store.Audit(ctx, workspace, actor, "sandbox.resumed", id, map[string]any{})
		return nil
	}
	size := sandboxSizes[v.Size]
	egress := "standard"
	if v.Tier == "free" {
		egress = "restricted"
	}
	bootstrap, err := wrencomputer.Bootstrap(wrencomputer.BootstrapConfig{SSHPublicKey: v.SSHPublicKey, SSHSourceCIDR: cfg.SSHSourceCIDR, Egress: egress, ImageVersion: cfg.imageVersion(), Sandbox: true, Desktop: v.Desktop})
	if err != nil {
		return err
	}
	generation := v.Generation + 1
	allocation := id + "-g" + strconv.Itoa(generation)
	request := sdk.VPSRequest{Name: "sandbox", VCPUs: size.VCPU, MemoryMiB: size.Memory, DiskGiB: size.Disk, SSHPublicKey: v.SSHPublicKey, SSHCIDR: cfg.SSHSourceCIDR, ForSeconds: 0, ImageID: cfg.ImageID}
	plan, err := engine.PlanVPS(ctx, workspace, allocation, request)
	if err != nil {
		return fmt.Errorf("no sandbox allocation is currently available")
	}
	if plan.Shape.VCPU != size.VCPU || plan.Shape.Memory != size.Memory || plan.Shape.GB != size.Disk || plan.Shape.ID == "" || plan.ImageID == "" || len(plan.Networks) == 0 {
		return fmt.Errorf("sandbox allocation exceeded its authorized bounds")
	}
	plan.Spec.Spec.Compute.Bootstrap = bootstrap
	plan.Spec.Spec.Intent = "Canter sandbox for " + allocation
	plan.Spec.Spec.M1.Prefix = "workspaces/" + workspace + "/sandboxes/" + id + "/g" + strconv.Itoa(generation)
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE sandboxes SET phase='waking',generation=$3,plan=$4,sleep_requested=false,sleep_forced_at=NULL,failure='',awake_since=$5,updated_at=now(),next_attempt_at=now() WHERE workspace_id=$1 AND id=$2 AND phase='asleep'`, workspace, id, generation, raw, now)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	_ = h.service.Store.Audit(ctx, workspace, actor, "sandbox.woke", id, map[string]any{})
	return nil
}

func sandboxForUpdate(ctx context.Context, tx pgx.Tx, workspace, id string) (Sandbox, error) {
	return scanSandbox(tx.QueryRow(ctx, `SELECT `+sandboxColumns+` FROM sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, id))
}

// RequestSandboxDelete marks a sandbox for verified teardown.
func (h *HTTPServer) RequestSandboxDelete(ctx context.Context, workspace, id string, actor sdk.ActorRef) error {
	result, err := h.service.Store.pool.Exec(ctx, `UPDATE sandboxes SET phase='deleting',delete_requested=true,updated_at=now(),next_attempt_at=now() WHERE workspace_id=$1 AND id=$2 AND phase<>'deleted'`, workspace, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	_ = h.service.Store.Audit(ctx, workspace, actor, "sandbox.deleted", id, map[string]any{})
	return nil
}

// sandboxAgentClient is the guest-agent surface sandboxes use; *wrencomputer.Client
// satisfies it and tests substitute scripted fakes through HTTPServer.sandboxDial.
func (s *Store) recordSandboxJob(ctx context.Context, sandboxID, jobID string) error {
	result, err := s.pool.Exec(ctx, `INSERT INTO sandbox_jobs(sandbox_id,job_id,started_at)
SELECT id,$2,$3 FROM sandboxes WHERE id=$1 AND phase='ready' AND NOT sleep_requested AND NOT delete_requested
ON CONFLICT DO NOTHING`, sandboxID, jobID, s.now())
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("%w: sandbox changed before the job could start", ErrConflict)
	}
	return nil
}

func (s *Store) finishSandboxJob(ctx context.Context, sandboxID, jobID string) {
	_, _ = s.pool.Exec(ctx, `UPDATE sandbox_jobs SET finished_at=now() WHERE sandbox_id=$1 AND job_id=$2 AND finished_at IS NULL`, sandboxID, jobID)
}

type openSandboxJob struct {
	ID        string
	StartedAt time.Time
}

// A failed lookup is uncertainty, never evidence that a workload is idle.
func (s *Store) openSandboxJobs(ctx context.Context, sandboxID string) ([]openSandboxJob, error) {
	rows, err := s.pool.Query(ctx, `SELECT job_id,started_at FROM sandbox_jobs WHERE sandbox_id=$1 AND finished_at IS NULL`, sandboxID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[openSandboxJob])
}

// touchSandbox bumps last_used_at at most once per 30s (terminal/preview I/O).
func (s *Store) touchSandbox(ctx context.Context, sandboxID string) {
	_, _ = s.pool.Exec(ctx, `UPDATE sandboxes SET last_used_at=$2 WHERE id=$1 AND phase='ready' AND (last_used_at IS NULL OR last_used_at < $2 - interval '30 seconds')`, sandboxID, s.now())
}

type sandboxAgentClient interface {
	Probe(ctx context.Context) (wrencomputer.Capabilities, error)
	StartJob(ctx context.Context, request wrencomputer.JobRequest) (wrencomputer.Job, error)
	Job(ctx context.Context, id string) (wrencomputer.Job, error)
	StopJob(ctx context.Context, id string) (wrencomputer.Job, error)
	ReadFile(ctx context.Context, path string, offset int64, limit int) (wrencomputer.FileChunk, error)
	ReadFileVersion(ctx context.Context, path string, offset int64, limit int, version string) (wrencomputer.FileChunk, error)
	JobLogs(ctx context.Context, id, stream string, offset int64, limit int) (wrencomputer.JobLogChunk, error)
	BeginUpload(ctx context.Context, id, path string, size int64, sha256 string) (wrencomputer.FileUpload, error)
	UploadChunk(ctx context.Context, id string, offset int64, data []byte) (wrencomputer.FileUpload, error)
	CompleteUpload(ctx context.Context, id string) (wrencomputer.FileUpload, error)
	AbortUpload(ctx context.Context, id string) error
	Metrics(ctx context.Context) (wrencomputer.ResourceMetrics, error)
	WriteFile(ctx context.Context, path string, data []byte) (wrencomputer.FileChunk, error)
	ListFiles(ctx context.Context, path string) ([]wrencomputer.FileEntry, error)
	CreateCheckpoint(ctx context.Context, id string, includeBrowser bool, keepStopped ...bool) ([]byte, wrencomputer.CheckpointManifest, error)
	RestoreCheckpoint(ctx context.Context, id string, data []byte, includeBrowser bool) error
	OpenTerminal(ctx context.Context, cols, rows int) (*wrencomputer.Terminal, error)
	DialPort(ctx context.Context, port int) (net.Conn, error)
	Close()
}

func (h *HTTPServer) dialSandbox(config wrencomputer.Config) (sandboxAgentClient, error) {
	if h.sandboxDial != nil {
		return h.sandboxDial(config)
	}
	return wrencomputer.NewSSH(config)
}

func (h *HTTPServer) sandboxClient(ctx context.Context, v Sandbox) (sandboxAgentClient, error) {
	if (v.Phase != "ready" && v.Phase != "save_failed") || v.Address == "" {
		return nil, fmt.Errorf("%w: this sandbox is not ready", ErrConflict)
	}
	key, err := h.config.Secrets.open(v.KeyVersion, v.KeyCiphertext, secretBinding(v.WorkspaceID, v.ID, "sandbox-ssh"))
	if err != nil {
		return nil, err
	}
	defer clear(key)
	client, err := h.dialSandbox(wrencomputer.Config{Address: v.Address, PrivateKeyPEM: key, HostKeySHA256: v.HostFingerprint, UserFiles: true})
	if err != nil {
		return nil, err
	}
	return &fencedSandboxClient{sandboxAgentClient: client, store: h.service.Store, sandbox: v}, nil
}

// SandboxPlans feeds paid-tier billing. Free sandboxes are never metered and
// asleep sandboxes hold no VM.
func (s *Store) SandboxPlans(ctx context.Context, workspace string) ([]sdk.VPSPlan, error) {
	rows, err := s.pool.Query(ctx, `SELECT plan FROM sandboxes WHERE workspace_id=$1 AND tier='paid' AND phase NOT IN ('deleted','asleep','failed') AND plan IS NOT NULL`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sdk.VPSPlan{}
	for rows.Next() {
		var raw []byte
		var plan sdk.VPSPlan
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &plan); err != nil {
			return nil, err
		}
		out = append(out, plan)
	}
	return out, rows.Err()
}

// SandboxDispatcher resumes sandbox intents: provisioning, checkpointed sleep,
// wake-restore, verified deletion, idle auto-sleep, and free-hour accounting.
type SandboxDispatcher struct{ Server *HTTPServer }

func (d *SandboxDispatcher) Run(ctx context.Context) error {
	if d.Server == nil {
		return fmt.Errorf("sandbox dispatcher has no server")
	}
	// Snapshot GC loop — bounded runs, no overlap, stops with Run.
	go func() {
		gc := time.NewTicker(time.Minute)
		defer gc.Stop()
		for {
			gctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			if err := d.Server.collectSandboxSnapshotGarbage(gctx); err != nil && ctx.Err() == nil {
				slog.Warn("sandbox snapshot gc", "error", err)
			}
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-gc.C:
			}
		}
	}()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	slots := make(chan struct{}, 2)
	for {
		rows, err := d.Server.service.Store.pool.Query(ctx, `SELECT workspace_id,id FROM sandboxes WHERE phase<>'deleted' AND (delete_requested OR phase NOT IN ('failed','asleep','save_failed')) AND (next_attempt_at IS NULL OR next_attempt_at<=now()) ORDER BY next_attempt_at LIMIT 30`)
		if err != nil {
			return err
		}
		items := [][2]string{}
		for rows.Next() {
			var item [2]string
			if err = rows.Scan(&item[0], &item[1]); err != nil {
				rows.Close()
				return err
			}
			items = append(items, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, item := range items {
			select {
			case slots <- struct{}{}:
				go func(workspace, id string) { defer func() { <-slots }(); _ = d.Reconcile(ctx, workspace, id) }(item[0], item[1])
			default:
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func sandboxSetupFailure(err error) string {
	stage := "provider setup"
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "boot proof"):
		stage = "waiting for the trusted boot receipt"
	case strings.Contains(text, "bootstrap failed"):
		stage = "guest bootstrap"
	case strings.Contains(text, "wait for compute"):
		stage = "waiting for the provider VM"
	case strings.Contains(text, "state") || strings.Contains(text, "intent") || strings.Contains(text, "fence"):
		stage = "durable provider state reconciliation"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		stage += " (setup deadline reached)"
	}
	return "Sandbox setup failed during " + stage + ". Canter is cleaning up the allocation."
}

// save persists the dispatcher's runtime view with a runtime-only fenced CAS:
// it cannot overwrite storage pointers/key metadata from a stale in-memory
// Sandbox, and it refuses to write through a generation or observed-phase
// change so a concurrent sleep/delete request is never clobbered.
func (d *SandboxDispatcher) save(ctx context.Context, v *Sandbox) error {
	next := d.Server.service.Store.now().Add(3 * time.Second)
	if (v.Phase == "ready" && !sandboxSetupPending(v.Setup)) || v.Phase == "asleep" {
		next = d.Server.service.Store.now().Add(30 * time.Second)
	}
	err := d.Server.service.Store.pool.QueryRow(ctx, `UPDATE sandboxes SET phase=$2,address=NULLIF($3,'')::inet,host_fingerprint=NULLIF($4,''),ssh_key_ciphertext=$5,failure=$6,
		sleep_requested=CASE WHEN $2 IN ('asleep','deleted') THEN $7 ELSE sleep_requested OR $7 END,
		awake_since=$8,last_used_at=GREATEST(last_used_at,$9),sleep_forced_at=$10,notice=$11,
		next_attempt_at=CASE WHEN $2 NOT IN ('asleep','deleted') AND (sleep_requested OR $7)
		 THEN LEAST($12,now()+interval '3 seconds') ELSE $12 END,updated_at=now(),
		current_snapshot_id=CASE WHEN $2='deleted' THEN NULL ELSE current_snapshot_id END,
		pending_snapshot_id=CASE WHEN $2='deleted' THEN NULL ELSE pending_snapshot_id END,
		checkpoint_object_ref=CASE WHEN $2='deleted' THEN NULL ELSE checkpoint_object_ref END,
		checkpoint_key_version=CASE WHEN $2='deleted' THEN '' ELSE checkpoint_key_version END,
		checkpoint_nonce=CASE WHEN $2='deleted' THEN NULL ELSE checkpoint_nonce END,
		checkpoint_manifest=CASE WHEN $2='deleted' THEN NULL ELSE checkpoint_manifest END
		WHERE id=$1 AND workspace_id=$13 AND generation=$14 AND phase=$15
		 AND (NOT delete_requested OR $2 IN ('deleting','deleted'))
		RETURNING sleep_requested`,
		v.ID, v.Phase, v.Address, v.HostFingerprint, v.KeyCiphertext, v.Failure, v.SleepRequested,
		v.AwakeSince, v.LastUsedAt, v.SleepForcedAt, v.Notice, next, v.WorkspaceID, v.Generation, v.observedPhase).Scan(&v.SleepRequested)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	v.observedPhase = v.Phase
	return nil
}

func (d *SandboxDispatcher) destroyCheckpoint(ctx context.Context, v Sandbox) error {
	if v.CheckpointObject.Key == "" {
		return nil
	}
	engine, ok := d.Server.service.Engine.(ComputerCheckpointEngine)
	if !ok {
		return fmt.Errorf("checkpoint storage unavailable")
	}
	return engine.DeleteComputerCheckpoint(ctx, v.checkpointScope(), v.CheckpointObject)
}

func (v Sandbox) checkpointScope() string {
	return "workspaces/" + v.WorkspaceID + "/sandboxes/" + v.ID
}

func (d *SandboxDispatcher) Reconcile(ctx context.Context, workspace, id string) error {
	h := d.Server
	s := h.service.Store
	engine, ok := h.service.Engine.(VPSEngine)
	if !ok {
		return fmt.Errorf("sandbox provider unavailable")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,947))`, id).Scan(&locked); err != nil || !locked {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1,947))`, id)
	v, err := s.sandbox(ctx, conn, workspace, id)
	if err != nil {
		return err
	}
	if v.Phase == "deleted" || (!v.DeleteRequested && (v.Phase == "failed" || v.Phase == "save_failed")) {
		return nil
	}
	if v.DeleteRequested {
		v.Phase = "deleting"
	}
	now := s.now()
	// Free-tier accounting: force sleep once the monthly budget is spent.
	if v.Tier == "free" && (v.Phase == "ready" || v.Phase == "provisioning" || v.Phase == "waking") && v.AwakeSince != nil {
		used, e := s.SandboxFreeUsage(ctx, conn, workspace, now)
		if e == nil && used >= h.config.Sandboxes.freeSecondsPerMonth() {
			if v.Phase == "waking" && !v.SleepRequested {
				v.Failure = ""
			}
			v.SleepRequested = true
			if v.SleepForcedAt == nil {
				v.SleepForcedAt = &now
			}
		}
	}
	// Idle auto-sleep for ready sandboxes. A guest job still running counts
	// as activity; the free-hour cap above forces sleep regardless.
	if v.Phase == "ready" && !v.SleepRequested && !sandboxSetupPending(v.Setup) {
		idleBase := v.AwakeSince
		if v.LastUsedAt != nil {
			idleBase = v.LastUsedAt
		}
		idleTimeout := h.config.Sandboxes.idleSleep()
		if v.Tier == "paid" && v.IdleTimeoutSeconds >= 0 {
			idleTimeout = time.Duration(v.IdleTimeoutSeconds) * time.Second
		}
		if idleBase != nil && idleTimeout > 0 && now.Sub(*idleBase) > idleTimeout {
			if d.jobsStillRunning(ctx, v) {
				s.touchSandbox(ctx, v.ID)
				v.LastUsedAt = &now
			} else {
				v.SleepRequested = true
			}
		}
	}
	if v.Phase == "waking" && v.SleepRequested {
		return d.stopSandboxWake(ctx, &v, engine)
	}
	if v.SleepRequested && v.Phase != "sleeping" && v.Phase != "asleep" && v.Phase != "deleting" {
		v.Phase = "sleeping"
	}
	switch v.Phase {
	case "deleting":
		if err = d.save(ctx, &v); err != nil {
			return err
		}
		operation, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		state, e := engine.DestroyVPS(operation, v.Plan)
		if e != nil || state.Phase != "destroyed" {
			return firstErr(e, ErrConflict)
		}
		if err = d.settleStoppedSandbox(ctx, &v); err != nil {
			return err
		}
		if err = d.destroyCheckpoint(ctx, v); err != nil {
			return err
		}
		if err = d.Server.eraseSandboxSnapshots(ctx, v); err != nil {
			return err
		}
		v.Phase = "deleted"
		v.Address, v.HostFingerprint = "", ""
		v.KeyCiphertext = []byte{}
		v.CheckpointObject = sdk.ComputerCheckpointObject{}
		v.CheckpointKey, v.CheckpointNonce, v.CheckpointManifest = "", nil, nil
		return d.save(ctx, &v)
	case "sleeping":
		// Atomic barrier: wait out live mutation/lease claims before saving.
		if e := d.Server.service.Store.enterSandboxSleep(ctx, v); e != nil {
			return e
		}
		v.SleepRequested = true
		v.observedPhase = "sleeping"
		// v2 verified snapshot, then destroy — no legacy saves, no destructive
		// forced fallback: a failed export keeps the VM in save_failed.
		if v.Address != "" {
			if e := d.saveSandboxFiles(ctx, &v); e != nil {
				return e
			}
		}
		stopCtx, stopCancel := context.WithTimeout(ctx, 2*time.Minute)
		state, e := engine.DestroyVPS(stopCtx, v.Plan)
		stopCancel()
		if e != nil || state.Phase != "destroyed" {
			return firstErr(e, ErrConflict)
		}
		if err = d.settleStoppedSandbox(ctx, &v); err != nil {
			return err
		}
		v.Phase = "asleep"
		v.SleepRequested = false
		v.SleepForcedAt = nil
		v.Address, v.HostFingerprint = "", ""
		v.AwakeSince = nil
		v.Failure = ""
		return d.save(ctx, &v)
	case "ready":
		if sandboxSetupPending(v.Setup) {
			if err := d.reconcileSandboxSetup(ctx, &v); err != nil {
				return err
			}
		}
		return d.save(ctx, &v)
	}
	// queued / provisioning / waking: apply the durable allocation. A wake
	// keeps its "waking" phase so a failure parks the sandbox asleep with its
	// checkpoint instead of deleting saved files.
	waking := v.Phase == "waking"
	fail := func(message string, cause error) error {
		if !waking {
			v.Phase = "deleting"
			v.Failure = message
			_ = d.save(ctx, &v)
			return cause
		}
		v.Failure = message + " The encrypted checkpoint was retained."
		return d.stopSandboxWake(ctx, &v, engine)
	}
	if !waking {
		v.Phase = "provisioning"
	}
	if err = d.save(ctx, &v); err != nil {
		return err
	}
	if v.SleepRequested {
		return nil
	}
	operation, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	state, applyErr := engine.ApplyVPS(operation, v.Plan)
	if applyErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("sandbox provider apply failed", "sandbox", v.ID, "error", applyErr)
		if err = fail(sandboxSetupFailure(applyErr), applyErr); err != nil {
			return err
		}
		return applyErr
	}
	if state.Phase != "ready" || len(state.Resources) != 1 || state.Resources[0].Address == "" {
		v.Failure = "Waiting for the provider VM."
		return d.save(ctx, &v)
	}
	fingerprint, e := engine.VPSHostFingerprint(operation, v.Plan)
	if e != nil || !strings.HasPrefix(fingerprint, "SHA256:") {
		return fail("Sandbox identity could not be verified.", firstErr(e, ErrConflict))
	}
	v.Address, v.HostFingerprint = state.Resources[0].Address, fingerprint
	client, e := d.sandboxDirectClient(v)
	if e != nil {
		return e
	}
	defer client.Close()
	probeCtx, probeCancel := context.WithTimeout(operation, 30*time.Second)
	capabilities, e := client.Probe(probeCtx)
	probeCancel()
	if e != nil || !capabilities.PersistentFiles || !capabilities.DurableJobs {
		v.Failure = "The VM is running; waiting for its tools."
		_ = d.save(ctx, &v)
		return e
	}
	if v.CurrentSnapshotID != "" || v.CheckpointObject.Key != "" {
		restoreView := v // immutable view for the monitor — v mutates after restore
		restore, restoreCancel := context.WithTimeout(ctx, 6*time.Hour)
		monitorDone := make(chan struct{})
		go func() { // cancel restore if the row leaves its transition state
			defer close(monitorDone)
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-restore.Done():
					return
				case <-ticker.C:
				}
				check, c := context.WithTimeout(restore, 5*time.Second)
				var live bool
				e := s.pool.QueryRow(check, `SELECT EXISTS(SELECT 1 FROM sandboxes WHERE workspace_id=$1 AND id=$2 AND generation=$3 AND phase=$4 AND NOT delete_requested AND NOT sleep_requested)`, restoreView.WorkspaceID, restoreView.ID, restoreView.Generation, restoreView.Phase).Scan(&live)
				c()
				if e != nil || !live {
					restoreCancel()
					return
				}
			}
		}()
		e := d.restoreSandboxFiles(restore, v, client)
		restoreCancel() // stop the monitor the moment restore returns
		<-monitorDone
		if e != nil {
			return fail("Saved files could not be restored.", e)
		}
	}
	v.Notice = reinstallSandboxPackages(ctx, client)
	v.Phase = "ready"
	v.Failure = ""
	if v.AwakeSince == nil {
		awake := s.now()
		v.AwakeSince = &awake
	}
	return d.save(ctx, &v)
}

// jobsStillRunning asks the guest whether any recorded job is still active.
// Transport or status uncertainty is treated conservatively as activity: a
// possibly running workload is never destroyed because its status RPC failed.
func (d *SandboxDispatcher) jobsStillRunning(ctx context.Context, v Sandbox) bool {
	open, err := d.Server.service.Store.openSandboxJobs(ctx, v.ID)
	if err != nil {
		return true
	}
	if len(open) == 0 {
		return false
	}
	client, err := d.sandboxDirectClient(v)
	if err != nil {
		return true
	}
	defer client.Close()
	for _, record := range open {
		job, err := client.Job(ctx, record.ID)
		if err != nil {
			var remote *wrencomputer.Error
			missing := errors.Is(err, ErrNotFound) || (errors.As(err, &remote) && remote.Code == "not_found")
			if !missing || d.Server.service.Store.now().Sub(record.StartedAt) < 30*time.Second {
				return true
			}
			// Close the uncertain launch with a guest tombstone before allowing
			// sleep. A late StartJob with this ID then observes cancellation.
			job, err = client.StopJob(ctx, record.ID)
			if err != nil {
				return true
			}
		}
		if job.Status == "starting" || job.Status == "running" {
			return true
		}
		d.Server.service.Store.finishSandboxJob(ctx, v.ID, record.ID)
	}
	return false
}

// reinstallSandboxPackages re-applies the apt packages recorded in the
// checkpointed /workspace/.canter-packages. Best-effort: the result is a
// non-fatal notice on the sandbox, never a wake failure.
func reinstallSandboxPackages(ctx context.Context, client sandboxAgentClient) string {
	job, err := client.StartJob(ctx, wrencomputer.JobRequest{ID: "canter-apt-restore-" + strconv.FormatInt(time.Now().UnixNano(), 36), Command: "if [ -s /workspace/.canter-packages ]; then sudo /usr/local/sbin/canter-apt install $(tr '\n' ' ' < /workspace/.canter-packages); fi", TimeoutSeconds: 660})
	if err != nil {
		return "Some installed packages could not be restored after wake."
	}
	for i := 0; i < 120; i++ {
		job, err = client.Job(ctx, job.ID)
		if err != nil {
			return "Package restore status could not be checked."
		}
		if job.Status == "completed" {
			return ""
		}
		if job.Status != "starting" && job.Status != "running" {
			return "Some installed packages could not be restored after wake."
		}
		select {
		case <-ctx.Done():
			return "Package restore is still running."
		case <-time.After(2 * time.Second):
		}
	}
	return "Package restore is still running."
}

func (d *SandboxDispatcher) sandboxDirectClient(v Sandbox) (sandboxAgentClient, error) {
	key, err := d.Server.config.Secrets.open(v.KeyVersion, v.KeyCiphertext, secretBinding(v.WorkspaceID, v.ID, "sandbox-ssh"))
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return d.Server.dialSandbox(wrencomputer.Config{Address: v.Address, PrivateKeyPEM: key, HostKeySHA256: v.HostFingerprint})
}

func firstErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
