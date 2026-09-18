package controlplane

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The keyring lives outside the database and client bundle. Old keys may remain
// available for reads while all new writes use Active. Never log this structure.
type SecretVault struct {
	active string
	keys   map[string]cipher.AEAD
}

func NewSecretVault(raw []byte) (*SecretVault, error) {
	var input struct {
		Active string            `json:"active"`
		Keys   map[string]string `json:"keys"`
	}
	if json.Unmarshal(raw, &input) != nil || input.Active == "" {
		return nil, errors.New("invalid secrets keyring")
	}
	v := &SecretVault{active: input.Active, keys: map[string]cipher.AEAD{}}
	for id, value := range input.Keys {
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(key) != 32 || !secretKeyID.MatchString(id) {
			return nil, errors.New("secrets keyring requires named 32-byte keys")
		}
		block, err := aes.NewCipher(key)
		clear(key)
		if err != nil {
			return nil, errors.New("invalid secrets keyring")
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		v.keys[id] = aead
	}
	if v.keys[v.active] == nil {
		return nil, errors.New("active secrets key is missing")
	}
	return v, nil
}

var secretKeyID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,79}$`)
var errSecretUnavailable = errors.New("workspace secret is unavailable; ask the owner to rotate it")

func secretBinding(workspace, id, purpose string) []byte {
	return []byte("canter:workspace-secret:v1\x00" + workspace + "\x00" + id + "\x00" + purpose)
}
func (v *SecretVault) seal(value, binding []byte) (string, []byte, error) {
	if v == nil {
		return "", nil, errSecretUnavailable
	}
	a := v.keys[v.active]
	n := make([]byte, a.NonceSize())
	if _, err := rand.Read(n); err != nil {
		return "", nil, err
	}
	return v.active, a.Seal(n, n, value, binding), nil
}
func (v *SecretVault) open(key string, value, binding []byte) ([]byte, error) {
	if v == nil {
		return nil, errSecretUnavailable
	}
	a := v.keys[key]
	if a == nil || len(value) < a.NonceSize()+a.Overhead() {
		return nil, errSecretUnavailable
	}
	out, err := a.Open(nil, value[:a.NonceSize()], value[a.NonceSize():], binding)
	if err != nil {
		return nil, errSecretUnavailable
	}
	return out, nil
}

type workspaceSecret struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Purpose    string     `json:"purpose"`
	Note       string     `json:"note"`
	Version    int        `json:"version"`
	UpdatedBy  string     `json:"updatedBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

