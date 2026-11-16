package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/pricing"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
)

// The allowlist is an operator-owned paid-compute grant. Models cannot widen it.
type WrenComputerConfig struct {
	SSHSourceCIDR      string
	AllowedWorkspaces  []string
	MaxPerWorkspace    int
	MaxLifetimeSeconds int
	MaxMonthlyCents    int64
}

func (c WrenComputerConfig) allows(workspace string) bool {
	for _, id := range c.AllowedWorkspaces {
		if id == workspace {
			return true
		}
	}
	return false
}
func (c WrenComputerConfig) maximum() int {
	if c.MaxPerWorkspace < 1 {
		return 2
	}
	return min(c.MaxPerWorkspace, 2)
}
func (c WrenComputerConfig) lifetime() time.Duration {
	n := c.MaxLifetimeSeconds
	if n < 300 {
		n = 3600
	}
	return time.Duration(min(n, 604800)) * time.Second
}

type WrenComputer struct {
	RestoreCheckpointID string      `json:"restoreCheckpointId,omitempty"`
	ID                  string      `json:"id"`
	WorkspaceID         string      `json:"workspaceId"`
	ConversationID      string      `json:"conversationId"`
	OwnerID             string      `json:"ownerId"`
	ScopeKind           string      `json:"scopeKind"`
	Name                string      `json:"name"`
	Phase               string      `json:"phase"`
	VCPUs               int         `json:"vcpus"`
	MemoryMiB           int         `json:"memoryMiB"`
	DiskGiB             int         `json:"diskGiB"`
	MonthlyCents        int64       `json:"monthlyCents"`
	EstimatedCents      int64       `json:"estimatedCents"`
	Failure             string      `json:"failure,omitempty"`
	DeleteRequested     bool        `json:"deleteRequested"`
	CreatedAt           time.Time   `json:"createdAt"`
	UpdatedAt           time.Time   `json:"updatedAt"`
	LastUsedAt          *time.Time  `json:"lastUsedAt,omitempty"`
	ExpiresAt           time.Time   `json:"expiresAt"`
	ControlAccountID    string      `json:"controlAccountId,omitempty"`
	ControlName         string      `json:"controlName,omitempty"`
	ControlExpiresAt    *time.Time  `json:"controlExpiresAt,omitempty"`
	MutationRunID       string      `json:"busyRunId,omitempty"`
	MutationExpiresAt   *time.Time  `json:"busyUntil,omitempty"`
	MutationLease       string      `json:"-"`
	BusyReason          string      `json:"busyReason,omitempty"`
	UnsettledCodingWork int         `json:"unsettledCodingWork,omitempty"`
	UnsettledCommands   int         `json:"unsettledCommands,omitempty"`
	SuspendSupported    bool        `json:"suspendSupported"`
	Plan                sdk.VPSPlan `json:"-"`
	KeyVersion          string      `json:"-"`
	KeyCiphertext       []byte      `json:"-"`
	Address             string      `json:"-"`
	HostFingerprint     string      `json:"-"`
}

const computerColumns = `id,workspace_id,conversation_id,COALESCE(owner_id,''),scope_kind,name,phase,plan,ssh_key_version,ssh_key_ciphertext,address,host_fingerprint,monthly_cents,failure,delete_requested,created_at,updated_at,last_used_at,expires_at,COALESCE(control_account_id,''),control_expires_at,COALESCE(mutation_run_id,''),COALESCE(mutation_lease_token,''),mutation_expires_at,COALESCE(restore_checkpoint_id,'')`

