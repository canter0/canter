package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/canter0/canter/pricing"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

type VPSEngine interface {
	PlanVPS(context.Context, string, string, sdk.VPSRequest) (sdk.VPSPlan, error)
	ApplyVPS(context.Context, sdk.VPSPlan) (sdk.State, error)
	VPSHostFingerprint(context.Context, sdk.VPSPlan) (string, error)
	DestroyVPS(context.Context, sdk.VPSPlan) (sdk.State, error)
	VPSBillingResources(context.Context, sdk.VPSPlan) ([]sdk.BillingResource, error)
}

type VPS struct {
	MonthlyCents       int64          `json:"monthlyCents"`
	ProvisioningFailed bool           `json:"provisioningFailed,omitempty"`
	DeleteRequested    bool           `json:"deleteRequested"`
	ID                 string         `json:"id"`
	WorkspaceID        string         `json:"workspaceId"`
	Request            sdk.VPSRequest `json:"request"`
	Plan               sdk.VPSPlan    `json:"plan"`
	Digest             string         `json:"digest"`
	Phase              string         `json:"phase"`
	CreatedAt          time.Time      `json:"createdAt"`
	ApprovedAt         *time.Time     `json:"approvedAt,omitempty"`
	ApprovedBy         string         `json:"approvedBy,omitempty"`
	ExpiresAt          *time.Time     `json:"expiresAt,omitempty"`
	Address            string         `json:"address,omitempty"`
	HostFingerprint    string         `json:"hostFingerprint,omitempty"`
	Failure            string         `json:"failure,omitempty"`
}

func vpsDigest(v VPS) string {
	raw, _ := json.Marshal(struct {
		Workspace    string
		ID           string
		Request      sdk.VPSRequest
		Plan         sdk.VPSPlan
		MonthlyCents int64
	}{v.WorkspaceID, v.ID, v.Request, v.Plan, v.MonthlyCents})
	return fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
}
func publicVPS(v VPS) map[string]any {
	monthly := v.MonthlyCents
	if monthly == 0 {
		monthly, _ = pricing.EstimateComputeMonthlyCents(v.Plan.Shape.VCPU, v.Plan.Shape.Memory)
	}
	result := map[string]any{"deleteRequested": v.DeleteRequested, "id": v.ID, "name": v.Request.Name, "phase": v.Phase, "digest": v.Digest, "vcpus": v.Plan.Shape.VCPU, "memoryMiB": v.Plan.Shape.Memory, "diskGiB": v.Plan.Shape.GB, "os": "Ubuntu 24.04 LTS", "monthlyCents": monthly, "forSeconds": v.Request.ForSeconds, "sshCidr": v.Request.SSHCIDR, "sshPublicKey": v.Request.SSHPublicKey, "createdAt": v.CreatedAt, "expiresAt": v.ExpiresAt, "address": v.Address, "hostFingerprint": v.HostFingerprint, "failure": v.Failure, "username": "canter"}
	if v.Request.ForSeconds > 0 {
		result["estimatedCents"] = float64(monthly) * float64(v.Request.ForSeconds) / (720 * 3600)
	}
	return result
}

