package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
	"net/http"
	"time"
)

type ComputerCheckpointEngine interface {
	PutComputerCheckpoint(context.Context, string, []byte) (sdk.ComputerCheckpointObject, error)
	GetComputerCheckpoint(context.Context, string, sdk.ComputerCheckpointObject) ([]byte, error)
	DeleteComputerCheckpoint(context.Context, string, sdk.ComputerCheckpointObject) error
}
type WrenComputerCheckpoint struct {
	EncryptionNonce        []byte                          `json:"-"`
	ID                     string                          `json:"id"`
	WorkspaceID            string                          `json:"-"`
	ConversationID         string                          `json:"conversationId"`
	OwnerID                string                          `json:"-"`
	ScopeKind              string                          `json:"scope"`
	ComputerID             string                          `json:"computerId"`
	State                  string                          `json:"state"`
	IncludeBrowser         bool                            `json:"includeBrowser"`
	DeleteAfter            bool                            `json:"sleepAfterSaving"`
	KeyVersion             string                          `json:"-"`
	Object                 sdk.ComputerCheckpointObject    `json:"-"`
	Manifest               wrencomputer.CheckpointManifest `json:"-"`
	Failure                string                          `json:"failure,omitempty"`
	CreatedAt              time.Time                       `json:"createdAt"`
	UpdatedAt              time.Time                       `json:"updatedAt"`
	RawBytes               int64                           `json:"rawBytes"`
	CompressedBytes        int64                           `json:"compressedBytes"`
	FileCount              int                             `json:"fileCount"`
	RAMSupported           bool                            `json:"ramSupported"`
	ProcessResumeSupported bool                            `json:"processResumeSupported"`
	BrowserLoginGuaranteed bool                            `json:"browserLoginGuaranteed"`
}

const checkpointColumns = `id,workspace_id,conversation_id,owner_id,scope_kind,computer_id,state,include_browser,delete_after,key_version,encryption_nonce,object,manifest,failure,created_at,updated_at`

