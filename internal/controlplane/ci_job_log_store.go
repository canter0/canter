package controlplane

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// ciJobLogScope is the frozen seven-field identity a claimed log download is
// bound to. Every field is re-read and compared inside the publish
// transaction, so a scope change between fetch and publish writes nothing.
type ciJobLogScope struct {
	Workspace            string
	Job                  string
	Runner               string
	GitHubInstallationID int64
	GitHubRepositoryID   int64
	GitHubJobID          int64
	GitHubRunID          int64
}

// ciJobLogClaim is one worker's owned collection attempt: the frozen scope,
// the random nonce fencing stale writers, and the lease deadline.
type ciJobLogClaim struct {
	Scope      ciJobLogScope
	Nonce      string
	LeaseUntil time.Time
}

type ciJobLogWorkItem struct {
	Workspace string
	Job       string
}

// CILogCollection is the read-only collection state exposed on job detail.
// It records collection proof only; it is never a job result.
type CILogCollection struct {
	State       string     `json:"state"`
	Truncated   bool       `json:"truncated"`
	CollectedAt *time.Time `json:"collectedAt"`
}

func ciJobLogIDsValid(workspace, job string) bool {
	return ciAdmissionScopeIDValid(workspace) && ciAdmissionScopeIDValid(job)
}

// ciJobLogTextValid is the publish-time guard: PostgreSQL text rejects NUL,
// splitCIJobLog requires valid UTF-8 at every boundary, and the 1 MiB bound is
// enforced before any chunk is written.
func ciJobLogTextValid(text string) bool {
	return utf8.ValidString(text) && !strings.Contains(text, "\x00") && len(text) <= ciJobLogMaxBytes
}

