package controlplane

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

// Personal API keys are long-lived human credentials for the CLI and SDKs.
// The plain secret is shown once at creation and is never stored: only its
// SHA-256 hash reaches the database, matching session and agent tokens.
const (
	personalAPIKeyPrefix      = "canter_pk_"
	personalAPIKeySecretBytes = 32
	personalAPIKeyMaxActive   = 50
	personalAPIKeyListLimit   = 100
	personalAPIKeyDefaultDays = 90
	personalAPIKeyMaxDays     = 365
	personalAPIKeyNameRunes   = 80
	personalAPIKeyDisplayLen  = 18
)

// validPersonalAPIToken reports whether token has the exact personal-key
// shape: the fixed prefix plus 32 random bytes in unpadded base64url.
// The length check precedes strict decoding because the non-strict decoder
// silently ignores embedded CR/LF; no other whitespace is tolerated.
// Checking the shape before the hash lookup keeps malformed input off the
// unique index and makes non-key tokens cheap to reject.
func validPersonalAPIToken(token string) bool {
	if !strings.HasPrefix(token, personalAPIKeyPrefix) {
		return false
	}
	body := strings.TrimPrefix(token, personalAPIKeyPrefix)
	if len(body) != base64.RawURLEncoding.EncodedLen(personalAPIKeySecretBytes) {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(body)
	return err == nil && len(raw) == personalAPIKeySecretBytes
}

func validatePersonalAPIKeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("key name must be printable UTF-8 without control characters")
	}
	if runes := utf8.RuneCountInString(name); runes < 1 || runes > personalAPIKeyNameRunes {
		return "", fmt.Errorf("key name must be between 1 and %d characters", personalAPIKeyNameRunes)
	}
	return name, nil
}

func validatePersonalAPIKeyExpiry(days int) (int, error) {
	if days == 0 {
		return personalAPIKeyDefaultDays, nil
	}
	if days < 1 || days > personalAPIKeyMaxDays {
		return 0, fmt.Errorf("expiresInDays must be between 1 and %d", personalAPIKeyMaxDays)
	}
	return days, nil
}

// createPersonalAPIKeyTx inserts the key under the caller's locked account
// row. The account lock serializes concurrent creations so the active-key
// cap and the session revalidation cannot race credential invalidation.
// The returned secret is the only copy ever produced; it is never persisted.
func (s *Store) createPersonalAPIKeyTx(ctx context.Context, tx pgx.Tx, a authAccount, name string, days int) (PersonalAPIKey, string, error) {
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM personal_api_keys WHERE account_id=$1 AND revoked_at IS NULL AND expires_at>$2 AND auth_version=$3`, a.ID, s.now(), a.Version).Scan(&active); err != nil {
		return PersonalAPIKey{}, "", err
	}
	if active >= personalAPIKeyMaxActive {
		return PersonalAPIKey{}, "", fmt.Errorf("%w: account already has %d active API keys", ErrCapacity, personalAPIKeyMaxActive)
	}
	secret, err := newSecret(personalAPIKeyPrefix, personalAPIKeySecretBytes)
	if err != nil {
		return PersonalAPIKey{}, "", err
	}
	id, err := newID("apk_")
	if err != nil {
		return PersonalAPIKey{}, "", err
	}
	now := s.now()
	key := PersonalAPIKey{
		ID:        id,
		Name:      name,
		Prefix:    secret[:personalAPIKeyDisplayLen],
		CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour),
	}
	if _, err := tx.Exec(ctx, `INSERT INTO personal_api_keys(id,account_id,name,token_hash,prefix,auth_version,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, key.ID, a.ID, key.Name, secretHash(secret), key.Prefix, a.Version, key.CreatedAt, key.ExpiresAt); err != nil {
		return PersonalAPIKey{}, "", err
	}
	if err := authEventTx(ctx, tx, a.ID, "API key created"); err != nil {
		return PersonalAPIKey{}, "", err
	}
	return key, secret, nil
}

// ListPersonalAPIKeys returns active keys first, then the most recent
// history, bounded at personalAPIKeyListLimit. Since the active cap is 50,
// every usable key is always listed and revocable; stale rows bumped by an
// auth_version change still appear as live here but can be revoked away.
func (s *Store) ListPersonalAPIKeys(ctx context.Context, accountID string) ([]PersonalAPIKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,name,prefix,created_at,expires_at,last_used_at,revoked_at FROM personal_api_keys WHERE account_id=$1 ORDER BY (revoked_at IS NULL AND expires_at>$2) DESC,created_at DESC,id LIMIT $3`, accountID, s.now(), personalAPIKeyListLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PersonalAPIKey{}
	for rows.Next() {
		var key PersonalAPIKey
		if err := rows.Scan(&key.ID, &key.Name, &key.Prefix, &key.CreatedAt, &key.ExpiresAt, &key.LastUsedAt, &key.RevokedAt); err != nil {
			return nil, err
		}
		result = append(result, key)
	}
	return result, rows.Err()
}

// RevokePersonalAPIKey marks the caller's own key revoked exactly once. An
// already-revoked own key is an idempotent success without a second security
// event; foreign or missing keys are indistinguishable not-found.
func (s *Store) RevokePersonalAPIKey(ctx context.Context, accountID, keyID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owner string
	var revoked *time.Time
	err = tx.QueryRow(ctx, `SELECT account_id,revoked_at FROM personal_api_keys WHERE id=$1 FOR UPDATE`, keyID).Scan(&owner, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if owner != accountID {
		return ErrNotFound
	}
	if revoked != nil {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE personal_api_keys SET revoked_at=$1 WHERE id=$2`, s.now(), keyID); err != nil {
		return err
	}
	if err := authEventTx(ctx, tx, accountID, "API key revoked"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ResolvePersonalAPIKey authenticates a personal key as its owning account.
// The principal carries no session ID: the key is not a browser session and
// must never satisfy session-scoped revalidation. Membership and account
// state are checked live on every request, so revocation, expiry, account
// disablement and credential rotation take effect immediately.
func (s *Store) ResolvePersonalAPIKey(ctx context.Context, token string) (Principal, error) {
	if !validPersonalAPIToken(token) {
		return Principal{}, ErrUnauthorized
	}
	var p Principal
	var account Account
	var key PersonalAPIKey
	err := s.pool.QueryRow(ctx, `SELECT a.id,a.email,a.created_at,k.id,k.name,k.prefix,k.created_at,k.expires_at,k.last_used_at,k.revoked_at FROM personal_api_keys k JOIN accounts a ON a.id=k.account_id WHERE k.token_hash=$1 AND k.revoked_at IS NULL AND k.expires_at>$2 AND a.disabled_at IS NULL AND a.email_verified_at IS NOT NULL AND k.auth_version=a.auth_version`, secretHash(token), s.now()).Scan(&account.ID, &account.Email, &account.CreatedAt, &key.ID, &key.Name, &key.Prefix, &key.CreatedAt, &key.ExpiresAt, &key.LastUsedAt, &key.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, err
	}
	now := s.now()
	_, _ = s.pool.Exec(ctx, `UPDATE personal_api_keys SET last_used_at=$1 WHERE id=$2 AND (last_used_at IS NULL OR last_used_at<$3)`, now, key.ID, now.Add(-time.Minute))
	p.Account = &account
	p.APIKey = &key
	p.Actor = sdk.ActorRef{Kind: "human", ID: account.ID, DisplayName: account.Email}
	return p, nil
}