func scanComputerCheckpoint(row interface{ Scan(...any) error }) (WrenComputerCheckpoint, error) {
	var c WrenComputerCheckpoint
	var object, manifest []byte
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.ConversationID, &c.OwnerID, &c.ScopeKind, &c.ComputerID, &c.State, &c.IncludeBrowser, &c.DeleteAfter, &c.KeyVersion, &c.EncryptionNonce, &object, &manifest, &c.Failure, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if json.Unmarshal(object, &c.Object) != nil || json.Unmarshal(manifest, &c.Manifest) != nil {
		return c, fmt.Errorf("invalid checkpoint record")
	}
	c.RawBytes, c.CompressedBytes, c.FileCount = c.Manifest.RawBytes, c.Manifest.CompressedBytes, len(c.Manifest.Entries)
	return c, nil
}
func (c WrenComputerCheckpoint) storageScope() string {
	return c.WorkspaceID + "\x00" + c.ConversationID + "\x00" + c.ScopeKind
}
func (c WrenComputerCheckpoint) binding() []byte {
	return secretBinding(c.WorkspaceID, c.ID, "wren-computer-checkpoint:"+c.ConversationID+":"+c.ScopeKind)
}
func (h *HTTPServer) computerCheckpoint(ctx context.Context, workspace, account, id string, manage bool) (WrenComputerCheckpoint, error) {
	c, err := scanComputerCheckpoint(h.service.Store.pool.QueryRow(ctx, `SELECT `+checkpointColumns+` FROM wren_computer_checkpoints WHERE id=$1 AND workspace_id=$2`, id, workspace))
	if err != nil {
		return c, err
	}
	kind, _, _, err := h.service.Store.computerScope(ctx, workspace, account, c.ConversationID, manage)
	if err != nil || kind != c.ScopeKind {
		return c, ErrForbidden
	}
	return c, nil
}
func (h *HTTPServer) CreateWrenComputerCheckpoint(ctx context.Context, workspace, account, computer string, browser, sleep bool) (WrenComputerCheckpoint, error) {
	if _, ok := h.service.Engine.(ComputerCheckpointEngine); !ok || h.config.Secrets == nil {
		return WrenComputerCheckpoint{}, fmt.Errorf("encrypted checkpoint storage is unavailable")
	}
	v, err := h.service.Store.WrenComputer(ctx, workspace, account, computer)
	if err != nil {
		return WrenComputerCheckpoint{}, err
	}
	if _, _, _, err = h.service.Store.computerScope(ctx, workspace, account, v.ConversationID, true); err != nil {
		return WrenComputerCheckpoint{}, err
	}
	// Fence metadata creation against an already-authorized input, file write,
	// command launch or coding-worker creation. Once committed, the creating
	// checkpoint blocks subsequent mutations until the snapshot finishes.
	operation, err := h.service.Store.beginComputerOperation(ctx, v.ID)
	if err != nil {
		return WrenComputerCheckpoint{}, err
	}
	defer operation.close()
	ctx = operation.ctx
	commands, err := h.service.Store.pendingComputerCommands(ctx, v.ID)
	if err != nil {
		return WrenComputerCheckpoint{}, err
	}
	if len(commands) > 0 {
		client, current, e := h.ClientForConversation(ctx, Conversation{ID: v.ConversationID, WorkspaceID: workspace, AccountID: account})
		if e != nil {
			return WrenComputerCheckpoint{}, e
		}
		if current.ID != v.ID {
			return WrenComputerCheckpoint{}, ErrConflict
		}
		// Read-only reconciliation: saving never implicitly stops live work.
		if e = h.service.Store.settleComputerCommands(ctx, client, v.ID, commands, nil); e != nil {
			var running *computerCommandsRunning
			if errors.As(e, &running) {
				return WrenComputerCheckpoint{}, fmt.Errorf("%w: a command is still running. Finish it or use Take over to review and stop work before saving", ErrConflict)
			}
			return WrenComputerCheckpoint{}, fmt.Errorf("%w: could not verify that computer work has finished; try again before saving", ErrConflict)
		}
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return WrenComputerCheckpoint{}, err
	}
	defer tx.Rollback(ctx)
	// Checkpoint metadata deliberately has no cascading tenant FK: explicit
	// account/workspace locks still fence creation against account erasure.
	var scopeLock string
	if err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 AND disabled_at IS NULL FOR KEY SHARE`, account).Scan(&scopeLock); err != nil {
		return WrenComputerCheckpoint{}, ErrForbidden
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR KEY SHARE`, workspace).Scan(&scopeLock); err != nil {
		return WrenComputerCheckpoint{}, ErrForbidden
	}
	if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return WrenComputerCheckpoint{}, err
	}
	if _, _, _, err = h.service.Store.computerScope(ctx, workspace, account, v.ConversationID, true); err != nil {
		return WrenComputerCheckpoint{}, err
	}
	v, err = scanWrenComputer(tx.QueryRow(ctx, `SELECT `+computerColumns+` FROM wren_computers WHERE id=$1 FOR UPDATE`, computer))
	if err != nil {
		return WrenComputerCheckpoint{}, err
	}
	if v.Phase != "ready" || v.DeleteRequested || !v.ExpiresAt.After(h.service.Store.now()) {
		return WrenComputerCheckpoint{}, ErrConflict
	}
	if h.activeComputerHumanControl(ctx, v) && v.ControlAccountID != account {
		return WrenComputerCheckpoint{}, fmt.Errorf("%w: another person is controlling this computer", ErrConflict)
	}
	busy, err := activeComputerACP(ctx, tx, v.ID, "")
	if err != nil {
		return WrenComputerCheckpoint{}, err
	}
	held, err := liveComputerMutation(ctx, tx, v)
	if err != nil {
		return WrenComputerCheckpoint{}, err
	}
	if busy || held {
		return WrenComputerCheckpoint{}, fmt.Errorf("%w: stop active computer work before saving its workspace", ErrConflict)
	}
	var count, pending int
	if err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE state='creating') FROM wren_computer_checkpoints WHERE workspace_id=$1 AND conversation_id=$2 `, workspace, v.ConversationID).Scan(&count, &pending); err != nil {
		return WrenComputerCheckpoint{}, err
	}
	if pending > 0 {
		return WrenComputerCheckpoint{}, ErrConflict
	}
	if count >= 3 {
		return WrenComputerCheckpoint{}, fmt.Errorf("%w: delete an older checkpoint before creating another", ErrCapacity)
	}
	id, err := newID("checkpoint_")
	if err != nil {
		return WrenComputerCheckpoint{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO wren_computer_checkpoints(id,workspace_id,conversation_id,owner_id,scope_kind,computer_id,include_browser,delete_after) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, workspace, v.ConversationID, account, v.ScopeKind, v.ID, browser, sleep); err != nil {
		return WrenComputerCheckpoint{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return WrenComputerCheckpoint{}, err
	}
	return h.computerCheckpoint(ctx, workspace, account, id, false)
}
func (h *HTTPServer) WakeWrenComputerCheckpoint(ctx context.Context, workspace, account, id string) (WrenComputer, error) {
	c, err := h.computerCheckpoint(ctx, workspace, account, id, true)
	if err != nil {
		return WrenComputer{}, err
	}
	if c.State != "ready" && c.State != "restoring" {
		return WrenComputer{}, ErrConflict
	}
	return h.enableWrenComputer(ctx, workspace, account, c.ConversationID, id)
}
func (h *HTTPServer) workspaceComputerCheckpoints(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	account := p.Account.ID
	if len(parts) == 0 && r.Method == http.MethodGet {
		conversation := r.URL.Query().Get("conversationId")
		if _, _, _, err := h.service.Store.computerScope(r.Context(), workspace, account, conversation, false); err != nil {
			writeStoreError(w, err)
			return
		}
		rows, err := h.service.Store.pool.Query(r.Context(), `SELECT `+checkpointColumns+` FROM wren_computer_checkpoints WHERE workspace_id=$1 AND conversation_id=$2 ORDER BY created_at DESC LIMIT 20`, workspace, conversation)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer rows.Close()
		items := []WrenComputerCheckpoint{}
		for rows.Next() {
			item, e := scanComputerCheckpoint(rows)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			items = append(items, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"checkpoints": items})
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		writeStoreError(w, ErrNotFound)
		return
	}
	if parts[1] == "wake" {
		v, err := h.WakeWrenComputerCheckpoint(r.Context(), workspace, account, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, v)
		return
	}
	if parts[1] == "delete" {
		if err := h.DeleteWrenComputerCheckpoint(r.Context(), workspace, account, parts[0]); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"state": "deleting"})
		return
	}
	writeStoreError(w, ErrNotFound)
}
func (d *WrenComputerDispatcher) reconcileCheckpoints(ctx context.Context) error {
	rows, err := d.Server.service.Store.pool.Query(ctx, `SELECT id FROM wren_computer_checkpoints WHERE state IN ('creating','deleting','restoring') AND next_attempt_at<=now() ORDER BY created_at LIMIT 10`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = d.reconcileCheckpoint(ctx, id); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}
func (d *WrenComputerDispatcher) reconcileCheckpoint(ctx context.Context, id string) (returnErr error) {
	h := d.Server
	s := h.service.Store
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,950))`, id).Scan(&locked); err != nil || !locked {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1,950))`, id)
	c, err := scanComputerCheckpoint(conn.QueryRow(ctx, `SELECT `+checkpointColumns+` FROM wren_computer_checkpoints WHERE id=$1`, id))
	if err != nil {
		return err
	}
	defer func() {
		if returnErr != nil && ctx.Err() == nil && c.State == "creating" {
			_, _ = conn.Exec(ctx, `UPDATE wren_computer_checkpoints SET state='failed',failure='Saving files or encrypted storage verification failed. No sleep removal was requested. Check the computer status; its original expiry still applies.',updated_at=now() WHERE id=$1 AND state='creating'`, id)
		}
	}()
	engine, ok := h.service.Engine.(ComputerCheckpointEngine)
	if !ok {
		return fmt.Errorf("checkpoint storage unavailable")
	}
	if c.State == "deleting" {
		if c.Object.Key != "" {
			if err = engine.DeleteComputerCheckpoint(ctx, c.storageScope(), c.Object); err != nil {
				return err
			}
		}
		_, err = conn.Exec(ctx, `DELETE FROM wren_computer_checkpoints WHERE id=$1`, id)
		return err
	}
	if c.State == "restoring" {
		_, err = conn.Exec(ctx, `UPDATE wren_computer_checkpoints SET state='ready',failure='The restore allocation ended; saved files remain available.',updated_at=now(),next_attempt_at=now()+interval '30 seconds' WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM wren_computers WHERE restore_checkpoint_id=$1 AND phase NOT IN ('deleted','failed'))`, id)
		return err
	}
	if c.State != "creating" {
		return nil
	}
	if _, _, _, err = s.computerScope(ctx, c.WorkspaceID, c.OwnerID, c.ConversationID, true); err != nil {
		_, _ = conn.Exec(ctx, `UPDATE wren_computer_checkpoints SET state='failed',failure='The computer scope no longer permits this checkpoint.',updated_at=now() WHERE id=$1`, id)
		return err
	}
	operation, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	v, err := s.wrenComputerPrivate(operation, c.WorkspaceID, c.ComputerID)
	if err != nil {
		return err
	}
	if v.Phase != "ready" || v.DeleteRequested {
		return fmt.Errorf("computer is not ready for checkpoint")
	}
	key, err := h.config.Secrets.open(v.KeyVersion, v.KeyCiphertext, secretBinding(v.WorkspaceID, v.ID, "wren-computer-ssh"))
	if err != nil {
		return err
	}
	client, err := wrencomputer.NewSSH(wrencomputer.Config{Address: v.Address, PrivateKeyPEM: key, HostKeySHA256: v.HostFingerprint})
	clear(key)
	if err != nil {
		return err
	}
	archive, manifest, err := client.CreateCheckpoint(operation, c.ID, c.IncludeBrowser, c.DeleteAfter)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, _ = conn.Exec(ctx, `UPDATE wren_computer_checkpoints SET state='failed',failure='Saving files failed or exceeded the 32 MiB compressed/128 MiB unpacked limit. No sleep removal was requested; its original expiry still applies.',updated_at=now() WHERE id=$1`, id)
		return err
	}
	defer clear(archive)
	if c.Manifest.ArchiveSHA256 != "" && c.Manifest.ArchiveSHA256 != manifest.ArchiveSHA256 {
		return fmt.Errorf("checkpoint archive changed after storage intent")
	}
	version := c.KeyVersion
	var ciphertext []byte
	if len(c.EncryptionNonce) > 0 {
		a := h.config.Secrets.keys[version]
		if a == nil || len(c.EncryptionNonce) != a.NonceSize() {
			return errSecretUnavailable
		}
		ciphertext = a.Seal(bytes.Clone(c.EncryptionNonce), c.EncryptionNonce, archive, c.binding())
	} else {
		version, ciphertext, err = h.config.Secrets.seal(archive, c.binding())
		if err != nil {
			return err
		}
	}
	defer clear(ciphertext)
	a := h.config.Secrets.keys[version]
	nonce := bytes.Clone(ciphertext[:a.NonceSize()])
	object, err := sdk.PlanComputerCheckpointObject(c.storageScope(), ciphertext)
	if err != nil {
		return err
	}
	rawObject, _ := json.Marshal(object)
	rawManifest, _ := json.Marshal(manifest)
	// Persist the deterministic encrypted-object intent BEFORE upload. A retry
	// reuses its nonce only after verifying the immutable plaintext digest.
	if _, err = conn.Exec(ctx, `UPDATE wren_computer_checkpoints SET key_version=$2,encryption_nonce=$3,object=$4,manifest=$5,updated_at=now() WHERE id=$1`, id, version, nonce, rawObject, rawManifest); err != nil {
		return err
	}
	if _, err = engine.PutComputerCheckpoint(operation, c.storageScope(), ciphertext); err != nil {
		return err
	}
	verified, err := engine.GetComputerCheckpoint(operation, c.storageScope(), object)
	if err != nil {
		return err
	}
	clear(verified)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE wren_computer_checkpoints SET state='ready',failure='',updated_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	if c.DeleteAfter {
		if _, err = tx.Exec(ctx, `UPDATE wren_computers SET delete_requested=true,next_attempt_at=now() WHERE id=$1 AND phase='ready'`, v.ID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (h *HTTPServer) restoreWrenComputerCheckpoint(ctx context.Context, v WrenComputer, client *wrencomputer.Client) error {
	if v.RestoreCheckpointID == "" {
		return nil
	}
	c, err := scanComputerCheckpoint(h.service.Store.pool.QueryRow(ctx, `SELECT `+checkpointColumns+` FROM wren_computer_checkpoints WHERE id=$1 AND workspace_id=$2 AND conversation_id=$3 AND scope_kind=$4`, v.RestoreCheckpointID, v.WorkspaceID, v.ConversationID, v.ScopeKind))
	if err != nil {
		return err
	}
	engine, ok := h.service.Engine.(ComputerCheckpointEngine)
	if !ok {
		return fmt.Errorf("checkpoint storage unavailable")
	}
	ciphertext, err := engine.GetComputerCheckpoint(ctx, c.storageScope(), c.Object)
	if err != nil {
		return err
	}
	defer clear(ciphertext)
	archive, err := h.config.Secrets.open(c.KeyVersion, ciphertext, c.binding())
	if err != nil {
		return err
	}
	defer clear(archive)
	if err = client.RestoreCheckpoint(ctx, c.ID, archive, c.IncludeBrowser); err != nil {
		return err
	}
	_, err = h.service.Store.pool.Exec(ctx, `UPDATE wren_computer_checkpoints SET state='ready',failure='',updated_at=now() WHERE id=$1`, c.ID)
	return err
}

// PurgeWrenComputerCheckpoints must run before erasing tenant/account metadata;
// failed object deletion preserves its scoped erasure handle for a retry.
func (h *HTTPServer) PurgeWrenComputerCheckpoints(ctx context.Context, workspaces []string, privateOwner string) error {
	rows, err := h.service.Store.pool.Query(ctx, `SELECT `+checkpointColumns+` FROM wren_computer_checkpoints WHERE workspace_id=ANY($1) OR (scope_kind='private' AND owner_id=$2)`, workspaces, privateOwner)
	if err != nil {
		return err
	}
	items := []WrenComputerCheckpoint{}
	for rows.Next() {
		c, e := scanComputerCheckpoint(rows)
		if e != nil {
			rows.Close()
			return e
		}
		items = append(items, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	engine, ok := h.service.Engine.(ComputerCheckpointEngine)
	if !ok {
		return fmt.Errorf("checkpoint erasure unavailable")
	}
	for _, c := range items {
		conn, e := h.service.Store.pool.Acquire(ctx)
		if e != nil {
			return e
		}
		if _, e = conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,950))`, c.ID); e != nil {
			conn.Release()
			return e
		}
		current, e := scanComputerCheckpoint(conn.QueryRow(ctx, `SELECT `+checkpointColumns+` FROM wren_computer_checkpoints WHERE id=$1`, c.ID))
		if errors.Is(e, ErrNotFound) {
			_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,950))`, c.ID)
			conn.Release()
			continue
		}
		if e != nil {
			_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1,950))`, c.ID)
			conn.Release()
			return e
		}
		c = current
		_, e = conn.Exec(ctx, `UPDATE wren_computer_checkpoints SET state='deleting' WHERE id=$1`, c.ID)
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1,950))`, c.ID)
		conn.Release()
		if e != nil {
			return e
		}
		if c.Object.Key != "" {
			if err = engine.DeleteComputerCheckpoint(ctx, c.storageScope(), c.Object); err != nil {
				return err
			}
		}
		if _, err = h.service.Store.pool.Exec(ctx, `DELETE FROM wren_computer_checkpoints WHERE id=$1`, c.ID); err != nil {
			return err
		}
	}
	return nil
}

func (d *WrenComputerDispatcher) ReconcileCheckpoint(ctx context.Context, id string) error {
	return d.reconcileCheckpoint(ctx, id)
}
func activeComputerCheckpoint(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, computer string) (bool, error) {
	var active bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_computer_checkpoints WHERE computer_id=$1 AND state='creating')`, computer).Scan(&active)
	return active, err
}