func (h *HTTPServer) workspaceSecrets(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	// Agent principals may inspect metadata, but only a workspace owner can write.
	owner := false
	if p.Account != nil && p.Installation == nil {
		role, err := h.service.Store.Membership(r.Context(), p.Account.ID, workspace)
		owner = err == nil && role == "owner"
	}
	if len(parts) == 0 && r.Method == http.MethodGet {
		rows, err := h.service.Store.pool.Query(r.Context(), `SELECT id,name,purpose,note,version,updated_by,created_at,updated_at,last_used_at FROM workspace_secrets WHERE workspace_id=$1 AND revoked_at IS NULL ORDER BY updated_at DESC`, workspace)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer rows.Close()
		items := []workspaceSecret{}
		for rows.Next() {
			var item workspaceSecret
			if err = rows.Scan(&item.ID, &item.Name, &item.Purpose, &item.Note, &item.Version, &item.UpdatedBy, &item.CreatedAt, &item.UpdatedAt, &item.LastUsedAt); err != nil {
				writeStoreError(w, err)
				return
			}
			items = append(items, item)
		}
		if err = rows.Err(); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"secrets": items, "enabled": h.config.Secrets != nil, "canManage": owner})
		return
	}
	if !owner {
		writeStoreError(w, ErrForbidden)
		return
	}
	create := len(parts) == 0 && r.Method == http.MethodPost
	rotate := len(parts) == 1 && r.Method == http.MethodPut
	revoke := len(parts) == 1 && r.Method == http.MethodDelete
	if h.config.Secrets == nil && !revoke {
		writeError(w, http.StatusServiceUnavailable, errors.New("workspace secret storage is not configured"))
		return
	}
	if !create && !rotate && !revoke {
		writeStoreError(w, ErrNotFound)
		return
	}
	var input struct {
		Name    string `json:"name"`
		Purpose string `json:"purpose"`
		Note    string `json:"note"`
		Value   string `json:"value"`
		Version int    `json:"version"`
	}
	if !decodeLimit(w, r, &input, 24<<10) {
		return
	}
	if !revoke && (len(input.Value) == 0 || len(input.Value) > 8192 || strings.TrimSpace(input.Value) == "") {
		writeError(w, http.StatusBadRequest, errors.New("enter a secret value up to 8 KiB"))
		return
	}
	if create && (!secretName.MatchString(input.Name) || len(input.Note) > 500 || (input.Purpose != "stored" && input.Purpose != "openrouter")) {
		writeError(w, http.StatusBadRequest, errors.New("use an uppercase secret name, a supported purpose, and a note up to 500 characters"))
		return
	}
	ctx := r.Context()
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	id := ""
	purpose := input.Purpose
	version := 1
	action := "secret.created"
	if create {
		id, err = newID("sec_")
	} else {
		id = parts[0]
		err = tx.QueryRow(ctx, `SELECT purpose,version FROM workspace_secrets WHERE workspace_id=$1 AND id=$2 AND revoked_at IS NULL FOR UPDATE`, workspace, id).Scan(&purpose, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			writeStoreError(w, ErrNotFound)
			return
		}
		if err == nil && input.Version != version {
			writeStoreError(w, ErrConflict)
			return
		}
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if revoke {
		action = "secret.revoked"
		_, err = tx.Exec(ctx, `UPDATE workspace_secrets SET ciphertext=NULL,revoked_at=now(),updated_at=now(),updated_by=$3,version=version+1 WHERE workspace_id=$1 AND id=$2`, workspace, id, p.Account.ID)
	} else {
		if purpose == "openrouter" && (strings.ContainsAny(input.Value, "\r\n\t ") || len(input.Value) < 16) {
			writeError(w, http.StatusBadRequest, errors.New("enter an OpenRouter API key without whitespace"))
			return
		}
		plain := []byte(input.Value)
		input.Value = ""
		key, sealed, sealErr := h.config.Secrets.seal(plain, secretBinding(workspace, id, purpose))
		clear(plain)
		if sealErr != nil {
			writeError(w, http.StatusServiceUnavailable, errSecretUnavailable)
			return
		}
		if create {
			_, err = tx.Exec(ctx, `INSERT INTO workspace_secrets(id,workspace_id,name,purpose,note,ciphertext,key_id,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, workspace, input.Name, purpose, input.Note, sealed, key, p.Account.ID)
		} else {
			action = "secret.rotated"
			_, err = tx.Exec(ctx, `UPDATE workspace_secrets SET ciphertext=$3,key_id=$4,version=version+1,updated_by=$5,updated_at=now() WHERE workspace_id=$1 AND id=$2`, workspace, id, sealed, key, p.Account.ID)
		}
	}
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			writeError(w, http.StatusConflict, errors.New("this secret name or OpenRouter connection already exists"))
		} else {
			writeStoreError(w, err)
		}
		return
	}
	if err = secretAudit(ctx, tx, workspace, p.Actor, action, id); err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "saved": true})
}
func secretAudit(ctx context.Context, tx pgx.Tx, workspace string, actor sdk.ActorRef, action, id string) error {
	event, err := newID("evt_")
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(id,workspace_id,actor_kind,actor_id,session_id,action,subject) VALUES($1,$2,$3,$4,$5,$6,$7)`, event, workspace, actor.Kind, actor.ID, actor.SessionID, action, id)
	return err
}

// Resolve immediately before each provider request. Values never enter model
// messages, checkpoints, API responses, or arbitrary agent-selected endpoints.
func (h *HTTPServer) workspaceModelConfig(ctx context.Context, workspace string, actor sdk.ActorRef, config OperatorConfig) (OperatorConfig, error) {
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return config, err
	}
	defer tx.Rollback(ctx)
	var id, key string
	var sealed []byte
	err = tx.QueryRow(ctx, `SELECT id,key_id,ciphertext FROM workspace_secrets WHERE workspace_id=$1 AND purpose='openrouter' AND revoked_at IS NULL FOR UPDATE`, workspace).Scan(&id, &key, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	plain, err := h.config.Secrets.open(key, sealed, secretBinding(workspace, id, "openrouter"))
	if err != nil {
		return config, err
	}
	defer clear(plain)
	if err = secretAudit(ctx, tx, workspace, actor, "secret.used", id); err != nil {
		return config, err
	}
	if _, err = tx.Exec(ctx, `UPDATE workspace_secrets SET last_used_at=now() WHERE id=$1`, id); err != nil {
		return config, err
	}
	if err = tx.Commit(ctx); err != nil {
		return config, err
	}
	config.APIKey = string(plain)
	config.BaseURL = "https://openrouter.ai/api/v1"
	return config, nil
}
func (h *HTTPServer) workspaceOperatorReady(ctx context.Context, workspace string) bool {
	if h.config.Operator.Ready() {
		return true
	}
	if h.config.Secrets == nil || h.config.Operator.Model == "" {
		return false
	}
	var found bool
	err := h.service.Store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_secrets WHERE workspace_id=$1 AND purpose='openrouter' AND revoked_at IS NULL)`, workspace).Scan(&found)
	return err == nil && found
}
