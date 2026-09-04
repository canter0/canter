package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	nodeEnrollmentLifetime = 20 * time.Minute
	nodeCredentialLifetime = 90 * 24 * time.Hour
)

func (s *Store) CreateNodeEnrollment(ctx context.Context, workspaceID, systemName, m1Prefix string) (NodeEnrollment, error) {
	if workspaceID == "" || systemName == "" || m1Prefix == "" {
		return NodeEnrollment{}, fmt.Errorf("workspace, system, and prefix are required")
	}
	nodeID, _ := newID("nod_")
	token, _ := newSecret("ce_", 32)
	now := s.now()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return NodeEnrollment{}, err
	}
	defer tx.Rollback(ctx)
	var node NodeInstallation
	err = tx.QueryRow(ctx, `INSERT INTO node_installations(id,workspace_id,system_name,m1_prefix,created_at)
		VALUES($1,$2,$3,$4,$5)
		ON CONFLICT (workspace_id,system_name) WHERE revoked_at IS NULL
		DO UPDATE SET system_name=EXCLUDED.system_name
		RETURNING id,workspace_id,system_name,m1_prefix,created_at,last_seen_at,revoked_at`, nodeID, workspaceID, systemName, m1Prefix, now).
		Scan(&node.ID, &node.WorkspaceID, &node.System, &node.M1Prefix, &node.CreatedAt, &node.LastSeenAt, &node.RevokedAt)
	if err != nil {
		return NodeEnrollment{}, err
	}
	if node.M1Prefix != m1Prefix {
		return NodeEnrollment{}, fmt.Errorf("%w: active node scope no longer matches the System prefix", ErrConflict)
	}

	// Reuse the single unconsumed enrollment row for this durable node while
	// rotating its bearer token. A fenced retry therefore cannot create a
	// second node identity, and a token abandoned by a crashed worker stops
	// being valid before any later provider attempt begins.
	var enrollmentID string
	err = tx.QueryRow(ctx, `SELECT id FROM node_enrollments WHERE node_id=$1 AND consumed_at IS NULL FOR UPDATE`, node.ID).Scan(&enrollmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		enrollmentID, _ = newID("nen_")
		_, err = tx.Exec(ctx, `INSERT INTO node_enrollments(id,node_id,token_hash,created_at,expires_at) VALUES($1,$2,$3,$4,$5)`, enrollmentID, node.ID, secretHash(token), now, now.Add(nodeEnrollmentLifetime))
	} else if err == nil {
		_, err = tx.Exec(ctx, `UPDATE node_enrollments SET token_hash=$1,created_at=$2,expires_at=$3 WHERE id=$4 AND node_id=$5 AND consumed_at IS NULL`, secretHash(token), now, now.Add(nodeEnrollmentLifetime), enrollmentID, node.ID)
	}
	if err != nil {
		return NodeEnrollment{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return NodeEnrollment{}, err
	}
	return NodeEnrollment{ID: enrollmentID, EnrollmentToken: token, ExpiresAt: now.Add(nodeEnrollmentLifetime), Node: node}, nil
}

func (s *Store) ExchangeNodeEnrollment(ctx context.Context, enrollmentID, token string) (NodeCredential, error) {
	if enrollmentID == "" || token == "" {
		return NodeCredential{}, ErrUnauthorized
	}
	now := s.now()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return NodeCredential{}, err
	}
	defer tx.Rollback(ctx)
	var node NodeInstallation
	var consumed *time.Time
	err = tx.QueryRow(ctx, `SELECT n.id,n.workspace_id,n.system_name,n.m1_prefix,n.created_at,n.last_seen_at,n.revoked_at,e.consumed_at FROM node_enrollments e JOIN node_installations n ON n.id=e.node_id WHERE e.id=$1 AND e.token_hash=$2 AND e.expires_at>$3 FOR UPDATE OF e,n`, enrollmentID, secretHash(token), now).Scan(&node.ID, &node.WorkspaceID, &node.System, &node.M1Prefix, &node.CreatedAt, &node.LastSeenAt, &node.RevokedAt, &consumed)
	if errors.Is(err, pgx.ErrNoRows) || consumed != nil || node.RevokedAt != nil {
		return NodeCredential{}, ErrUnauthorized
	}
	if err != nil {
		return NodeCredential{}, err
	}
	credentialID, _ := newID("ncr_")
	nodeToken, _ := newSecret("cn_", 32)
	expires := now.Add(nodeCredentialLifetime)
	if _, err = tx.Exec(ctx, `UPDATE node_enrollments SET consumed_at=$1 WHERE id=$2 AND consumed_at IS NULL`, now, enrollmentID); err != nil {
		return NodeCredential{}, err
	}
	// Enrollment exchange is replacement, not additive issuance. Revoking the
	// previous bearer in the same transaction ensures an orphaned host cannot
	// keep reading future desired state after a reinstall or recovery.
	if _, err = tx.Exec(ctx, `UPDATE node_credentials SET revoked_at=$1 WHERE node_id=$2 AND revoked_at IS NULL`, now, node.ID); err != nil {
		return NodeCredential{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO node_credentials(id,node_id,token_hash,created_at,expires_at) VALUES($1,$2,$3,$4,$5)`, credentialID, node.ID, secretHash(nodeToken), now, expires); err != nil {
		return NodeCredential{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return NodeCredential{}, err
	}
	return NodeCredential{NodeToken: nodeToken, ExpiresAt: expires, Node: node}, nil
}

func (s *Store) ResolveNode(ctx context.Context, token string) (NodeInstallation, error) {
	if token == "" {
		return NodeInstallation{}, ErrUnauthorized
	}
	var node NodeInstallation
	var credentialID string
	now := s.now()
	err := s.pool.QueryRow(ctx, `SELECT n.id,n.workspace_id,n.system_name,n.m1_prefix,n.created_at,n.last_seen_at,n.revoked_at,c.id FROM node_credentials c JOIN node_installations n ON n.id=c.node_id WHERE c.token_hash=$1 AND c.revoked_at IS NULL AND c.expires_at>$2 AND n.revoked_at IS NULL`, secretHash(token), now).Scan(&node.ID, &node.WorkspaceID, &node.System, &node.M1Prefix, &node.CreatedAt, &node.LastSeenAt, &node.RevokedAt, &credentialID)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeInstallation{}, ErrUnauthorized
	}
	if err != nil {
		return NodeInstallation{}, err
	}
	_, _ = s.pool.Exec(ctx, `UPDATE node_installations SET last_seen_at=$1 WHERE id=$2 AND (last_seen_at IS NULL OR last_seen_at<$3)`, now, node.ID, now.Add(-time.Minute))
	node.LastSeenAt = &now
	return node, nil
}

func (s *Store) RevokeNodeInstallation(ctx context.Context, workspaceID, nodeID string) error {
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE node_installations SET revoked_at=$1 WHERE id=$2 AND workspace_id=$3 AND revoked_at IS NULL`, now, nodeID, workspaceID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrNotFound
	}
	if _, err = tx.Exec(ctx, `UPDATE node_credentials SET revoked_at=$1 WHERE node_id=$2 AND revoked_at IS NULL`, now, nodeID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
