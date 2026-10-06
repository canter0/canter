package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (h *HTTPServer) deleteAccountTx(ctx context.Context, tx pgx.Tx, a authAccount, privateWorkspaces []string) error {
	now := h.service.Store.now()
	// Credential grants made by this identity stop working, including in shared
	// workspaces. Another owner can explicitly authorize replacement agents.
	for _, query := range []string{
		`UPDATE change_approval_capabilities SET revoked_at=$2 WHERE requested_by_installation IN (SELECT id FROM agent_installations WHERE created_by=$1) AND consumed_at IS NULL AND revoked_at IS NULL`,
		`UPDATE agent_sessions SET ended_at=COALESCE(ended_at,$2) WHERE installation_id IN (SELECT id FROM agent_installations WHERE created_by=$1)`,
		`UPDATE agent_credentials SET revoked_at=COALESCE(revoked_at,$2) WHERE installation_id IN (SELECT id FROM agent_installations WHERE created_by=$1)`,
		`UPDATE agent_installations SET revoked_at=COALESCE(revoked_at,$2) WHERE created_by=$1`,
		`UPDATE standing_policies SET revoked_at=COALESCE(revoked_at,$2) WHERE created_by_account=$1`,
	} {
		if _, err := tx.Exec(ctx, query, a.ID, now); err != nil {
			return err
		}
	}
	for _, workspace := range privateWorkspaces {
		if err := deletePrivateWorkspaceTx(ctx, tx, workspace); err != nil {
			return err
		}
	}
	// Free-text prompts and immutable deployment records in shared workspaces
	// belong to that workspace. Remove direct identity/session references while
	// retaining its operational history and financial records.
	for _, query := range []string{
		`UPDATE audit_events SET actor_id='deleted-account',session_id='' WHERE actor_kind='human' AND actor_id=$1`,
		`UPDATE executions SET requested_by_id='deleted-account',requested_session_id='' WHERE requested_by_kind='human' AND requested_by_id=$1`,
		`UPDATE initial_deployment_executions SET requested_by_id='deleted-account',requested_session_id='' WHERE requested_by_kind='human' AND requested_by_id=$1`,
		`UPDATE deployment_artifacts SET uploaded_by_id='deleted-account',uploaded_session_id='' WHERE uploaded_by_kind='human' AND uploaded_by_id=$1`,
		`UPDATE beta_invites SET consumed_by=NULL WHERE consumed_by=$1`,
		`DELETE FROM account_acquisition WHERE account_id=$1`,
	} {
		if _, err := tx.Exec(ctx, query, a.ID); err != nil {
			return err
		}
	}
	if err := h.deleteLegacyAccountMailTx(ctx, tx, a.Email); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM auth_email_outbox WHERE account_id=$1 OR recipient_hash=$2`, a.ID, secretHash(a.Email)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM auth_challenges WHERE email=$1`, a.Email); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM auth_email_suppressions WHERE email_hash=$1`, secretHash(a.Email)); err != nil {
		return err
	}
	// The FK cascades erase sessions, credentials, factors, recovery codes,
	// connected accounts, private conversations (including attachments), and
	// acquisition/security data. No disabled account or reusable password stays.
	_, err := tx.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, a.ID)
	return err
}

func (h *HTTPServer) deleteLegacyAccountMailTx(ctx context.Context, tx pgx.Tx, email string) error {
	if h.config.Secrets == nil {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id,key_id,ciphertext FROM auth_email_outbox WHERE account_id IS NULL AND recipient_hash IS NULL AND ciphertext IS NOT NULL FOR UPDATE`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id, key string
		var sealed []byte
		if err = rows.Scan(&id, &key, &sealed); err != nil {
			break
		}
		var raw []byte
		raw, err = h.config.Secrets.open(key, sealed, []byte("canter:auth-mail:"+id))
		if err != nil {
			break
		}
		var mail AuthEmail
		err = json.Unmarshal(raw, &mail)
		clear(raw)
		if err != nil {
			break
		}
		if strings.EqualFold(mail.To, email) {
			ids = append(ids, id)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		_, err = tx.Exec(ctx, `DELETE FROM auth_email_outbox WHERE id=ANY($1)`, ids)
	}
	return err
}

