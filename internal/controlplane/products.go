package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

type productEntry struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Enabled     bool       `json:"enabled"`
	AlwaysOn    bool       `json:"alwaysOn"`
	EnabledAt   *time.Time `json:"enabledAt,omitempty"`
}

var productCatalog = []productEntry{
	{ID: "wrens", Name: "Wrens", Description: "Persistent agents that work alongside your organization.", AlwaysOn: true},
	{ID: "sandboxes", Name: "Sandboxes", Description: "Cloud computers for you and your agents. Run code, keep files, sleep when idle."},
	{ID: "ci", Name: "CI", Description: "GitHub Actions runners with a free first hour on every run."},
}

func productKnown(id string) bool {
	for _, p := range productCatalog {
		if p.ID == id {
			return true
		}
	}
	return false
}

func productAlwaysOn(id string) bool {
	for _, p := range productCatalog {
		if p.ID == id {
			return p.AlwaysOn
		}
	}
	return false
}

// WorkspaceProducts returns the catalog annotated with this workspace's
// enablement state. Any member may read it.
func (s *Store) WorkspaceProducts(ctx context.Context, workspaceID string) ([]productEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT product,enabled_at FROM workspace_products WHERE workspace_id=$1`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	enabled := map[string]time.Time{}
	for rows.Next() {
		var product string
		var at time.Time
		if err = rows.Scan(&product, &at); err != nil {
			return nil, err
		}
		enabled[product] = at
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]productEntry, 0, len(productCatalog))
	for _, p := range productCatalog {
		if p.ID == "wrens" && !s.wrensAvailable() {
			continue
		}
		entry := p
		if at, ok := enabled[p.ID]; ok || p.AlwaysOn {
			entry.Enabled = true
			if ok {
				t := at
				entry.EnabledAt = &t
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

func (s *Store) productEnabled(ctx context.Context, q artifactQueryer, workspaceID, product string) (bool, error) {
	if product == "wrens" && !s.wrensAvailable() {
		return false, nil
	}
	if productAlwaysOn(product) {
		return true, nil
	}
	var found bool
	err := q.QueryRow(ctx, `SELECT true FROM workspace_products WHERE workspace_id=$1 AND product=$2`, workspaceID, product).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return found, err
}

// EnableWorkspaceProduct records enablement. Idempotent: an enabled product
// stays enabled without error.
func (s *Store) EnableWorkspaceProduct(ctx context.Context, workspaceID, product string, actor sdk.ActorRef) error {
	if !productKnown(product) || (product == "wrens" && !s.wrensAvailable()) {
		return ErrNotFound
	}
	if productAlwaysOn(product) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO workspace_products(workspace_id,product,enabled_by_kind,enabled_by_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, workspaceID, product, actor.Kind, actor.ID)
	return err
}

// requireWorkspaceProductForUseTx activates a product as part of an authorized
// first use. Callers must hold the workspace row lock (also used by disable)
// and commit only after all operation-specific validation and writes succeed.
// account is the human authority, including for the home operator; connected
// installations pass an empty account and may only use already-enabled products.
func (s *Store) requireWorkspaceProductForUseTx(ctx context.Context, tx pgx.Tx, workspaceID, product, account string, actor sdk.ActorRef) error {
	if !productKnown(product) || (product == "wrens" && !s.wrensAvailable()) {
		return ErrNotFound
	}
	enabled, err := s.productEnabled(ctx, tx, workspaceID, product)
	if err != nil || enabled {
		return err
	}
	if account == "" {
		return fmt.Errorf("%w: a workspace owner or operator must enable %s first", ErrConflict, product)
	}
	if err := requireWorkspaceWriter(ctx, tx, workspaceID, account, true); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `INSERT INTO workspace_products(workspace_id,product,enabled_by_kind,enabled_by_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, workspaceID, product, actor.Kind, actor.ID)
	if err != nil || result.RowsAffected() == 0 {
		return err
	}
	event, err := newID("evt_")
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(id,workspace_id,actor_kind,actor_id,session_id,action,subject,metadata,occurred_at) VALUES($1,$2,$3,$4,$5,'product.enabled',$6,'{"implicit":true}'::jsonb,$7)`, event, workspaceID, actor.Kind, actor.ID, actor.SessionID, product, s.now())
	return err
}

// DisableWorkspaceProduct removes enablement. alwaysOn products and disabling
// Sandboxes while sandboxes still exist are conflicts. Idempotent.
func (s *Store) DisableWorkspaceProduct(ctx context.Context, workspaceID, product string) error {
	if !productKnown(product) || (product == "wrens" && !s.wrensAvailable()) {
		return ErrNotFound
	}
	if productAlwaysOn(product) {
		return ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&locked); err != nil {
		return err
	}
	if product == "sandboxes" {
		var live bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandboxes WHERE workspace_id=$1 AND phase<>'deleted')
			OR EXISTS(SELECT 1 FROM sandbox_snapshots WHERE workspace_id=$1 AND state='ready' AND retained)
			OR EXISTS(SELECT 1 FROM sandbox_templates WHERE workspace_id=$1 AND deleted_at IS NULL)`, workspaceID).Scan(&live)
		if err != nil {
			return err
		}
		if live {
			return ErrConflict
		}
	}
	if product == "ci" {
		protected, err := s.ciWorkspaceProtectedTx(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if protected {
			return fmt.Errorf("%w: wait for CI runners and billing reconciliation to finish before disabling CI", ErrConflict)
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM workspace_products WHERE workspace_id=$1 AND product=$2`, workspaceID, product); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (h *HTTPServer) workspaceProducts(w http.ResponseWriter, r *http.Request, p Principal, workspaceID string, parts []string) {
	if len(parts) == 0 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		if p.Account == nil {
			writeStoreError(w, ErrForbidden)
			return
		}
		products, err := h.service.Store.WorkspaceProducts(r.Context(), workspaceID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"products": products})
		return
	}
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	product := parts[0]
	switch r.Method {
	case http.MethodPut:
		// Humans and the home agent acting on a human's behalf share the
		// account principal; connected-agent installations may not enable.
		if p.Account == nil || p.Installation != nil {
			writeStoreError(w, ErrForbidden)
			return
		}
		role, err := workspaceRole(r.Context(), h.service.Store.pool, workspaceID, p.Account.ID, false)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if role != "owner" && role != "operator" {
			writeStoreError(w, ErrForbidden)
			return
		}
		if err = h.service.Store.EnableWorkspaceProduct(r.Context(), workspaceID, product, p.Actor); err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "product.enabled", product, map[string]any{})
		writeJSON(w, http.StatusOK, map[string]any{"product": product, "enabled": true})
	case http.MethodDelete:
		if p.Account == nil || p.Installation != nil {
			writeStoreError(w, ErrForbidden)
			return
		}
		role, err := workspaceRole(r.Context(), h.service.Store.pool, workspaceID, p.Account.ID, false)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if role != "owner" {
			writeStoreError(w, ErrForbidden)
			return
		}
		err = h.service.Store.DisableWorkspaceProduct(r.Context(), workspaceID, product)
		if errors.Is(err, ErrConflict) && product == "sandboxes" {
			writeError(w, http.StatusConflict, errors.New("Delete your sandboxes and saved environments first."))
			return
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "product.disabled", product, map[string]any{})
		writeJSON(w, http.StatusOK, map[string]any{"product": product, "enabled": false})
	default:
		methodNotAllowed(w)
	}
}

func productDisplayName(id string) string {
	for _, p := range productCatalog {
		product := p
		if product.ID == id {
			return product.Name
		}
	}
	return id
}