func (s *Service) DraftVPS(ctx context.Context, workspace string, input sdk.VPSRequest, actor sdk.ActorRef) (VPS, error) {
	if err := input.Validate(); err != nil {
		return VPS{}, err
	}
	engine, ok := s.Engine.(VPSEngine)
	if !ok {
		return VPS{}, fmt.Errorf("Canter VM provisioning is unavailable on this server")
	}
	id, err := newID("vps_")
	if err != nil {
		return VPS{}, err
	}
	plan, err := engine.PlanVPS(ctx, workspace, id, input)
	if err != nil {
		return VPS{}, fmt.Errorf("Canter could not find an available VM for this plan; retry or adjust the sizing")
	}
	if plan.Shape.VCPU < input.VCPUs || plan.Shape.Memory < input.MemoryMiB || plan.Shape.GB < input.DiskGiB || plan.Shape.VCPU > 32 || plan.Shape.Memory > 131072 || plan.Shape.GB > 2048 || len(plan.Networks) == 0 || plan.ImageID == "" || plan.Shape.ID == "" {
		return VPS{}, fmt.Errorf("available VM allocation is outside supported bounds")
	}
	v := VPS{ID: id, WorkspaceID: workspace, Request: input, Plan: plan, Phase: "drafted", CreatedAt: s.Store.now()}
	v.MonthlyCents, err = pricing.EstimateComputeMonthlyCents(plan.Shape.VCPU, plan.Shape.Memory)
	if err != nil {
		return VPS{}, err
	}
	v.Digest = vpsDigest(v)
	raw, err := json.Marshal(v)
	if err != nil {
		return VPS{}, err
	}
	_, err = s.Store.pool.Exec(ctx, `INSERT INTO workspace_vps(id,workspace_id,phase,document,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)`, id, workspace, v.Phase, raw, v.CreatedAt)
	if err == nil {
		_ = s.Store.Audit(ctx, workspace, actor, "vps.drafted", id, map[string]any{"digest": v.Digest})
	}
	return v, err
}
func scanVPS(row pgx.Row) (VPS, error) {
	var v VPS
	var raw []byte
	err := row.Scan(&raw, &v.DeleteRequested)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err == nil {
		requested := v.DeleteRequested
		err = json.Unmarshal(raw, &v)
		v.DeleteRequested = requested
	}
	return v, err
}
func (s *Store) VPS(ctx context.Context, workspace, id string) (VPS, error) {
	return scanVPS(s.pool.QueryRow(ctx, `SELECT document,delete_requested FROM workspace_vps WHERE workspace_id=$1 AND id=$2`, workspace, id))
}
func (s *Store) ListVPS(ctx context.Context, workspace string) ([]VPS, error) {
	rows, err := s.pool.Query(ctx, `SELECT document,delete_requested FROM workspace_vps WHERE workspace_id=$1 ORDER BY created_at DESC`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VPS{}
	for rows.Next() {
		v, err := scanVPS(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) ApproveVPS(ctx context.Context, workspace, id, digest string, actor sdk.ActorRef) (VPS, error) {
	if actor.Kind != "human" {
		return VPS{}, ErrForbidden
	}
	if err := s.requireBillingPayment(ctx, workspace); err != nil {
		return VPS{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return VPS{}, err
	}
	defer tx.Rollback(ctx)
	v, err := scanVPS(tx.QueryRow(ctx, `SELECT document,delete_requested FROM workspace_vps WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, id))
	if err != nil {
		return v, err
	}
	if v.Digest != digest || v.Digest != vpsDigest(v) || digest == "" {
		return v, ErrConflict
	}
	if v.Phase != "drafted" {
		if v.ApprovedAt != nil {
			return v, nil
		}
		return v, ErrConflict
	}
	if s.now().Sub(v.CreatedAt) > 30*time.Minute {
		return v, fmt.Errorf("%w: this VM proposal expired; ask Canter to refresh it", ErrConflict)
	}
	// Serialize the existing workspace spend guard with other paid allocations.
	var limit, reserved, spent int
	if err = tx.QueryRow(ctx, `SELECT limit_cents,reserved_cents,spent_cents FROM workspace_usage_caps WHERE workspace_id=$1 FOR UPDATE`, workspace).Scan(&limit, &reserved, &spent); err != nil {
		return v, err
	}
	monthly := v.MonthlyCents
	if monthly < 1 {
		return v, ErrConflict
	}
	reservation := initialDeploymentReservationCents
	if v.Request.ForSeconds > 0 {
		reservation = max(reservation, int((monthly*int64(v.Request.ForSeconds)+720*3600-1)/(720*3600)))
	} else {
		reservation = max(reservation, int(monthly))
	}
	if reserved+spent+reservation > limit {
		return v, fmt.Errorf("%w: this workspace cannot allocate more compute", ErrCapacity)
	}
	rid, err := newID("usrv_")
	if err != nil {
		return v, err
	}
	now := s.now()
	if _, err = tx.Exec(ctx, `INSERT INTO usage_reservations(id,workspace_id,subject_kind,subject_id,amount_cents,phase,created_at,updated_at) VALUES($1,$2,'vps',$3,$4,'reserved',$5,$5)`, rid, workspace, id, reservation, now); err != nil {
		return v, err
	}
	if _, err = tx.Exec(ctx, `UPDATE workspace_usage_caps SET reserved_cents=reserved_cents+$1,updated_at=$2 WHERE workspace_id=$3`, reservation, now, workspace); err != nil {
		return v, err
	}
	v.Phase = "queued"
	v.ApprovedAt = &now
	v.ApprovedBy = actor.ID
	if v.Request.ForSeconds > 0 {
		expiry := now.Add(time.Duration(v.Request.ForSeconds) * time.Second)
		v.ExpiresAt = &expiry
	}
	raw, _ := json.Marshal(v)
	if _, err = tx.Exec(ctx, `UPDATE workspace_vps SET phase=$3,document=$4,updated_at=$5 WHERE workspace_id=$1 AND id=$2`, workspace, id, v.Phase, raw, now); err != nil {
		return v, err
	}
	if err = tx.Commit(ctx); err == nil {
		_ = s.Audit(ctx, workspace, actor, "vps.approved", id, map[string]any{"digest": digest, "expiresAt": v.ExpiresAt})
	}
	return v, err
}

func (h *HTTPServer) workspaceVPS(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if len(parts) == 0 && r.Method == http.MethodGet {
		list, err := h.service.Store.ListVPS(r.Context(), workspace)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		out := []map[string]any{}
		for _, v := range list {
			out = append(out, publicVPS(v))
		}
		writeJSON(w, http.StatusOK, map[string]any{"servers": out})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		v, err := h.service.Store.VPS(r.Context(), workspace, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, publicVPS(v))
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := h.allowWorkspace(r, p, workspace, true); err != nil {
		writeStoreError(w, err)
		return
	}
	if len(parts) == 0 {
		if p.Installation != nil && !p.Installation.Authority.Draft {
			writeStoreError(w, ErrForbidden)
			return
		}
		var in sdk.VPSRequest
		if !decode(w, r, &in) {
			return
		}
		v, err := h.service.DraftVPS(r.Context(), workspace, in, p.Actor)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, publicVPS(v))
		return
	}
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	if len(parts) == 2 && parts[1] == "approve" {
		var in struct {
			Digest string `json:"digest"`
		}
		if !decode(w, r, &in) {
			return
		}
		v, err := h.service.Store.ApproveVPS(r.Context(), workspace, parts[0], in.Digest, p.Actor)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, publicVPS(v))
		return
	}
	if len(parts) == 2 && parts[1] == "delete" {
		v, err := h.service.Store.VPS(r.Context(), workspace, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if v.Phase == "drafted" {
			writeStoreError(w, ErrConflict)
			return
		}
		_, err = h.service.Store.pool.Exec(r.Context(), `UPDATE workspace_vps SET delete_requested=true,next_attempt_at=now() WHERE workspace_id=$1 AND id=$2`, workspace, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), workspace, p.Actor, "vps.deletion-requested", v.ID, map[string]any{})
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "deletion_requested"})
		return
	}
	writeStoreError(w, ErrNotFound)
}

// VPSDispatcher resumes durable SDK intents after a crash. Expiry and deletion
// are server-owned and do not depend on the conversation or browser remaining open.
type VPSDispatcher struct {
	Store  *Store
	Engine VPSEngine
}

func (d *VPSDispatcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	slots := make(chan struct{}, 4)
	for {
		rows, err := d.Store.pool.Query(ctx, `SELECT workspace_id,id FROM workspace_vps WHERE phase NOT IN ('drafted','deleted') AND next_attempt_at<=now() ORDER BY next_attempt_at LIMIT 100`)
		if err != nil {
			return err
		}
		var items [][2]string
		for rows.Next() {
			var item [2]string
			if err = rows.Scan(&item[0], &item[1]); err != nil {
				break
			}
			items = append(items, item)
		}
		rowsErr := rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if rowsErr != nil {
			return rowsErr
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
		case <-ticker.C:
		}
	}
}
func (d *VPSDispatcher) Reconcile(ctx context.Context, workspace, id string) error {
	conn, err := d.Store.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,929))`, id).Scan(&locked); err != nil || !locked {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1,929))`, id)
	v, err := d.Store.VPS(ctx, workspace, id)
	if err != nil {
		return err
	}
	if v.Phase == "drafted" || v.Phase == "deleted" {
		return nil
	}
	if v.ApprovedAt == nil || v.Digest != vpsDigest(v) {
		return fmt.Errorf("VM approval is missing or changed")
	}
	var remove bool
	if err = conn.QueryRow(ctx, `SELECT delete_requested FROM workspace_vps WHERE workspace_id=$1 AND id=$2`, workspace, id).Scan(&remove); err != nil {
		return err
	}
	now := d.Store.now()
	expired := v.ExpiresAt != nil && !v.ExpiresAt.After(now)
	if remove || expired || v.Phase == "deleting" {
		v.Phase = "deleting"
		if err = d.save(ctx, v); err != nil {
			return err
		}
		operation, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		_, err = d.Engine.DestroyVPS(operation, v.Plan)
		if err != nil {
			v.Failure = "Deletion is still pending. Canter will retry until removal is verified."
			_ = d.save(context.WithoutCancel(ctx), v)
			return err
		}
		if err = d.releaseReservation(ctx, v); err != nil {
			return err
		}
		v.Phase = "deleted"
		v.Address = ""
		v.Failure = ""
		if v.ProvisioningFailed {
			v.Failure = "Provisioning failed; the allocated resources have been removed."
		}
		if err = d.save(ctx, v); err != nil {
			return err
		}
		_ = d.Store.Audit(ctx, workspace, sdk.ActorRef{Kind: "system", ID: "vps-dispatcher"}, "vps.deleted", id, map[string]any{"expired": expired})
		return nil
	}
	if v.Phase == "ready" {
		return d.save(ctx, v)
	}
	if v.Phase == "queued" {
		if err = d.Store.requireBillingPayment(ctx, workspace); err != nil {
			v.Phase = "deleting"
			v.Failure = "Billing needs attention; provisioning was cancelled."
			return d.save(ctx, v)
		}
		v.Phase = "creating"
		if err = d.save(ctx, v); err != nil {
			return err
		}
	}
	deadline := v.ApprovedAt.Add(10 * time.Minute)
	if v.ExpiresAt != nil && v.ExpiresAt.Before(deadline) {
		deadline = *v.ExpiresAt
	}
	operation, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	state, applyErr := d.Engine.ApplyVPS(operation, v.Plan)
	if applyErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		} // shutdown leaves recovery to the next worker
		v.Phase = "deleting"
		v.ProvisioningFailed = true
		v.Failure = "Provisioning did not finish. Canter is cleaning up the allocation."
		_ = d.save(ctx, v)
		return applyErr
	}
	if len(state.Resources) != 1 || state.Resources[0].Address == "" || state.Phase != "ready" {
		v.Phase = "deleting"
		v.ProvisioningFailed = true
		v.Failure = "VM access could not be verified; cleaning up."
		return d.save(ctx, v)
	}
	fingerprint, err := d.Engine.VPSHostFingerprint(operation, v.Plan)
	if err != nil {
		v.Phase = "deleting"
		v.ProvisioningFailed = true
		v.Failure = "SSH setup could not be verified; cleaning up."
		return d.save(ctx, v)
	}
	v.Phase = "ready"
	v.Address = state.Resources[0].Address
	v.HostFingerprint = fingerprint
	v.Failure = ""
	if err = d.save(ctx, v); err == nil {
		_ = d.Store.Audit(ctx, workspace, sdk.ActorRef{Kind: "system", ID: "vps-dispatcher"}, "vps.ready", id, map[string]any{"expiresAt": v.ExpiresAt})
	}
	return err
}
func (d *VPSDispatcher) save(ctx context.Context, v VPS) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	next := d.Store.now().Add(10 * time.Second)
	if v.Phase == "ready" {
		next = d.Store.now().Add(time.Minute)
		if v.ExpiresAt != nil && v.ExpiresAt.Before(next) {
			next = *v.ExpiresAt
		}
	}
	_, err = d.Store.pool.Exec(ctx, `UPDATE workspace_vps SET phase=$3,document=$4,updated_at=$5,next_attempt_at=CASE WHEN delete_requested THEN now() ELSE $6 END WHERE workspace_id=$1 AND id=$2`, v.WorkspaceID, v.ID, v.Phase, raw, d.Store.now(), next)
	return err
}
func (d *VPSDispatcher) releaseReservation(ctx context.Context, v VPS) error {
	tx, err := d.Store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var amount int
	err = tx.QueryRow(ctx, `UPDATE usage_reservations SET phase='released',updated_at=now() WHERE workspace_id=$1 AND subject_kind='vps' AND subject_id=$2 AND phase='reserved' RETURNING amount_cents`, v.WorkspaceID, v.ID).Scan(&amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE workspace_usage_caps SET reserved_cents=reserved_cents-$1,updated_at=now() WHERE workspace_id=$2`, amount, v.WorkspaceID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