func deletePrivateWorkspaceTx(ctx context.Context, tx pgx.Tx, workspace string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO account_deletion_artifacts(storage_key) SELECT storage_key FROM deployment_artifacts WHERE workspace_id=$1 ON CONFLICT DO NOTHING`, workspace); err != nil {
		return err
	}
	// Delete children first; shared workspace FKs intentionally do not cascade
	// arbitrary resource deletion. The policy check has already fenced live work.
	for _, query := range []string{
		`DELETE FROM change_policy_decisions WHERE workspace_id=$1`,
		`DELETE FROM change_approval_capabilities WHERE workspace_id=$1`,
		`DELETE FROM standing_policies WHERE workspace_id=$1`,
		`DELETE FROM executions WHERE workspace_id=$1`,
		`DELETE FROM change_records WHERE workspace_id=$1`,
		`DELETE FROM initial_deployment_executions WHERE workspace_id=$1`,
		`DELETE FROM initial_deployments WHERE workspace_id=$1`,
		`DELETE FROM workspace_tasks WHERE workspace_id=$1`,
		`DELETE FROM operator_grants WHERE workspace_id=$1`,
		`DELETE FROM operator_conversations WHERE workspace_id=$1`,
		`DELETE FROM agent_pairings WHERE workspace_id=$1`,
		`DELETE FROM device_authorizations WHERE workspace_id=$1`,
		`DELETE FROM agent_sessions WHERE installation_id IN (SELECT id FROM agent_installations WHERE workspace_id=$1)`,
		`DELETE FROM agent_credentials WHERE installation_id IN (SELECT id FROM agent_installations WHERE workspace_id=$1)`,
		`DELETE FROM agent_installations WHERE workspace_id=$1`,
		`DELETE FROM node_installations WHERE workspace_id=$1`,
		`DELETE FROM workspace_secrets WHERE workspace_id=$1`,
		`DELETE FROM usage_reservations WHERE workspace_id=$1`,
		`DELETE FROM workspace_usage_caps WHERE workspace_id=$1`,
		`DELETE FROM workspace_vps WHERE workspace_id=$1`,
		`DELETE FROM systems WHERE workspace_id=$1`,
		`DELETE FROM deployment_artifacts WHERE workspace_id=$1`,
		`DELETE FROM account_acquisition WHERE workspace_id=$1`,
		`DELETE FROM audit_events WHERE workspace_id=$1`,
		`DELETE FROM memberships WHERE workspace_id=$1`,
	} {
		if _, err := tx.Exec(ctx, query, workspace); err != nil {
			return err
		}
	}
	var financial bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_billing WHERE workspace_id=$1 AND customer_id IS NOT NULL) OR EXISTS(SELECT 1 FROM billing_paid_invoices WHERE workspace_id=$1) OR EXISTS(SELECT 1 FROM billing_usage_events WHERE workspace_id=$1)`, workspace).Scan(&financial); err != nil {
		return err
	}
	if financial {
		// An inaccessible shell preserves reconciled billing references, rather
		// than destroying invoices or reopening a canceled subscription.
		if _, err := tx.Exec(ctx, `UPDATE workspace_billing SET checkout_url='',checkout_id='',checkout_plan='',checkout_expires_at=NULL WHERE workspace_id=$1`, workspace); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE workspaces SET name='Closed workspace' WHERE id=$1`, workspace)
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, workspace)
	return err
}

type accountArtifactEraser interface {
	DeleteControlPlaneArtifact(context.Context, string) error
}

func (h *HTTPServer) eraseDeletedAccountArtifact(ctx context.Context) (bool, error) {
	engine, ok := h.service.Engine.(accountArtifactEraser)
	if !ok {
		return false, nil
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var key string
	var attempts int
	err = tx.QueryRow(ctx, `SELECT storage_key,attempts FROM account_deletion_artifacts WHERE next_attempt_at<=now() ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&key, &attempts)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Uploads hold the same digest lock from before remote staging until their
	// ownership row commits, so an erasure cannot race a new owner's upload.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,929))`, key); err != nil {
		return false, err
	}
	var referenced bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_artifacts WHERE storage_key=$1)`, key).Scan(&referenced); err != nil {
		return false, err
	}
	if !referenced {
		eraseCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = engine.DeleteControlPlaneArtifact(eraseCtx, key)
		cancel()
	}
	if err != nil {
		// Keep only the canonical key and retry schedule, never provider errors
		// that might contain credentials or the deleted person's identity.
		delay := time.Minute * time.Duration(1<<min(attempts, 6))
		_, retryErr := tx.Exec(ctx, `UPDATE account_deletion_artifacts SET attempts=attempts+1,next_attempt_at=$2 WHERE storage_key=$1`, key, h.service.Store.now().Add(delay))
		if retryErr != nil {
			return false, retryErr
		}
		return true, tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM account_deletion_artifacts WHERE storage_key=$1`, key); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