// ciJobLogCandidates lists at most ciJobLogWorkers eligible completed or
// cancelled managed jobs inside the bounded backfill window.
func (s *Store) ciJobLogCandidates(ctx context.Context) ([]ciJobLogWorkItem, error) {
	ctx, cancel := context.WithTimeout(ctx, ciJobLogDBBound)
	defer cancel()
	now := s.now().UTC().Truncate(time.Microsecond)
	rows, err := s.pool.Query(ctx, ciJobLogCandidatesSQL,
		now.Add(-ciJobLogBackfillWindow), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ciJobLogWorkItem
	for rows.Next() {
		var item ciJobLogWorkItem
		if err := rows.Scan(&item.Workspace, &item.Job); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// claimCIJobLog takes the workspace lock, re-reads the authoritative scope,
// and upserts a collecting lease under the caller's nonce. Unknown,
// ineligible, or still-leased jobs return ErrNotFound with no network work.
func (s *Store) claimCIJobLog(ctx context.Context, workspace, job, nonce string) (ciJobLogClaim, error) {
	if !ciJobLogIDsValid(workspace, job) || !ciLifecycleLeaseID.MatchString(nonce) {
		return ciJobLogClaim{}, ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, ciJobLogDBBound)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ciJobLogClaim{}, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, ciJobLogWorkspaceLockSQL, workspace).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ciJobLogClaim{}, ErrNotFound
		}
		return ciJobLogClaim{}, err
	}
	scope, err := ciJobLogScopeTx(ctx, tx, workspace, job)
	if err != nil {
		return ciJobLogClaim{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	leaseUntil := now.Add(ciJobLogLeaseWindow)
	var claimed string
	err = tx.QueryRow(ctx, ciJobLogClaimSQL, workspace, job, nonce, leaseUntil, now).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ciJobLogClaim{}, ErrNotFound
	}
	if err != nil {
		return ciJobLogClaim{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ciJobLogClaim{}, err
	}
	return ciJobLogClaim{Scope: scope, Nonce: nonce, LeaseUntil: leaseUntil}, nil
}

func ciJobLogScopeTx(ctx context.Context, tx pgx.Tx, workspace, job string) (ciJobLogScope, error) {
	var scope ciJobLogScope
	err := tx.QueryRow(ctx, ciJobLogScopeSQL, workspace, job).Scan(
		&scope.Workspace, &scope.Job, &scope.Runner,
		&scope.GitHubInstallationID, &scope.GitHubRepositoryID,
		&scope.GitHubJobID, &scope.GitHubRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ciJobLogScope{}, ErrNotFound
	}
	if err != nil {
		return ciJobLogScope{}, err
	}
	if scope.GitHubInstallationID <= 0 || scope.GitHubRepositoryID <= 0 ||
		scope.GitHubJobID <= 0 || scope.GitHubRunID <= 0 {
		return ciJobLogScope{}, ErrConflict
	}
	return scope, nil
}

// publishCIJobLog appends the downloaded plaintext and completes the
// collection in one transaction. The workspace lock, the reloaded seven-field
// scope, the collecting state, the exact nonce, and an unexpired lease are
// all re-verified inside the transaction; any mismatch rolls everything back
// so a stale owner can never append or complete.
func (s *Store) publishCIJobLog(ctx context.Context, claim ciJobLogClaim, text string, truncated bool) error {
	if !ciJobLogIDsValid(claim.Scope.Workspace, claim.Scope.Job) ||
		!ciLifecycleLeaseID.MatchString(claim.Nonce) ||
		!ciJobLogTextValid(text) {
		return ErrConflict
	}
	chunks := splitCIJobLog(text)
	ctx, cancel := context.WithTimeout(ctx, ciJobLogDBBound)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, ciJobLogWorkspaceLockSQL, claim.Scope.Workspace).Scan(&locked); err != nil {
		return err
	}
	fresh, err := ciJobLogScopeTx(ctx, tx, claim.Scope.Workspace, claim.Scope.Job)
	if err != nil {
		return err
	}
	if fresh != claim.Scope {
		return ErrConflict
	}
	var state, nonce string
	var leaseUntil time.Time
	err = tx.QueryRow(ctx, ciJobLogLeaseSQL, claim.Scope.Workspace, claim.Scope.Job).
		Scan(&state, &nonce, &leaseUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if state != "collecting" || nonce != claim.Nonce || !leaseUntil.After(now) {
		return ErrConflict
	}
	var next int
	if err = tx.QueryRow(ctx, ciJobLogNextSequenceSQL, claim.Scope.Workspace, claim.Scope.Job).Scan(&next); err != nil {
		return err
	}
	for _, chunk := range chunks {
		if _, err = tx.Exec(ctx, ciJobLogInsertSQL,
			claim.Scope.Workspace, claim.Scope.Job, next, chunk, now); err != nil {
			return err
		}
		next++
	}
	now = s.now().UTC().Truncate(time.Microsecond)
	tag, err := tx.Exec(ctx, ciJobLogCompleteSQL,
		claim.Scope.Workspace, claim.Scope.Job, claim.Nonce, now, truncated)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return tx.Commit(ctx)
}

// failCIJobLog marks the collection unavailable under the exact nonce so a
// stale owner can never fail a newer owner's attempt. Transport errors,
// URLs, tokens, and raw responses are never stored.
func (s *Store) failCIJobLog(ctx context.Context, claim ciJobLogClaim) error {
	if !ciJobLogIDsValid(claim.Scope.Workspace, claim.Scope.Job) ||
		!ciLifecycleLeaseID.MatchString(claim.Nonce) {
		return ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, ciJobLogDBBound)
	defer cancel()
	now := s.now().UTC().Truncate(time.Microsecond)
	_, err := s.pool.Exec(ctx, ciJobLogUnavailableSQL,
		claim.Scope.Workspace, claim.Scope.Job, claim.Nonce, now, now.Add(ciJobLogRetryInterval))
	return err
}

// CIJobLogCollection returns the stored collection state, or nil when no
// collection row exists (legacy jobs omit the field entirely).
func (s *Store) CIJobLogCollection(ctx context.Context, workspace, job string) (*CILogCollection, error) {
	if !ciJobLogIDsValid(workspace, job) {
		return nil, ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, ciJobLogDBBound)
	defer cancel()
	var out CILogCollection
	var collectedAt *time.Time
	err := s.pool.QueryRow(ctx, ciJobLogCollectionSQL, workspace, job).
		Scan(&out.State, &out.Truncated, &collectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.CollectedAt = collectedAt
	return &out, nil
}
