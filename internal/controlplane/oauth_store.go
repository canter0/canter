package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type oauthLoginState struct {
	Provider, Verifier, Nonce, Next, Mode string
	InviteHash                            []byte
	LinkAccountID                         *string
	WorkspaceID                           *string
}

func (s *Store) beginOAuth(ctx context.Context, state, browser string, login oauthLoginState) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM oauth_login_states WHERE expires_at <= $1`, s.now()); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO oauth_login_states(state_hash,browser_hash,provider,verifier,nonce,next_path,mode,invite_hash,link_account_id,expires_at,workspace_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, secretHash(state), secretHash(browser), login.Provider, login.Verifier, login.Nonce, login.Next, login.Mode, login.InviteHash, login.LinkAccountID, s.now().Add(10*time.Minute), login.WorkspaceID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) consumeOAuth(ctx context.Context, state, browser, provider string) (oauthLoginState, error) {
	var login oauthLoginState
	err := s.pool.QueryRow(ctx, `DELETE FROM oauth_login_states WHERE state_hash=$1 AND browser_hash=$2 AND provider=$3 AND expires_at>$4 RETURNING provider,verifier,nonce,next_path,mode,invite_hash,link_account_id,workspace_id`, secretHash(state), secretHash(browser), provider, s.now()).Scan(&login.Provider, &login.Verifier, &login.Nonce, &login.Next, &login.Mode, &login.InviteHash, &login.LinkAccountID, &login.WorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrUnauthorized
	}
	return login, err
}

var errOAuthAccountExists = errors.New("an account with this email already exists")

// OAuth identities are keyed by the provider's stable subject, never by email.
// An existing account must authenticate before attaching a new identity.
func (s *Store) signinOAuth(ctx context.Context, identity oauthIdentity, login oauthLoginState, requireInvite bool) (string, error) {
	if identity.Subject == "" || (identity.Provider != "google" && identity.Provider != "github") {
		return "", ErrUnauthorized
	}
	email, err := normalizeEmail(identity.Email)
	if err != nil {
		return "", ErrUnauthorized
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var accountID string
	var disabled *time.Time
	err = tx.QueryRow(ctx, `SELECT a.id,a.disabled_at FROM oauth_identities oi JOIN accounts a ON a.id=oi.account_id WHERE oi.provider=$1 AND oi.subject=$2 FOR UPDATE OF a`, identity.Provider, identity.Subject).Scan(&accountID, &disabled)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err == nil {
		if disabled != nil || (login.LinkAccountID != nil && *login.LinkAccountID != accountID) {
			return "", ErrForbidden
		}
	} else {
		if login.LinkAccountID != nil {
			err = tx.QueryRow(ctx, `SELECT id,disabled_at FROM accounts WHERE id=$1 FOR UPDATE`, *login.LinkAccountID).Scan(&accountID, &disabled)
			if err != nil || disabled != nil {
				return "", ErrForbidden
			}
		} else {
			var exists bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE email=$1)`, email).Scan(&exists); err != nil {
				return "", err
			}
			if exists {
				return "", errOAuthAccountExists
			}
			accountID, err = newID("usr_")
			if err != nil {
				return "", err
			}
			workspaceID, err := newID("wrk_")
			if err != nil {
				return "", err
			}
			if requireInvite {
				result, err := tx.Exec(ctx, `UPDATE beta_invites SET consumed_by=$1,consumed_at=$2 WHERE key_hash=$3 AND consumed_at IS NULL`, accountID, s.now(), login.InviteHash)
				if err != nil {
					return "", err
				}
				if result.RowsAffected() != 1 {
					return "", ErrForbidden
				}
			}
			// This sentinel cannot pass password verification. No password is generated or exposed.
			if _, err = tx.Exec(ctx, `INSERT INTO accounts(id,email,password_hash,created_at) VALUES($1,$2,'!oauth',$3)`, accountID, email, s.now()); err != nil {
				return "", err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO workspaces(id,name,created_at) VALUES($1,'default',$2)`, workspaceID, s.now()); err != nil {
				return "", err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO workspace_usage_caps(workspace_id,limit_cents,created_at,updated_at) VALUES($1,500,$2,$2)`, workspaceID, s.now()); err != nil {
				return "", err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'owner')`, accountID, workspaceID); err != nil {
				return "", err
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO oauth_identities(provider,subject,account_id,email,created_at) VALUES($1,$2,$3,$4,$5)`, identity.Provider, identity.Subject, accountID, email, s.now()); err != nil {
			return "", fmt.Errorf("%w: this provider is already connected", ErrConflict)
		}
	}
	sessionID, err := newID("hss_")
	if err != nil {
		return "", err
	}
	token, err := newSecret("chs_", 32)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO human_sessions(id,account_id,token_hash,created_at,last_seen_at,expires_at) VALUES($1,$2,$3,$4,$4,$5)`, sessionID, accountID, secretHash(token), s.now(), s.now().Add(7*24*time.Hour))
	if err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return token, nil
}
