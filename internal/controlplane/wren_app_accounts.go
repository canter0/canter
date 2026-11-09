package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type WrenAppAccount struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	Endpoint  string    `json:"endpoint"`
	Label     string    `json:"label"`
	Revision  int       `json:"revision"`
	Reconnect bool      `json:"reconnect"`
	CreatedAt time.Time `json:"createdAt"`
}

func (s *Store) WrenAppAccounts(ctx context.Context, workspace, account string) ([]WrenAppAccount, error) {
	if err := s.RequireWorkspaceWriter(ctx, workspace, account); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,provider,endpoint,label,revision,reconnect,created_at FROM wren_app_accounts WHERE workspace_id=$1 AND account_id=$2 AND revoked_at IS NULL ORDER BY created_at,id`, workspace, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WrenAppAccount{}
	for rows.Next() {
		var v WrenAppAccount
		if err = rows.Scan(&v.ID, &v.Provider, &v.Endpoint, &v.Label, &v.Revision, &v.Reconnect, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func appAccountEndpoint(manifest WrenExtensionManifest) (string, bool) {
	if manifest.Auth != "bearer" || len(manifest.Tools) == 0 {
		return "", false
	}
	endpoint := manifest.Tools[0].Endpoint
	for _, tool := range manifest.Tools {
		if tool.Method != "MCP" || tool.Endpoint != endpoint {
			return "", false
		}
	}
	return endpoint, true
}
func (h *HTTPServer) linkWrenAppAccount(ctx context.Context, workspace, account, installation string, input WrenExtensionCredentialInput) (WrenExtensionInstallation, error) {
	if input.Remove || input.Value != "" || input.SharedService || input.ExpectedRevision < 0 {
		return WrenExtensionInstallation{}, fmt.Errorf("choose one personal app account")
	}
	v, err := h.service.Store.WrenExtensionInstallation(ctx, workspace, account, installation)
	if err != nil {
		return v, err
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return v, err
	}
	defer tx.Rollback(ctx)
	if err = extensionScopeWrite(ctx, tx, workspace, account, v.WrenKnowledgeScope, true); err != nil {
		return v, err
	}
	v, err = scanExtensionInstall(tx.QueryRow(ctx, `SELECT `+extensionInstallColumns+` FROM wren_extension_installations i WHERE i.id=$1 FOR UPDATE`, installation))
	if err != nil {
		return v, err
	}
	if v.CredentialMode != "personal" || v.RevokedAt != nil {
		return v, ErrForbidden
	}
	v.Extension, err = scanExtension(tx.QueryRow(ctx, `SELECT `+extensionColumns+` FROM wren_extensions e JOIN wren_extension_versions v ON v.extension_id=e.id AND v.version=$2 WHERE e.id=$1 FOR SHARE OF e`, v.ExtensionID, v.Version))
	if err != nil {
		return v, err
	}
	endpoint, ok := appAccountEndpoint(v.Extension.Manifest)
	if !ok {
		return v, fmt.Errorf("this connection cannot use a saved app account")
	}
	var revision int
	var prior string
	err = tx.QueryRow(ctx, `SELECT revision,COALESCE(app_account_id,'') FROM wren_extension_credentials WHERE installation_id=$1 AND credential_key=$2 FOR UPDATE`, installation, "user:"+account).Scan(&revision, &prior)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	var linked bool
	if err = tx.QueryRow(ctx, `SELECT true FROM wren_app_accounts WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND endpoint=$4 AND revoked_at IS NULL AND NOT reconnect FOR SHARE`, input.AppAccountID, workspace, account, endpoint).Scan(&linked); errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	} else if err != nil {
		return v, err
	}
	if revision != input.ExpectedRevision {
		if revision == input.ExpectedRevision+1 && prior == input.AppAccountID {
			return h.service.Store.WrenExtensionInstallation(ctx, workspace, account, installation)
		}
		return v, ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO wren_extension_credentials(installation_id,credential_key,account_id,revision,key_id,ciphertext,updated_by,app_account_id) VALUES($1,$2,$3,$4,'',NULL,$3,$5) ON CONFLICT(installation_id,credential_key) DO UPDATE SET revision=EXCLUDED.revision,key_id='',ciphertext=NULL,app_account_id=EXCLUDED.app_account_id,updated_by=EXCLUDED.updated_by,updated_at=now()`, installation, "user:"+account, account, revision+1, input.AppAccountID)
	if err != nil {
		return v, err
	}
	if err = tx.Commit(ctx); err != nil {
		return v, err
	}
	return h.service.Store.WrenExtensionInstallation(ctx, workspace, account, installation)
}
func (h *HTTPServer) appAccountDiscovery(ctx context.Context, workspace, account, id string) (wrenMCPDiscovery, error) {
	if err := h.service.Store.RequireWorkspaceWriter(ctx, workspace, account); err != nil {
		return wrenMCPDiscovery{}, err
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return wrenMCPDiscovery{}, err
	}
	defer tx.Rollback(ctx)
	if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return wrenMCPDiscovery{}, err
	}
	var endpoint, providerID string
	err = tx.QueryRow(ctx, `SELECT endpoint,provider FROM wren_app_accounts WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND revoked_at IS NULL`, id, workspace, account).Scan(&endpoint, &providerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return wrenMCPDiscovery{}, ErrNotFound
	}
	if err != nil {
		return wrenMCPDiscovery{}, err
	}
	credential, err := h.appAccountTokenTx(ctx, tx, workspace, account, id, endpoint)
	if err != nil {
		// A permanent authorization failure updates account state, but executes no tool.
		if errors.Is(err, errAppReconnect) {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return wrenMCPDiscovery{}, commitErr
			}
		}
		return wrenMCPDiscovery{}, err
	}
	var credentialVersion []byte
	if err = tx.QueryRow(ctx, `SELECT ciphertext FROM wren_app_accounts WHERE id=$1`, id).Scan(&credentialVersion); err != nil {
		return wrenMCPDiscovery{}, err
	}
	// Save a rotated refresh token before any separate provider discovery can fail.
	if err = tx.Commit(ctx); err != nil {
		return wrenMCPDiscovery{}, err
	}
	result, err := discoverExtensionMCP(ctx, h.config.Extensions, endpoint, credential)
	if errors.Is(err, errMCPUnauthorized) {
		if markErr := h.markRejectedAppAccount(ctx, workspace, account, id, credentialVersion); markErr != nil {
			return result, markErr
		}
		return result, errAppReconnect
	}
	if err != nil {
		return result, err
	}
	if provider, ok := h.appProvider(providerID); ok && provider.Endpoint == endpoint {
		result.Name = provider.Name
		// This provider is explicitly bound to its documented read-only endpoint,
		// not inferred from untrusted tool annotations.
		for i := range result.Tools {
			result.Tools[i].Impact = "read"
		}
	}
	return result, nil
}
func (h *HTTPServer) workspaceAppAccounts(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	account := p.Account.ID
	if err := h.service.Store.RequireWorkspaceWriter(r.Context(), workspace, account); err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if len(parts) == 0 && r.Method == "GET" {
		accounts, err := h.service.Store.WrenAppAccounts(r.Context(), workspace, account)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"accounts": accounts, "enabled": h.config.Secrets != nil})
		return
	}
	if len(parts) == 1 && parts[0] == "connect" && r.Method == "POST" {
		h.startAppOAuth(w, r, workspace, account)
		return
	}
	if len(parts) == 2 && parts[1] == "discover" && r.Method == "POST" {
		result, err := h.appAccountDiscovery(r.Context(), workspace, account, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, result)
		return
	}
	if len(parts) == 1 && (r.Method == "DELETE" || r.Method == "PATCH") {
		var input struct {
			ExpectedRevision int    `json:"expectedRevision"`
			Label            string `json:"label"`
		}
		if !decodeLimit(w, r, &input, 2048) {
			return
		}
		if input.ExpectedRevision < 1 || (r.Method == "PATCH" && !artifactSafeText(input.Label, 100)) {
			writeError(w, 400, fmt.Errorf("provide a current revision and valid account label"))
			return
		}
		tx, err := h.service.Store.pool.Begin(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer tx.Rollback(r.Context())
		if err = requireWorkspaceWriter(r.Context(), tx, workspace, account, true); err != nil {
			writeStoreError(w, err)
			return
		}
		var revision int
		var revoked *time.Time
		err = tx.QueryRow(r.Context(), `SELECT revision,revoked_at FROM wren_app_accounts WHERE id=$1 AND workspace_id=$2 AND account_id=$3 FOR UPDATE`, parts[0], workspace, account).Scan(&revision, &revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			writeStoreError(w, ErrNotFound)
			return
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if r.Method == "DELETE" && revoked != nil {
			writeJSON(w, 200, map[string]bool{"disconnected": true})
			return
		}
		if revoked != nil || revision != input.ExpectedRevision {
			writeStoreError(w, ErrConflict)
			return
		}
		if r.Method == "DELETE" {
			_, err = tx.Exec(r.Context(), `UPDATE wren_app_accounts SET revoked_at=now(),ciphertext=NULL,revision=revision+1,updated_at=now() WHERE id=$1`, parts[0])
		} else {
			_, err = tx.Exec(r.Context(), `UPDATE wren_app_accounts SET label=$2,revision=revision+1,updated_at=now() WHERE id=$1`, parts[0], input.Label)
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"saved": true})
		return
	}
	writeStoreError(w, ErrNotFound)
}

// Compare the encrypted credential generation: a delayed rejection must not
// invalidate a different token installed by a concurrent refresh/reconnect.
func (h *HTTPServer) markRejectedAppAccount(ctx context.Context, workspace, account, id string, credentialVersion []byte) error {
	_, err := h.service.Store.pool.Exec(ctx, `UPDATE wren_app_accounts SET reconnect=true,revision=revision+1,updated_at=now() WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND ciphertext=$4 AND NOT reconnect AND revoked_at IS NULL`, id, workspace, account, credentialVersion)
	return err
}