func (h *HTTPServer) LatestSavedWrenComputerCheckpoint(ctx context.Context, c Conversation) (WrenComputerCheckpoint, error) {
	saved, err := scanComputerCheckpoint(h.service.Store.pool.QueryRow(ctx, `SELECT `+checkpointColumns+` FROM wren_computer_checkpoints WHERE workspace_id=$1 AND conversation_id=$2 AND state='ready' ORDER BY created_at DESC,id DESC LIMIT 1`, c.WorkspaceID, c.ID))
	if err != nil {
		return saved, err
	}
	return h.computerCheckpoint(ctx, c.WorkspaceID, c.AccountID, saved.ID, false)
}

func (h *HTTPServer) DeleteWrenComputerCheckpoint(ctx context.Context, workspace, account, id string) error {
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 AND disabled_at IS NULL FOR KEY SHARE`, account).Scan(&locked); err != nil {
		return ErrForbidden
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR KEY SHARE`, workspace).Scan(&locked); err != nil {
		return ErrForbidden
	}
	if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return err
	}
	c, err := scanComputerCheckpoint(tx.QueryRow(ctx, `SELECT `+checkpointColumns+` FROM wren_computer_checkpoints WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, id, workspace))
	if err != nil {
		return err
	}
	if _, _, _, err = h.service.Store.computerScope(ctx, workspace, account, c.ConversationID, true); err != nil {
		return err
	}
	if c.State == "creating" || c.State == "restoring" {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE wren_computer_checkpoints SET state='deleting',next_attempt_at=now(),updated_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