func scanWrenComputer(row pgx.Row) (WrenComputer, error) {
	var v WrenComputer
	var plan []byte
	err := row.Scan(&v.ID, &v.WorkspaceID, &v.ConversationID, &v.OwnerID, &v.ScopeKind, &v.Name, &v.Phase, &plan, &v.KeyVersion, &v.KeyCiphertext, &v.Address, &v.HostFingerprint, &v.MonthlyCents, &v.Failure, &v.DeleteRequested, &v.CreatedAt, &v.UpdatedAt, &v.LastUsedAt, &v.ExpiresAt, &v.ControlAccountID, &v.ControlExpiresAt, &v.MutationRunID, &v.MutationLease, &v.MutationExpiresAt, &v.RestoreCheckpointID)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal(plan, &v.Plan); err != nil {
		return v, err
	}
	if v.MutationRunID != "" && v.MutationExpiresAt != nil && v.MutationExpiresAt.After(time.Now()) {
		v.BusyReason = "An agent is using this computer."
	}
	v.VCPUs, v.MemoryMiB, v.DiskGiB = v.Plan.Shape.VCPU, v.Plan.Shape.Memory, v.Plan.Shape.GB
	duration := max(int64(0), int64(v.ExpiresAt.Sub(v.CreatedAt).Seconds()))
	v.EstimatedCents = (v.MonthlyCents*duration + 720*3600 - 1) / (720 * 3600)
	return v, nil
}
func (s *Store) computerScope(ctx context.Context, workspace, account, conversation string, manage bool) (kind, name, owner string, err error) {
	if _, err = s.Conversation(ctx, workspace, account, conversation); err != nil {
		return
	}
	role, membershipErr := s.Membership(ctx, account, workspace)
	if membershipErr != nil {
		err = ErrForbidden
		return
	}
	if manage && role == "viewer" {
		err = ErrForbidden
		return
	}
	err = s.pool.QueryRow(ctx, `SELECT 'private',name,account_id FROM wrens WHERE workspace_id=$1 AND conversation_id=$2 UNION ALL SELECT 'private',COALESCE(p.profile->>'name',chat.profile_snapshot->>'name'),chat.account_id FROM wren_shared_chats chat LEFT JOIN wren_publications p ON p.wren_id=chat.wren_id WHERE chat.workspace_id=$1 AND chat.conversation_id=$2 UNION ALL SELECT 'room',name,account_id FROM wren_rooms WHERE workspace_id=$1 AND conversation_id=$2 LIMIT 1`, workspace, conversation).Scan(&kind, &name, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err == nil && manage && owner != account {
		err = ErrForbidden
	}
	return
}
func (s *Store) wrenComputerPrivate(ctx context.Context, workspace, id string) (WrenComputer, error) {
	return scanWrenComputer(s.pool.QueryRow(ctx, `SELECT `+computerColumns+` FROM wren_computers WHERE workspace_id=$1 AND id=$2`, workspace, id))
}
func (s *Store) WrenComputer(ctx context.Context, workspace, account, id string) (WrenComputer, error) {
	v, err := s.wrenComputerPrivate(ctx, workspace, id)
	if err != nil {
		return v, err
	}
	_, _, _, err = s.computerScope(ctx, workspace, account, v.ConversationID, false)
	if err == nil {
		err = s.computerControlPresentation(ctx, &v)
	}
	return v, err
}

// Present only a current, authorized holder. The private row remains unchanged;
// input and takeover still arbitrate against the stored lease on each request.
func (s *Store) computerControlPresentation(ctx context.Context, v *WrenComputer) error {
	if v.Phase == "ready" {
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_harness_executions WHERE computer_id=$1 AND NOT process_stopped`, v.ID).Scan(&v.UnsettledCodingWork); err != nil {
			return err
		}
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_computer_commands WHERE computer_id=$1 AND status NOT IN ('completed','cancelled','failed','timed_out','not_started')`, v.ID).Scan(&v.UnsettledCommands); err != nil {
			return err
		}
	}

	clearControl := func() { v.ControlAccountID = ""; v.ControlExpiresAt = nil; v.ControlName = "" }
	if v.ControlAccountID == "" || v.ControlExpiresAt == nil || !v.ControlExpiresAt.After(s.now()) {
		clearControl()
		return nil
	}
	if _, _, _, err := s.computerScope(ctx, v.WorkspaceID, v.ControlAccountID, v.ConversationID, false); err != nil {
		if errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) {
			clearControl()
			return nil
		}
		return err
	}
	role, err := s.Membership(ctx, v.ControlAccountID, v.WorkspaceID)
	if errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) || err == nil && role == "viewer" {
		clearControl()
		return nil
	}
	if err != nil {
		return err
	}
	var name string
	if err = s.pool.QueryRow(ctx, `SELECT email FROM accounts WHERE id=$1 AND disabled_at IS NULL`, v.ControlAccountID).Scan(&name); errors.Is(err, pgx.ErrNoRows) {
		clearControl()
		return nil
	} else if err != nil {
		return err
	}
	if at := strings.LastIndex(name, "@"); at >= 0 {
		name = name[:at]
	}
	v.ControlName = name
	return nil
}
func (s *Store) WrenComputers(ctx context.Context, workspace, account string) ([]WrenComputer, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+computerColumns+` FROM wren_computers WHERE workspace_id=$1 ORDER BY created_at DESC`, workspace)
	if err != nil {
		return nil, err
	}
	items := []WrenComputer{}
	for rows.Next() {
		v, e := scanWrenComputer(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	visible := []WrenComputer{}
	for _, v := range items {
		if _, _, _, e := s.computerScope(ctx, workspace, account, v.ConversationID, false); e == nil {
			if e = s.computerControlPresentation(ctx, &v); e != nil {
				return nil, e
			}
			visible = append(visible, v)
		}
	}
	return visible, nil
}
func (h *HTTPServer) EnableWrenComputer(ctx context.Context, workspace, account, conversation string) (WrenComputer, error) {
	return h.enableWrenComputer(ctx, workspace, account, conversation, "")
}
func (h *HTTPServer) enableWrenComputer(ctx context.Context, workspace, account, conversation, restoreID string) (WrenComputer, error) {
	cfg := h.config.Computers
	if !cfg.allows(workspace) {
		return WrenComputer{}, fmt.Errorf("%w: computers are not enabled for this workspace", ErrForbidden)
	}
	if h.config.Secrets == nil {
		return WrenComputer{}, fmt.Errorf("computer key storage is not configured")
	}
	engine, ok := h.service.Engine.(VPSEngine)
	if !ok {
		return WrenComputer{}, fmt.Errorf("computer provisioning is unavailable")
	}
	if err := h.service.Store.ValidateWrenLifecycleConversation(ctx, conversation, ""); err != nil {
		return WrenComputer{}, err
	}
	kind, name, owner, err := h.service.Store.computerScope(ctx, workspace, account, conversation, true)
	if err != nil {
		return WrenComputer{}, err
	}
	// The workspace row serializes grants and active-computer capacity.
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return WrenComputer{}, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 AND disabled_at IS NULL FOR KEY SHARE`, account).Scan(&locked); err != nil {
		return WrenComputer{}, ErrForbidden
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		return WrenComputer{}, err
	}
	if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return WrenComputer{}, err
	}
	kind, name, owner, err = h.service.Store.computerScope(ctx, workspace, account, conversation, true)
	if err != nil {
		return WrenComputer{}, err
	}
	existing, err := scanWrenComputer(tx.QueryRow(ctx, `SELECT `+computerColumns+` FROM wren_computers WHERE conversation_id=$1 AND phase<>'deleted'`, conversation))
	if err == nil {
		if restoreID != "" && existing.RestoreCheckpointID != restoreID {
			return WrenComputer{}, fmt.Errorf("%w: stop the existing computer before restoring saved files", ErrConflict)
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return WrenComputer{}, err
	}
	if restoreID != "" {
		var valid bool
		if err = tx.QueryRow(ctx, `SELECT true FROM wren_computer_checkpoints WHERE id=$1 AND workspace_id=$2 AND conversation_id=$3 AND scope_kind=$4 AND state IN ('ready','restoring') FOR UPDATE`, restoreID, workspace, conversation, kind).Scan(&valid); err != nil || !valid {
			return WrenComputer{}, ErrForbidden
		}
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM wren_computers WHERE workspace_id=$1 AND phase<>'deleted'`, workspace).Scan(&count); err != nil {
		return WrenComputer{}, err
	}
	if count >= cfg.maximum() {
		return WrenComputer{}, fmt.Errorf("%w: this workspace already has its allowed computers", ErrCapacity)
	}
	id, err := newID("computer_")
	if err != nil {
		return WrenComputer{}, err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return WrenComputer{}, err
	}
	defer clear(private)
	publicSSH, err := ssh.NewPublicKey(public)
	if err != nil {
		return WrenComputer{}, err
	}
	publicKey := string(ssh.MarshalAuthorizedKey(publicSSH))
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return WrenComputer{}, err
	}
	defer clear(privateDER)
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	defer clear(privatePEM)
	version, sealed, err := h.config.Secrets.seal(privatePEM, secretBinding(workspace, id, "wren-computer-ssh"))
	if err != nil {
		return WrenComputer{}, err
	}
	bootstrap, err := wrencomputer.Bootstrap(wrencomputer.BootstrapConfig{SSHPublicKey: publicKey, SSHSourceCIDR: cfg.SSHSourceCIDR, Desktop: true})
	if err != nil {
		return WrenComputer{}, err
	}
	request := sdk.VPSRequest{Name: "wren-computer", VCPUs: 2, MemoryMiB: 4096, DiskGiB: 40, SSHPublicKey: publicKey, SSHCIDR: cfg.SSHSourceCIDR, ForSeconds: int(cfg.lifetime().Seconds())}
	plan, err := engine.PlanVPS(ctx, workspace, id, request)
	if err != nil {
		return WrenComputer{}, fmt.Errorf("no computer allocation is currently available")
	}
	if plan.Shape.VCPU < 2 || plan.Shape.VCPU > 2 || plan.Shape.Memory < 4096 || plan.Shape.Memory > 4096 || plan.Shape.GB < 40 || plan.Shape.GB > 80 || plan.Shape.ID == "" || plan.ImageID == "" || len(plan.Networks) == 0 {
		return WrenComputer{}, fmt.Errorf("computer allocation exceeded its authorized bounds")
	}
	plan.Spec.Spec.Compute.Bootstrap = bootstrap
	plan.Spec.Spec.Intent = "Private Wren computer for " + id
	plan.Spec.Spec.M1.Prefix = "workspaces/" + workspace + "/wren-computers/" + id
	monthly, err := pricing.EstimateComputeMonthlyCents(plan.Shape.VCPU, plan.Shape.Memory)
	if err != nil {
		return WrenComputer{}, err
	}
	ceiling := cfg.MaxMonthlyCents
	if ceiling < 1 {
		ceiling = 10000
	}
	if monthly > ceiling {
		return WrenComputer{}, fmt.Errorf("%w: computer allocation exceeds the workspace price limit", ErrCapacity)
	}
	now := h.service.Store.now()
	expires := now.Add(cfg.lifetime())
	estimate := int((monthly*int64(cfg.lifetime().Seconds()) + 720*3600 - 1) / (720 * 3600))
	reservation := max(estimate, 1)
	var limit, reserved, spent int
	if err = tx.QueryRow(ctx, `SELECT limit_cents,reserved_cents,spent_cents FROM workspace_usage_caps WHERE workspace_id=$1 FOR UPDATE`, workspace).Scan(&limit, &reserved, &spent); err != nil {
		return WrenComputer{}, err
	}
	if spent+reserved+reservation > limit {
		return WrenComputer{}, fmt.Errorf("%w: workspace compute allowance is exhausted", ErrCapacity)
	}
	rid, err := newID("usrv_")
	if err != nil {
		return WrenComputer{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO usage_reservations(id,workspace_id,subject_kind,subject_id,amount_cents,phase,created_at,updated_at) VALUES($1,$2,'wren_computer',$3,$4,'reserved',$5,$5)`, rid, workspace, id, reservation, now); err != nil {
		return WrenComputer{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE workspace_usage_caps SET reserved_cents=reserved_cents+$2,updated_at=$3 WHERE workspace_id=$1`, workspace, reservation, now); err != nil {
		return WrenComputer{}, err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return WrenComputer{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO wren_computers(id,workspace_id,conversation_id,owner_id,scope_kind,name,phase,plan,ssh_key_version,ssh_key_ciphertext,monthly_cents,created_at,updated_at,expires_at,restore_checkpoint_id) VALUES($1,$2,$3,$4,$5,$6,'queued',$7,$8,$9,$10,$11,$11,$12,NULLIF($13,''))`, id, workspace, conversation, owner, kind, name, raw, version, sealed, monthly, now, expires, restoreID); err != nil {
		return WrenComputer{}, err
	}
	if restoreID != "" {
		if _, err = tx.Exec(ctx, `UPDATE wren_computer_checkpoints SET state='restoring',updated_at=now(),next_attempt_at=now()+interval '30 seconds' WHERE id=$1`, restoreID); err != nil {
			return WrenComputer{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return WrenComputer{}, err
	}
	_ = h.service.Store.Audit(ctx, workspace, sdk.ActorRef{Kind: "human", ID: account}, "wren-computer.enabled", id, map[string]any{"conversationId": conversation, "scope": kind, "monthlyCents": monthly, "expiresAt": expires})
	return h.service.Store.wrenComputerPrivate(ctx, workspace, id)
}
func (h *HTTPServer) ClientForConversation(ctx context.Context, c Conversation) (*wrencomputer.Client, WrenComputer, error) {
	if _, _, _, err := h.service.Store.computerScope(ctx, c.WorkspaceID, c.AccountID, c.ID, false); err != nil {
		return nil, WrenComputer{}, err
	}
	v, err := scanWrenComputer(h.service.Store.pool.QueryRow(ctx, `SELECT `+computerColumns+` FROM wren_computers WHERE workspace_id=$1 AND conversation_id=$2 AND phase<>'deleted'`, c.WorkspaceID, c.ID))
	if err != nil {
		return nil, v, err
	}
	if v.Phase != "ready" || v.DeleteRequested || !v.ExpiresAt.After(h.service.Store.now()) {
		return nil, v, fmt.Errorf("%w: this computer is not ready", ErrConflict)
	}
	key, err := h.config.Secrets.open(v.KeyVersion, v.KeyCiphertext, secretBinding(v.WorkspaceID, v.ID, "wren-computer-ssh"))
	if err != nil {
		return nil, v, err
	}
	defer clear(key)
	client, err := h.computerClients.Get(wrencomputer.Config{Address: v.Address, PrivateKeyPEM: key, HostKeySHA256: v.HostFingerprint})
	if err != nil {
		return nil, v, err
	}
	_, err = h.service.Store.pool.Exec(ctx, `UPDATE wren_computers SET last_used_at=now() WHERE id=$1`, v.ID)
	return client, v, err
}

func (h *HTTPServer) workspaceComputers(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	account := p.Account.ID
	if len(parts) > 0 && parts[0] == "checkpoints" {
		h.workspaceComputerCheckpoints(w, r, p, workspace, parts[1:])
		return
	}
	if len(parts) == 0 && r.Method == http.MethodGet {
		items, err := h.service.Store.WrenComputers(r.Context(), workspace, account)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"computers": items, "enabled": h.config.Computers.allows(workspace) && h.config.Secrets != nil, "suspendSupported": false})
		return
	}
	if ((len(parts) == 0) || (len(parts) == 1 && parts[0] == "enable")) && r.Method == http.MethodPost {
		if err := h.allowWorkspace(r, p, workspace, true); err != nil {
			writeStoreError(w, err)
			return
		}
		var input struct {
			ConversationID string `json:"conversationId"`
		}
		if !decodeLimit(w, r, &input, 4096) {
			return
		}
		v, err := h.EnableWrenComputer(r.Context(), workspace, account, input.ConversationID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, v)
		return
	}
	if len(parts) == 0 {
		methodNotAllowed(w)
		return
	}
	v, err := h.service.Store.WrenComputer(r.Context(), workspace, account, parts[0])
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(parts) == 2 && (parts[1] == "checkpoint" || parts[1] == "sleep") && r.Method == http.MethodPost {
		var input struct {
			IncludeBrowser bool `json:"includeBrowser"`
		}
		if !decodeLimit(w, r, &input, 4096) {
			return
		}
		checkpoint, e := h.CreateWrenComputerCheckpoint(r.Context(), workspace, account, v.ID, input.IncludeBrowser, parts[1] == "sleep")
		if e != nil {
			writeStoreError(w, e)
			return
		}
		writeJSON(w, http.StatusAccepted, checkpoint)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, v)
		return
	}
	if r.Method != http.MethodGet {
		if err = h.allowWorkspace(r, p, workspace, true); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	if len(parts) == 2 && parts[1] == "delete" && r.Method == http.MethodPost {
		if _, _, _, err = h.service.Store.computerScope(r.Context(), workspace, account, v.ConversationID, true); err != nil {
			writeStoreError(w, err)
			return
		}
		_, err = h.service.Store.pool.Exec(r.Context(), `UPDATE wren_computers SET delete_requested=true,next_attempt_at=now() WHERE id=$1 AND phase<>'deleted'`, v.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), workspace, p.Actor, "wren-computer.deletion-requested", v.ID, map[string]any{})
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "deletion_requested"})
		return
	}
	if len(parts) == 2 && (parts[1] == "suspend" || parts[1] == "resume") && r.Method == http.MethodPost {
		writeError(w, http.StatusConflict, fmt.Errorf("this computer provider does not yet support verified suspend and resume"))
		return
	}
	if len(parts) == 2 && parts[1] == "control" && r.Method == http.MethodPost {
		var input struct {
			Mode              string     `json:"mode"`
			StopCommands      []string   `json:"stopCommands"`
			StopCodingJobs    []string   `json:"stopCodingJobs"`
			ExpectedExpiresAt *time.Time `json:"expectedExpiresAt"`
		}
		if !decodeLimit(w, r, &input, 8192) {
			return
		}
		if len(input.StopCommands)+len(input.StopCodingJobs) > 64 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("too many commands"))
			return
		}
		expected := []time.Time{}
		if input.ExpectedExpiresAt != nil {
			expected = append(expected, *input.ExpectedExpiresAt)
		}
		controlled, e := h.setComputerControl(r.Context(), workspace, account, v.ID, input.Mode, append(input.StopCommands, input.StopCodingJobs...), expected...)
		if e != nil {
			var running *computerCommandsRunning
			if errors.As(e, &running) {
				writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{"message": running.Error(), "code": "computer_work_running", "details": map[string]any{"commandIds": running.IDs, "codingJobs": running.CodingJobs}}})
				return
			}
			writeStoreError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, controlled)
		return
	}
	c, err := h.service.Store.Conversation(r.Context(), workspace, account, v.ConversationID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var operation *computerOperation
	if r.Method != http.MethodGet {
		operation, err = h.service.Store.beginComputerOperation(r.Context(), v.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer operation.close()
		r = r.WithContext(operation.ctx)
	}
	if r.Method != http.MethodGet && !(len(parts) == 4 && parts[1] == "jobs" && parts[3] == "stop") {
		if e := h.computerHumanMutationAllowed(r.Context(), v, account); e != nil {
			writeStoreError(w, e)
			return
		}
	}
	if len(parts) == 2 && (parts[1] == "files" || parts[1] == "file") && r.Method == http.MethodGet && !ComputerUserFileAllowed(r.URL.Query().Get("path")) {
		writeStoreError(w, ErrForbidden)
		return
	}
	client, _, err := h.ClientForConversation(r.Context(), c)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(parts) == 2 && parts[1] == "probe" && r.Method == http.MethodGet {
		result, e := client.Probe(r.Context())
		if e != nil {
			writeStoreError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	if len(parts) == 2 && parts[1] == "screen" && r.Method == http.MethodGet {
		h.streamComputerScreen(w, r, client, v, account)
		return
	}
	if len(parts) == 2 && parts[1] == "screenshot" && r.Method == http.MethodGet {
		png, e := client.Screenshot(r.Context())
		if e != nil {
			writeStoreError(w, e)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(png)
		return
	}
	if len(parts) == 2 && parts[1] == "files" && r.Method == http.MethodGet {
		entries, e := client.ListFiles(r.Context(), r.URL.Query().Get("path"))
		if e != nil {
			writeStoreError(w, e)
			return
		}
		visible := entries[:0]
		for _, entry := range entries {
			base := r.URL.Query().Get("path")
			if base == "" {
				base = "/workspace"
			}
			if ComputerUserFileAllowed(strings.TrimRight(base, "/") + "/" + entry.Name) {
				visible = append(visible, entry)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"files": visible})
		return
	}
	if len(parts) == 2 && parts[1] == "file" && r.Method == http.MethodGet {
		offset, e := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		if r.URL.Query().Get("offset") == "" {
			offset, e = 0, nil
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit == 0 {
			limit = wrencomputer.MaxFileBytes
		}
		if e != nil || offset < 0 || limit < 1 || limit > wrencomputer.MaxFileBytes {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid file range"))
			return
		}
		file, e := client.ReadFile(r.Context(), r.URL.Query().Get("path"), offset, limit)
		if e != nil {
			writeStoreError(w, e)
			return
		}
		if !ComputerUserFileAllowed(file.Path) {
			writeStoreError(w, ErrForbidden)
			return
		}
		writeJSON(w, http.StatusOK, file)
		return
	}
	if len(parts) == 2 && parts[1] == "file" && r.Method == http.MethodPut {
		var input struct {
			Path string `json:"path"`
			Data []byte `json:"data"`
		}
		if !decodeLimit(w, r, &input, 12<<20) {
			return
		}
		if !ComputerUserFileAllowed(input.Path) {
			writeStoreError(w, ErrForbidden)
			return
		}
		var file any
		e := operation.run(func(ctx context.Context) error {
			var e error
			file, e = client.WriteFile(ctx, input.Path, input.Data)
			return e
		})
		if e != nil {
			writeStoreError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, file)
		return
	}
	if len(parts) == 2 && parts[1] == "input" && r.Method == http.MethodPost {
		var input struct {
			wrencomputer.DesktopInput
			ExpectedExpiresAt *time.Time `json:"expectedExpiresAt"`
		}
		if !decodeLimit(w, r, &input, 48<<10) {
			return
		}
		expected := []time.Time{}
		if input.ExpectedExpiresAt != nil {
			expected = append(expected, *input.ExpectedExpiresAt)
		}
		if e := h.requireHumanComputerControl(r.Context(), v, account, expected...); e != nil {
			writeStoreError(w, e)
			return
		}
		if e := operation.run(func(ctx context.Context) error { return client.Input(ctx, input.DesktopInput) }); e != nil {
			writeStoreError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"accepted": true})
		return
	}
	if len(parts) == 2 && (parts[1] == "commands" || parts[1] == "jobs") && r.Method == http.MethodPost {
		var input wrencomputer.JobRequest
		if !decodeLimit(w, r, &input, 80<<10) {
			return
		}
		var job any
		e := operation.run(func(ctx context.Context) error {
			var e error
			job, e = h.service.Store.startComputerCommand(ctx, client, v.ID, input)
			return e
		})
		if e != nil {
			writeStoreError(w, e)
			return
		}
		writeJSON(w, http.StatusAccepted, job)
		return
	}
	if len(parts) == 3 && parts[1] == "jobs" && r.Method == http.MethodGet {
		job, e := client.Job(r.Context(), parts[2])
		if e == nil {
			e = h.service.Store.recordComputerCommand(r.Context(), v.ID, job)
		}
		if e != nil {
			writeStoreError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	}
	if len(parts) == 4 && parts[1] == "jobs" && parts[3] == "stop" && r.Method == http.MethodPost {
		if e := h.computerControlOwnerAllowed(r.Context(), v, account); e != nil {
			writeStoreError(w, e)
			return
		}
		var job wrencomputer.Job
		e := operation.run(func(ctx context.Context) error { var e error; job, e = client.StopJob(ctx, parts[2]); return e })
		if e == nil {
			e = h.service.Store.recordComputerCommand(r.Context(), v.ID, job)
		}
		if e != nil {
			writeStoreError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	}
	writeStoreError(w, ErrNotFound)
}

// WrenComputerDispatcher resumes provider intents. SDK durable ownership and
// reconciliation fence actual resources; an advisory lock serializes DB workers.
type WrenComputerDispatcher struct{ Server *HTTPServer }

func (d *WrenComputerDispatcher) Run(ctx context.Context) error {
	if d.Server == nil {
		return fmt.Errorf("computer dispatcher has no server")
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	slots := make(chan struct{}, 2)
	checkpointSlots := make(chan struct{}, 1)
	for {
		select {
		case checkpointSlots <- struct{}{}:
			go func() { defer func() { <-checkpointSlots }(); _ = d.reconcileCheckpoints(ctx) }()
		default:
		}
		rows, err := d.Server.service.Store.pool.Query(ctx, `SELECT workspace_id,id FROM wren_computers WHERE phase NOT IN ('deleted','failed') AND next_attempt_at<=now() ORDER BY next_attempt_at LIMIT 30`)
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

// Error details may contain signed URLs, bootstrap output, or provider payloads.
// Persist only known stage labels, never the original error text.
func computerSetupFailure(err error, phase string) string {
	stage := "provider setup"
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "boot proof"):
		stage = "waiting for the trusted boot receipt"
	case strings.Contains(text, "bootstrap failed"):
		stage = "guest bootstrap"
	case strings.Contains(text, "wait for compute"):
		stage = "waiting for the provider VM"
	case strings.Contains(text, "public endpoint") || strings.Contains(text, "exposure") || strings.Contains(text, "security"):
		stage = "restricted SSH network setup"
	case strings.Contains(text, "state") || strings.Contains(text, "intent") || strings.Contains(text, "fence"):
		stage = "durable provider state reconciliation"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		stage += " (setup deadline reached)"
	}
	if phase == "creating" || phase == "ready" || phase == "escalated" {
		stage += "; allocation phase " + phase
	}
	return "Computer setup failed during " + stage + ". Canter is cleaning up the allocation."
}

func (d *WrenComputerDispatcher) Reconcile(ctx context.Context, workspace, id string) error {
	h := d.Server
	s := h.service.Store
	engine, ok := h.service.Engine.(VPSEngine)
	if !ok {
		return fmt.Errorf("computer provider unavailable")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,938))`, id).Scan(&locked); err != nil || !locked {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1,938))`, id)
	v, err := s.wrenComputerPrivate(ctx, workspace, id)
	if err != nil {
		return err
	}
	if v.Phase == "deleted" {
		return nil
	}
	_, _, _, scopeErr := s.computerScope(ctx, v.WorkspaceID, v.OwnerID, v.ConversationID, true)
	if v.DeleteRequested || !v.ExpiresAt.After(s.now()) || scopeErr != nil || !h.config.Computers.allows(workspace) {
		v.Phase = "deleting"
	}
	if v.Phase == "deleting" {
		if err = d.save(ctx, v); err != nil {
			return err
		}
		operation, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		state, e := engine.DestroyVPS(operation, v.Plan)
		if e != nil || state.Phase != "destroyed" {
			pendingNote := "Deletion is pending. Canter will retry until provider removal is verified."
			if v.Failure == "" {
				v.Failure = pendingNote
			} else if !strings.Contains(v.Failure, pendingNote) {
				v.Failure += " " + pendingNote
			}
			_ = d.save(context.WithoutCancel(ctx), v)
			if e != nil {
				return e
			}
			return ErrConflict
		}
		if err = d.releaseReservation(ctx, v); err != nil {
			return err
		}
		v.Phase = "deleted"
		v.Address = ""
		v.HostFingerprint = ""
		v.KeyCiphertext = []byte{}
		pendingNote := "Deletion is pending. Canter will retry until provider removal is verified."
		if v.Failure == pendingNote {
			v.Failure = ""
		} else {
			v.Failure = strings.TrimSpace(strings.TrimSuffix(v.Failure, " "+pendingNote))
			if v.Failure != "" && !strings.Contains(v.Failure, "Allocated resources were removed.") {
				v.Failure += " Allocated resources were removed."
			}
		}
		if err = d.save(ctx, v); err != nil {
			return err
		}
		_ = s.Audit(ctx, workspace, sdk.ActorRef{Kind: "system", ID: "wren-computer-dispatcher"}, "wren-computer.deleted", id, map[string]any{})
		return nil
	}
	if v.Phase == "failed" {
		return nil
	}
	if v.Phase == "ready" {
		return d.save(ctx, v)
	}
	v.Phase = "provisioning"
	if err = d.save(ctx, v); err != nil {
		return err
	}
	deadline := minTime(v.CreatedAt.Add(15*time.Minute), v.ExpiresAt)
	operation, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	state, applyErr := engine.ApplyVPS(operation, v.Plan)
	if applyErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		v.Phase = "deleting"
		v.Failure = computerSetupFailure(applyErr, state.Phase)
		_ = d.save(ctx, v)
		return applyErr
	}
	if state.Phase != "ready" || len(state.Resources) != 1 || state.Resources[0].Address == "" {
		v.Phase = "deleting"
		v.Failure = "Computer access could not be verified; cleaning up."
		return d.save(ctx, v)
	}
	fingerprint, e := engine.VPSHostFingerprint(operation, v.Plan)
	if e != nil || !strings.HasPrefix(fingerprint, "SHA256:") {
		v.Phase = "deleting"
		v.Failure = "Computer identity could not be verified; cleaning up."
		return d.save(ctx, v)
	}
	v.Address, v.HostFingerprint = state.Resources[0].Address, fingerprint
	// The provider saying ACTIVE does not prove the guest desktop/agent works.
	key, e := h.config.Secrets.open(v.KeyVersion, v.KeyCiphertext, secretBinding(workspace, id, "wren-computer-ssh"))
	if e != nil {
		v.Phase = "deleting"
		v.Failure = "Computer key could not be restored; cleaning up."
		return d.save(ctx, v)
	}
	client, e := wrencomputer.NewSSH(wrencomputer.Config{Address: v.Address, PrivateKeyPEM: key, HostKeySHA256: v.HostFingerprint})
	clear(key)
	if e != nil {
		return e
	}
	probeCtx, probeCancel := context.WithTimeout(operation, 30*time.Second)
	capabilities, e := client.Probe(probeCtx)
	probeCancel()
	if e != nil || !capabilities.Desktop || !capabilities.Browser || !capabilities.PersistentFiles || !capabilities.DurableJobs {
		if !s.now().Before(deadline) {
			v.Phase = "deleting"
			v.Failure = "The computer desktop did not become ready; cleaning up."
		} else {
			v.Failure = "The VM is running; waiting for its desktop and tools."
		}
		_ = d.save(ctx, v)
		return e
	}
	if v.RestoreCheckpointID != "" {
		if e = h.restoreWrenComputerCheckpoint(operation, v, client); e != nil {
			v.Phase = "deleting"
			v.Failure = "Saved files could not be restored; cleaning up the new allocation. The encrypted checkpoint was retained."
			_ = d.save(ctx, v)
			return e
		}
	}
	v.Phase = "ready"
	v.Failure = ""
	if err = d.save(ctx, v); err != nil {
		return err
	}
	_ = s.Audit(ctx, workspace, sdk.ActorRef{Kind: "system", ID: "wren-computer-dispatcher"}, "wren-computer.ready", id, map[string]any{"scope": v.ScopeKind})
	return nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (d *WrenComputerDispatcher) save(ctx context.Context, v WrenComputer) error {
	next := d.Server.service.Store.now().Add(10 * time.Second)
	if v.Phase == "ready" {
		next = minTime(d.Server.service.Store.now().Add(time.Minute), v.ExpiresAt)
	}
	result, err := d.Server.service.Store.pool.Exec(ctx, `UPDATE wren_computers SET phase=CASE WHEN delete_requested AND $2<>'deleted' THEN 'deleting' ELSE $2 END,address=$3,host_fingerprint=$4,ssh_key_ciphertext=$5,failure=$6,updated_at=now(),next_attempt_at=CASE WHEN delete_requested AND $2 NOT IN ('deleting','deleted') THEN now() ELSE $7 END WHERE id=$1 AND phase<>'deleted' AND (phase<>'deleting' OR $2 IN ('deleting','deleted'))`, v.ID, v.Phase, v.Address, v.HostFingerprint, v.KeyCiphertext, v.Failure, next)
	if err == nil && result.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}
func (d *WrenComputerDispatcher) releaseReservation(ctx context.Context, v WrenComputer) error {
	s := d.Server.service.Store
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var amount int
	err = tx.QueryRow(ctx, `UPDATE usage_reservations SET phase='released',updated_at=now() WHERE workspace_id=$1 AND subject_kind='wren_computer' AND subject_id=$2 AND phase='reserved' RETURNING amount_cents`, v.WorkspaceID, v.ID).Scan(&amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE workspace_usage_caps SET reserved_cents=greatest(0,reserved_cents-$2),updated_at=now() WHERE workspace_id=$1`, v.WorkspaceID, amount); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) WrenComputerPlans(ctx context.Context, workspace string) ([]sdk.VPSPlan, error) {
	rows, err := s.pool.Query(ctx, `SELECT plan FROM wren_computers WHERE workspace_id=$1`, workspace)
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

// SetComputerControl is a human-account lease. It expires after disconnection;
// a model/installation principal never calls this account-only HTTP operation.
func (h *HTTPServer) SetComputerControl(ctx context.Context, workspace, account, id, mode string, expected ...time.Time) (WrenComputer, error) {
	return h.setComputerControl(ctx, workspace, account, id, mode, nil, expected...)
}
func (h *HTTPServer) setComputerControl(ctx context.Context, workspace, account, id, mode string, approved []string, expected ...time.Time) (WrenComputer, error) {
	if mode != "human" && mode != "agent" {
		return WrenComputer{}, fmt.Errorf("choose human or agent control")
	}
	v, err := h.service.Store.WrenComputer(ctx, workspace, account, id)
	if err != nil {
		return v, err
	}
	role, err := h.service.Store.Membership(ctx, account, workspace)
	if err != nil || role == "viewer" {
		return v, ErrForbidden
	}
	if v.Phase != "ready" || v.DeleteRequested || !v.ExpiresAt.After(h.service.Store.now()) {
		return v, ErrConflict
	}
	operation, err := h.service.Store.beginComputerOperation(ctx, id)
	if err != nil {
		return v, err
	}
	defer operation.close()
	ctx = operation.ctx
	// Access and lifecycle may have changed while a previous action drained.
	if v, err = h.service.Store.WrenComputer(ctx, workspace, account, id); err != nil {
		return v, err
	}
	if role, err = h.service.Store.Membership(ctx, account, workspace); err != nil || role == "viewer" {
		return v, ErrForbidden
	}

	if h.activeComputerHumanControl(ctx, v) && v.ControlAccountID != account {
		return v, fmt.Errorf("%w: another person is controlling this computer", ErrConflict)
	}
	if len(expected) > 0 && (v.ControlAccountID != account || v.ControlExpiresAt == nil || !v.ControlExpiresAt.Equal(expected[0])) {
		return v, fmt.Errorf("%w: computer control changed; refresh its state", ErrConflict)
	}
	if mode == "human" && !h.activeComputerHumanControl(ctx, v) {
		coding, e := h.service.Store.pendingComputerCodingWork(ctx, id)
		if e != nil {
			return v, e
		}
		ids, e := h.service.Store.pendingComputerCommands(ctx, id)
		if e != nil {
			return v, e
		}
		if len(ids)+len(coding) > 64 {
			return v, fmt.Errorf("%w: too much work to hand over safely; stop current work first", ErrConflict)
		}
		c, e := h.service.Store.Conversation(ctx, workspace, account, v.ConversationID)
		if e != nil {
			return v, e
		}
		var client *wrencomputer.Client
		live := []string{}
		if len(ids) > 0 {
			client, _, e = h.ClientForConversation(ctx, c)
			if e != nil {
				return v, e
			}
			// Read-only reconciliation first. No command is stopped until the
			// caller has reviewed both commands and coding workers together.
			e = operation.run(func(ctx context.Context) error {
				return h.service.Store.settleComputerCommands(ctx, client, id, ids, nil)
			})
			var running *computerCommandsRunning
			if errors.As(e, &running) {
				live = running.IDs
				operation.uncertain = false
			} else if e != nil {
				return v, e
			}
		}
		needsReview := false
		for _, command := range live {
			if !slices.Contains(approved, command) {
				needsReview = true
			}
		}
		for _, job := range coding {
			if !slices.Contains(approved, job.ID) {
				needsReview = true
			}
		}
		if needsReview {
			return v, &computerCommandsRunning{IDs: live, CodingJobs: coding}
		}
		if len(coding) > 0 {
			if e = h.service.Store.requestComputerCodingStops(ctx, c, id, approved); e != nil {
				return v, e
			}
		}
		if len(live) > 0 {
			if e = operation.run(func(ctx context.Context) error {
				return h.service.Store.settleComputerCommands(ctx, client, id, live, approved)
			}); e != nil {
				var running *computerCommandsRunning
				if errors.As(e, &running) {
					operation.uncertain = false
					running.CodingJobs = coding
				}
				return v, e
			}
		}
		if e = h.service.Store.waitComputerCodingStops(ctx, id, coding); e != nil {
			return v, e
		}
	}

	// A remote command check may take time. Revalidate the caller before
	// recording ownership; the UI must not turn a revoked grant into control.
	if _, err = h.service.Store.WrenComputer(ctx, workspace, account, id); err != nil {
		return v, err
	}
	if role, err = h.service.Store.Membership(ctx, account, workspace); err != nil || role == "viewer" {
		return v, ErrForbidden
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return v, err
	}
	defer tx.Rollback(ctx)
	v, err = scanWrenComputer(tx.QueryRow(ctx, `SELECT `+computerColumns+` FROM wren_computers WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return v, err
	}
	if v.Phase != "ready" || v.DeleteRequested || !v.ExpiresAt.After(h.service.Store.now()) {
		return v, ErrConflict
	}
	if len(expected) > 0 && (v.ControlAccountID != account || v.ControlExpiresAt == nil || !v.ControlExpiresAt.Equal(expected[0]) || !v.ControlExpiresAt.After(h.service.Store.now())) {
		return v, fmt.Errorf("%w: computer control changed; refresh its state", ErrConflict)
	}
	active := h.activeComputerHumanControl(ctx, v)
	if active && v.ControlAccountID != account {
		return v, fmt.Errorf("%w: another person is controlling this computer", ErrConflict)
	}
	if mode == "human" {
		if saving, e := activeComputerCheckpoint(ctx, tx, id); e != nil {
			return v, e
		} else if saving {
			return v, fmt.Errorf("%w: this computer is saving its workspace", ErrConflict)
		}
	}
	if mode == "human" {
		busy, e := activeComputerACP(ctx, tx, id, "")
		if e != nil {
			return v, e
		}
		if busy {
			return v, fmt.Errorf("%w: stop the active coding worker before taking control; its running shell cannot be paused safely", ErrConflict)
		}
	}
	if mode == "human" {
		_, err = tx.Exec(ctx, `UPDATE wren_computers SET control_account_id=$2,control_expires_at=now()+interval '60 seconds',mutation_run_id=NULL,mutation_lease_token=NULL,mutation_expires_at=NULL,updated_at=now() WHERE id=$1`, id, account)
	} else {
		_, err = tx.Exec(ctx, `UPDATE wren_computers SET control_account_id=NULL,control_expires_at=NULL,updated_at=now() WHERE id=$1`, id)
	}
	if err != nil {
		return v, err
	}
	if err = tx.Commit(ctx); err != nil {
		return v, err
	}
	return h.service.Store.WrenComputer(ctx, workspace, account, id)
}
func (h *HTTPServer) activeComputerHumanControl(ctx context.Context, v WrenComputer) bool {
	if v.ControlAccountID == "" || v.ControlExpiresAt == nil || !v.ControlExpiresAt.After(h.service.Store.now()) {
		return false
	}
	if _, _, _, err := h.service.Store.computerScope(ctx, v.WorkspaceID, v.ControlAccountID, v.ConversationID, false); err != nil {
		return false
	}
	role, err := h.service.Store.Membership(ctx, v.ControlAccountID, v.WorkspaceID)
	return err == nil && role != "viewer"
}
func (h *HTTPServer) requireHumanComputerControl(ctx context.Context, v WrenComputer, account string, expected ...time.Time) error {
	current, err := h.service.Store.wrenComputerPrivate(ctx, v.WorkspaceID, v.ID)
	if err != nil {
		return err
	}
	if len(expected) > 0 && (current.ControlExpiresAt == nil || !current.ControlExpiresAt.Equal(expected[0])) {
		return fmt.Errorf("%w: computer control changed; take control again", ErrConflict)
	}
	if !h.activeComputerHumanControl(ctx, current) || current.ControlAccountID != account {
		return fmt.Errorf("%w: take control of this computer before sending input", ErrConflict)
	}
	return nil
}

// ComputerAgentWriteAllowed prevents new bot mutations during human takeover.
// Reads and screenshots remain available. An already-started detached command
// retains its own explicit stop handle and must not be presented as stopped.
func (h *HTTPServer) ComputerAgentWriteAllowed(ctx context.Context, c Conversation) error {
	if err := h.service.Store.ValidateWrenLifecycleConversation(ctx, c.ID, ""); err != nil {
		return err
	}
	if _, _, _, err := h.service.Store.computerScope(ctx, c.WorkspaceID, c.AccountID, c.ID, false); err != nil {
		return err
	}
	role, err := h.service.Store.Membership(ctx, c.AccountID, c.WorkspaceID)
	if err != nil || role == "viewer" {
		return ErrForbidden
	}
	v, err := scanWrenComputer(h.service.Store.pool.QueryRow(ctx, `SELECT `+computerColumns+` FROM wren_computers WHERE workspace_id=$1 AND conversation_id=$2 AND phase<>'deleted'`, c.WorkspaceID, c.ID))
	if err != nil {
		return err
	}
	if h.activeComputerHumanControl(ctx, v) {
		return errComputerHumanControl
	}
	if saving, e := activeComputerCheckpoint(ctx, h.service.Store.pool, v.ID); e != nil {
		return e
	} else if saving {
		return fmt.Errorf("%w: this computer is saving its workspace", ErrConflict)
	}
	return nil
}
