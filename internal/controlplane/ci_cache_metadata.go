package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// makeCICacheGrant binds a signature-verified token to the server-loaded
// admitted job and derives the immutable trust namespace. The check-run ID
// comes only from the trusted GitHub source lookup, never the guest.
func makeCICacheGrant(job ciCacheActiveJob, token ciCacheToken, checkRunID int64, now time.Time) (ciCacheGrant, error) {
	scope := job.Scope
	scope.CheckRunID = checkRunID
	if checkRunID <= 0 || job.Epoch <= 0 || job.GitHubInstallationID <= 0 ||
		job.Proof.JobID <= 0 ||
		!ciCacheIdentityText(job.WorkspaceID, 128, false) ||
		!ciCacheIdentityText(job.InstallationID, 128, false) ||
		!ciCacheIdentityText(job.RunnerID, 128, false) ||
		!ciCacheIdentityText(job.JobID, 128, false) {
		return ciCacheGrant{}, errCICacheUnauthorized
	}
	bound, err := bindCICacheIdentity(token, scope, now)
	if err != nil {
		return ciCacheGrant{}, err
	}
	namespace, err := ciCacheNamespace(job.WorkspaceID, job.InstallationID, job.Epoch, bound)
	if err != nil {
		return ciCacheGrant{}, err
	}
	return ciCacheGrant{job: job, token: token, identity: bound, namespace: namespace}, nil
}

func ciCacheHexID() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func ciCacheDigestHex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func ciCacheScanPool(row pgx.Row) (ciCachePoolState, error) {
	var out ciCachePoolState
	err := row.Scan(&out.BackendDigest, &out.Enabled, &out.MaxTotalBytes,
		&out.MaxWorkspaceBytes, &out.MaxEntryBytes, &out.MaxObjects,
		&out.MaxWorkspaceObjects, &out.TTLSeconds, &out.ReservedBytes,
		&out.ReservedObjects)
	return out, err
}

func (s *Store) ciCachePoolState(ctx context.Context) (ciCachePoolState, error) {
	return ciCacheScanPool(s.pool.QueryRow(ctx, ciCachePoolQuery))
}

// configureCICachePool is a private operator action, never reachable over
// HTTP or from user cacheEnabled. Disabling retains recorded caps so held
// objects keep their accounting until physical cleanup proves absence.
func (s *Store) configureCICachePool(ctx context.Context, cfg ciCachePoolConfig) error {
	if ctx == nil || ctx.Err() != nil ||
		!ciCacheBlobValidDigest(cfg.BackendDigest) ||
		cfg.MaxTotalBytes < 0 || cfg.MaxWorkspaceBytes < 0 || cfg.MaxEntryBytes < 0 ||
		cfg.MaxObjects < 0 || cfg.MaxWorkspaceObjects < 0 || cfg.TTLSeconds < 0 ||
		cfg.MaxEntryBytes > ciCacheBlobMaxSize ||
		cfg.MaxEntryBytes > cfg.MaxWorkspaceBytes ||
		cfg.MaxWorkspaceBytes > cfg.MaxTotalBytes ||
		cfg.MaxWorkspaceObjects > cfg.MaxObjects ||
		cfg.MaxObjects > ciCacheMaxObjects ||
		cfg.TTLSeconds > int64(ciCacheMaxRetention/time.Second) ||
		(cfg.Enabled && (cfg.MaxEntryBytes <= 0 || cfg.MaxWorkspaceObjects <= 0 || cfg.TTLSeconds <= 0)) {
		return ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := ciCacheScanPool(tx.QueryRow(ctx, ciCachePoolLockQuery)); err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	tag, err := tx.Exec(ctx, ciCacheConfigurePoolQuery,
		cfg.BackendDigest, cfg.MaxTotalBytes, cfg.MaxWorkspaceBytes, cfg.MaxEntryBytes,
		cfg.MaxObjects, cfg.MaxWorkspaceObjects, cfg.TTLSeconds, cfg.Enabled, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return tx.Commit(ctx)
}

// ciCacheRevalidateGrant reloads the actual admitted job under lock and
// proves the cached grant still describes it: same workspace, installation,
// runner, job, epoch, proof and namespace, with live deadline and token
// clock. A cached grant is never trusted indefinitely.
func (s *Store) ciCacheRevalidateGrant(ctx context.Context, q ciCacheQueryer,
	g ciCacheGrant, lock bool) (ciCacheActiveJob, error) {
	job, err := s.ciCacheActiveJob(ctx, q, g.job.Proof.JobID, g.token, lock)
	if err != nil {
		return ciCacheActiveJob{}, err
	}
	if job.WorkspaceID != g.job.WorkspaceID ||
		job.InstallationID != g.job.InstallationID ||
		job.RunnerID != g.job.RunnerID || job.JobID != g.job.JobID ||
		job.GitHubInstallationID != g.job.GitHubInstallationID ||
		job.Epoch != g.job.Epoch || job.Proof != g.job.Proof {
		return ciCacheActiveJob{}, errCICacheUnauthorized
	}
	namespace, err := ciCacheNamespace(job.WorkspaceID, job.InstallationID,
		job.Epoch, g.identity)
	if err != nil || namespace != g.namespace {
		return ciCacheActiveJob{}, errCICacheUnauthorized
	}
	if err := ciCacheCheckGrantClock(ctx, g, job.Scope.ExecutionDeadline, s.now()); err != nil {
		return ciCacheActiveJob{}, err
	}
	return job, nil
}

// reserveCICacheObject claims quota and inserts the immutable reservation.
// Any existing reserved/writing/published row for the exact key/version is a
// cache hit signal: return created=false without resetting TTL or deadline.
func (s *Store) reserveCICacheObject(ctx context.Context, g ciCacheGrant,
	backend, key, version string, size int64, sha string) (ciCacheObject, bool, error) {
	if ctx == nil || ctx.Err() != nil ||
		!ciCacheBlobValidDigest(backend) || !ciCacheBlobValidDigest(sha) ||
		!ciCacheBlobValidSize(size) ||
		!ciCacheIdentityText(key, ciCacheMaxKeyBytes, false) ||
		!ciCacheIdentityText(version, ciCacheMaxVersionBytes, false) {
		return ciCacheObject{}, false, ErrConflict
	}
	keyDigest := ciCacheDigestHex([]byte(key))
	versionDigest := ciCacheDigestHex([]byte(version))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ciCacheObject{}, false, err
	}
	defer tx.Rollback(ctx)
	var lockedWorkspace string
	if err := tx.QueryRow(ctx, ciCacheLockWorkspaceQuery,
		g.job.WorkspaceID).Scan(&lockedWorkspace); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return ciCacheObject{}, false, err
	}
	job, err := s.ciCacheRevalidateGrant(ctx, tx, g, true)
	if err != nil {
		return ciCacheObject{}, false, err
	}
	pool, err := ciCacheScanPool(tx.QueryRow(ctx, ciCachePoolLockQuery))
	if err != nil {
		return ciCacheObject{}, false, err
	}
	if !pool.Enabled || pool.BackendDigest != backend {
		return ciCacheObject{}, false, ErrCapacity
	}
	// Lock waits can consume the token's remaining age; re-check the signed
	// clock against the freshly loaded job deadline before mutating.
	if err := ciCacheCheckGrantClock(ctx, g, job.Scope.ExecutionDeadline, s.now()); err != nil {
		return ciCacheObject{}, false, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if _, err := tx.Exec(ctx, ciCacheEnsureNamespaceQuery,
		g.namespace, g.job.WorkspaceID, g.job.InstallationID,
		g.identity.RepositoryID, job.Epoch, now); err != nil {
		return ciCacheObject{}, false, err
	}
	var scopeWorkspace, scopeInstallation string
	var scopeRepository, scopeEpoch int64
	if err := tx.QueryRow(ctx, ciCacheNamespaceScopeQuery, g.namespace).
		Scan(&scopeWorkspace, &scopeInstallation, &scopeRepository, &scopeEpoch); err != nil {
		return ciCacheObject{}, false, err
	}
	if scopeWorkspace != g.job.WorkspaceID || scopeInstallation != g.job.InstallationID ||
		scopeRepository != g.identity.RepositoryID || scopeEpoch != job.Epoch {
		return ciCacheObject{}, false, errCICacheUnauthorized
	}
	var existingID, existingState string
	err = tx.QueryRow(ctx, ciCacheExistingKeyQuery,
		g.namespace, keyDigest, versionDigest).Scan(&existingID, &existingState)
	if err == nil {
		return ciCacheObject{}, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ciCacheObject{}, false, err
	}
	if _, err := tx.Exec(ctx, ciCacheEnsureUsageQuery, g.job.WorkspaceID); err != nil {
		return ciCacheObject{}, false, err
	}
	var reservedBytes, reservedObjects int64
	if err := tx.QueryRow(ctx, ciCacheLockUsageQuery, g.job.WorkspaceID).
		Scan(&reservedBytes, &reservedObjects); err != nil {
		return ciCacheObject{}, false, err
	}
	tag, err := tx.Exec(ctx, ciCacheReservePoolQuery, backend, size, now)
	if err != nil {
		return ciCacheObject{}, false, err
	}
	if tag.RowsAffected() != 1 {
		return ciCacheObject{}, false, ErrCapacity
	}
	tag, err = tx.Exec(ctx, ciCacheReserveUsageQuery,
		g.job.WorkspaceID, size, pool.MaxWorkspaceBytes, pool.MaxWorkspaceObjects)
	if err != nil {
		return ciCacheObject{}, false, err
	}
	if tag.RowsAffected() != 1 {
		return ciCacheObject{}, false, ErrCapacity
	}
	id, err := ciCacheHexID()
	if err != nil {
		return ciCacheObject{}, false, err
	}
	expiresAt := now.Add(time.Duration(pool.TTLSeconds) * time.Second)
	uploadDeadline := now.Add(ciCacheMaxOperation)
	for _, candidate := range []time.Time{expiresAt, g.identity.ExpiresAt,
		job.Scope.ExecutionDeadline} {
		if candidate.Before(uploadDeadline) {
			uploadDeadline = candidate
		}
	}
	if !uploadDeadline.After(now) {
		return ciCacheObject{}, false, ErrConflict
	}
	if _, err := tx.Exec(ctx, ciCacheInsertObjectQuery,
		id, g.job.WorkspaceID, g.namespace, keyDigest, versionDigest, backend,
		g.job.RunnerID, g.job.JobID, size, sha, uploadDeadline, expiresAt, now); err != nil {
		return ciCacheObject{}, false, err
	}
	if err := ciCacheCheckGrantClock(ctx, g, job.Scope.ExecutionDeadline, s.now()); err != nil {
		return ciCacheObject{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ciCacheObject{}, false, err
	}
	return ciCacheObject{
		ID: id, SizeBytes: size, SHA256: sha, State: "reserved",
		KeyDigest: keyDigest, VersionDigest: versionDigest,
		UploadDeadlineAt: uploadDeadline, ExpiresAt: expiresAt,
	}, true, nil
}

func ciCacheScanObject(row pgx.Row) (ciCacheObject, error) {
	var out ciCacheObject
	err := row.Scan(&out.ID, &out.SizeBytes, &out.SHA256, &out.State,
		&out.WriterToken, &out.UploadDeadlineAt, &out.ExpiresAt,
		&out.KeyDigest, &out.VersionDigest)
	return out, err
}

// ciCacheUploadObject revalidates the grant and returns the caller's own
// reserved object so the HTTP layer can hand its immutable metadata to
// Blob.Put's authorize callback.
func (s *Store) ciCacheUploadObject(ctx context.Context, g ciCacheGrant,
	backend, id string) (ciCacheObject, error) {
	if ctx == nil || ctx.Err() != nil ||
		!ciCacheBlobValidDigest(backend) || !ciCacheBlobValidID(id) {
		return ciCacheObject{}, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ciCacheObject{}, err
	}
	defer tx.Rollback(ctx)
	var lockedWorkspace string
	if err := tx.QueryRow(ctx, ciCacheLockWorkspaceQuery,
		g.job.WorkspaceID).Scan(&lockedWorkspace); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return ciCacheObject{}, err
	}
	job, err := s.ciCacheRevalidateGrant(ctx, tx, g, false)
	if err != nil {
		return ciCacheObject{}, err
	}
	pool, err := ciCacheScanPool(tx.QueryRow(ctx, ciCachePoolLockQuery))
	if err != nil {
		return ciCacheObject{}, err
	}
	if !pool.Enabled || pool.BackendDigest != backend {
		return ciCacheObject{}, ErrCapacity
	}
	if err := ciCacheCheckGrantClock(ctx, g, job.Scope.ExecutionDeadline, s.now()); err != nil {
		return ciCacheObject{}, err
	}
	object, err := ciCacheScanObject(tx.QueryRow(ctx, ciCacheObjectForJobQuery,
		id, g.job.WorkspaceID, g.namespace, backend,
		g.job.RunnerID, g.job.JobID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return ciCacheObject{}, err
	}
	if object.State != "reserved" && object.State != "writing" && object.State != "published" {
		return ciCacheObject{}, ErrConflict
	}
	if err := ciCacheCheckGrantClock(ctx, g, job.Scope.ExecutionDeadline, s.now()); err != nil {
		return ciCacheObject{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ciCacheObject{}, err
	}
	return object, nil
}

// claimCICacheWrite is invoked ONLY inside Blob.Put's authorize callback
// while the filesystem object lock is held. It never runs before that lock.
func (s *Store) claimCICacheWrite(ctx context.Context, g ciCacheGrant,
	backend, id, writerToken string) (ciCacheObject, error) {
	if ctx == nil || ctx.Err() != nil ||
		!ciCacheBlobValidDigest(backend) || !ciCacheBlobValidID(id) ||
		!ciCacheBlobValidDigest(writerToken) {
		return ciCacheObject{}, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ciCacheObject{}, err
	}
	defer tx.Rollback(ctx)
	var lockedWorkspace string
	if err := tx.QueryRow(ctx, ciCacheLockWorkspaceQuery,
		g.job.WorkspaceID).Scan(&lockedWorkspace); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return ciCacheObject{}, err
	}
	job, err := s.ciCacheRevalidateGrant(ctx, tx, g, true)
	if err != nil {
		return ciCacheObject{}, err
	}
	pool, err := ciCacheScanPool(tx.QueryRow(ctx, ciCachePoolLockQuery))
	if err != nil {
		return ciCacheObject{}, err
	}
	if !pool.Enabled || pool.BackendDigest != backend {
		return ciCacheObject{}, ErrCapacity
	}
	if err := ciCacheCheckGrantClock(ctx, g, job.Scope.ExecutionDeadline, s.now()); err != nil {
		return ciCacheObject{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	var sizeBytes int64
	var sha, keyDigest, versionDigest string
	err = tx.QueryRow(ctx, ciCacheClaimWriteQuery,
		id, g.job.WorkspaceID, g.namespace, backend,
		g.job.RunnerID, g.job.JobID, writerToken, now).
		Scan(&sizeBytes, &sha, &keyDigest, &versionDigest)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrConflict
		}
		return ciCacheObject{}, err
	}
	if err := ciCacheCheckGrantClock(ctx, g, job.Scope.ExecutionDeadline, s.now()); err != nil {
		return ciCacheObject{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ciCacheObject{}, err
	}
	return ciCacheObject{
		ID: id, SizeBytes: sizeBytes, SHA256: sha, State: "writing",
		WriterToken: writerToken, KeyDigest: keyDigest, VersionDigest: versionDigest,
	}, nil
}

// publishCICacheObject runs only after a sealed Blob.Put. An immutable key
// conflict invalidates only this object and its entry; quota is never
// refunded until physical absence is proven.
func (s *Store) publishCICacheObject(ctx context.Context, g ciCacheGrant,
	backend, id, writerToken string) (bool, error) {
	if ctx == nil || ctx.Err() != nil ||
		!ciCacheBlobValidDigest(backend) || !ciCacheBlobValidID(id) ||
		!ciCacheBlobValidDigest(writerToken) {
		return false, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var lockedWorkspace string
	if err := tx.QueryRow(ctx, ciCacheLockWorkspaceQuery,
		g.job.WorkspaceID).Scan(&lockedWorkspace); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return false, err
	}
	job, err := s.ciCacheRevalidateGrant(ctx, tx, g, true)
	if err != nil {
		return false, err
	}
	pool, err := ciCacheScanPool(tx.QueryRow(ctx, ciCachePoolLockQuery))
	if err != nil {
		return false, err
	}
	if !pool.Enabled || pool.BackendDigest != backend {
		return false, ErrCapacity
	}
	if err := ciCacheCheckGrantClock(ctx, g, job.Scope.ExecutionDeadline, s.now()); err != nil {
		return false, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	object, err := ciCacheScanObject(tx.QueryRow(ctx, ciCacheObjectForJobLockQuery,
		id, g.job.WorkspaceID, g.namespace, backend,
		g.job.RunnerID, g.job.JobID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return false, err
	}
	if object.State == "published" {
		if err := ciCacheCheckGrantClock(ctx, g, job.Scope.ExecutionDeadline, s.now()); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	if object.State != "writing" || object.WriterToken != writerToken ||
		!object.UploadDeadlineAt.After(now) || !object.ExpiresAt.After(now) {
		return false, ErrConflict
	}
	var entryObject string
	err = tx.QueryRow(ctx, ciCachePublishEntryQuery, id, writerToken, now).
		Scan(&entryObject)
	if errors.Is(err, pgx.ErrNoRows) {
		// Immutable key conflict: invalidate only this object and drop its
		// entry, then commit false. Quota stays held until physical absence.
		if _, err := tx.Exec(ctx, ciCacheInvalidateObjectQuery,
			id, g.namespace, backend, now); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, ciCacheRemoveEntryQuery, id); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, ciCachePublishObjectQuery, id, writerToken, now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, ErrConflict
	}
	if err := ciCacheCheckGrantClock(ctx, g, job.Scope.ExecutionDeadline, s.now()); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func ciCacheEntryFromRow(row pgx.Row) (ciCacheEntry, error) {
	var out ciCacheEntry
	err := row.Scan(&out.ObjectID, &out.BackendDigest, &out.SizeBytes,
		&out.SHA256, &out.ExpiresAt)
	return out, err
}

func (s *Store) ciCacheBeginGrantTx(ctx context.Context, g ciCacheGrant) (pgx.Tx, ciCacheActiveJob, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, ciCacheActiveJob{}, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, ciCacheActiveJob{}, err
	}
	var lockedWorkspace string
	if err := tx.QueryRow(ctx, ciCacheLockWorkspaceQuery,
		g.job.WorkspaceID).Scan(&lockedWorkspace); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		tx.Rollback(ctx)
		return nil, ciCacheActiveJob{}, err
	}
	job, err := s.ciCacheRevalidateGrant(ctx, tx, g, false)
	if err != nil {
		tx.Rollback(ctx)
		return nil, ciCacheActiveJob{}, err
	}
	return tx, job, nil
}

// lookupCICacheEntry is an exact key/version match inside the grant's trust
// namespace only; there is never a cross-scope fallback.
func (s *Store) lookupCICacheEntry(ctx context.Context, g ciCacheGrant,
	backend, key, version string) (ciCacheEntry, bool, error) {
	if ctx == nil || ctx.Err() != nil ||
		!ciCacheBlobValidDigest(backend) ||
		!ciCacheIdentityText(key, ciCacheMaxKeyBytes, false) ||
		!ciCacheIdentityText(version, ciCacheMaxVersionBytes, false) {
		return ciCacheEntry{}, false, ErrConflict
	}
	tx, job, err := s.ciCacheBeginGrantTx(ctx, g)
	if err != nil {
		return ciCacheEntry{}, false, err
	}
	defer tx.Rollback(ctx)
	keyDigest := ciCacheDigestHex([]byte(key))
	versionDigest := ciCacheDigestHex([]byte(version))
	entry, err := ciCacheEntryFromRow(tx.QueryRow(ctx, ciCacheLookupEntryQuery,
		g.namespace, g.job.WorkspaceID, g.job.InstallationID,
		g.identity.RepositoryID, g.job.Epoch, keyDigest, versionDigest,
		backend, s.now()))
	if errors.Is(err, pgx.ErrNoRows) {
		return ciCacheEntry{}, false, nil
	}
	if err != nil {
		return ciCacheEntry{}, false, err
	}
	if err := ciCacheCheckGrantClock(ctx, g, job.Scope.ExecutionDeadline, s.now()); err != nil {
		return ciCacheEntry{}, false, err
	}
	return entry, true, tx.Commit(ctx)
}

// ciCacheDownloadEntry authorizes a blob download for an object inside the
// grant's exact trust scope; nothing outside it is ever returned.
func (s *Store) ciCacheDownloadEntry(ctx context.Context, g ciCacheGrant,
	backend, objectID string) (ciCacheEntry, error) {
	if ctx == nil || ctx.Err() != nil ||
		!ciCacheBlobValidDigest(backend) || !ciCacheBlobValidID(objectID) {
		return ciCacheEntry{}, ErrConflict
	}
	tx, job, err := s.ciCacheBeginGrantTx(ctx, g)
	if err != nil {
		return ciCacheEntry{}, err
	}
	defer tx.Rollback(ctx)
	entry, err := ciCacheEntryFromRow(tx.QueryRow(ctx, ciCacheEntryForObjectQuery,
		objectID, g.namespace, g.job.WorkspaceID, g.job.InstallationID,
		g.identity.RepositoryID, g.job.Epoch, backend, s.now()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return ciCacheEntry{}, err
	}
	if err := ciCacheCheckGrantClock(ctx, g, job.Scope.ExecutionDeadline, s.now()); err != nil {
		return ciCacheEntry{}, err
	}
	return entry, tx.Commit(ctx)
}

// invalidateCICacheObject is internal failure/corruption cleanup. It uses the
// already-authenticated grant namespace and deliberately does not require the
// job to still be active, so cancellation can never block cleanup. Quota is
// never refunded here; only proven physical absence releases it.
func (s *Store) invalidateCICacheObject(ctx context.Context, g ciCacheGrant,
	backend, id string) error {
	if ctx == nil || ctx.Err() != nil ||
		!ciCacheBlobValidDigest(backend) || !ciCacheBlobValidID(id) {
		return ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := ciCacheScanPool(tx.QueryRow(ctx, ciCachePoolLockQuery)); err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	tag, err := tx.Exec(ctx, ciCacheInvalidateObjectQuery,
		id, g.namespace, backend, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, ciCacheRemoveEntryQuery, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// claimCICacheCleanup leases cleanup candidates and removes their entries in
// one transaction, committed before any file deletion. Published objects
// outlive their source job; only TTL/policy expiry or incomplete writes are
// claimed.
func (s *Store) claimCICacheCleanup(ctx context.Context, backend string, batch int) ([]ciCacheCleanupClaim, error) {
	if ctx == nil || ctx.Err() != nil || batch < 1 || batch > ciCacheCleanupWorkers ||
		!ciCacheBlobValidDigest(backend) {
		return nil, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	now := s.now().UTC().Truncate(time.Microsecond)
	rows, err := tx.Query(ctx, ciCacheCleanupCandidatesQuery, backend, now, batch)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id, workspace string
		size          int64
		state         string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.workspace, &c.size, &c.state); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	claims := make([]ciCacheCleanupClaim, 0, len(candidates))
	leaseUntil := now.Add(ciCacheCleanupLease)
	for _, c := range candidates {
		token, err := ciCacheHexID()
		if err != nil {
			return nil, err
		}
		tag, err := tx.Exec(ctx, ciCacheClaimCleanupQuery,
			c.id, backend, token, now, leaseUntil)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() != 1 {
			continue
		}
		if _, err := tx.Exec(ctx, ciCacheRemoveEntryQuery, c.id); err != nil {
			return nil, err
		}
		claims = append(claims, ciCacheCleanupClaim{
			ObjectID: c.id, WorkspaceID: c.workspace, SizeBytes: c.size,
			BackendDigest: backend, CleanupToken: token,
		})
	}
	return claims, tx.Commit(ctx)
}

// ackCICacheAbsent runs only after a fresh Blob.Delete proved both pending
// and final names absent for this exact backend/id/cleanup token. Both quota
// ledgers decrement exactly once, even if the workspace row is already gone.
func (s *Store) ackCICacheAbsent(ctx context.Context, claim ciCacheCleanupClaim) error {
	if ctx == nil || ctx.Err() != nil ||
		!ciCacheBlobValidID(claim.ObjectID) ||
		!ciCacheBlobValidDigest(claim.BackendDigest) ||
		!ciCacheBlobValidDigest(claim.CleanupToken) ||
		claim.SizeBytes < 1 || claim.WorkspaceID == "" {
		return ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := ciCacheScanPool(tx.QueryRow(ctx, ciCachePoolLockQuery)); err != nil {
		return err
	}
	var workspaceID string
	var sizeBytes int64
	err = tx.QueryRow(ctx, ciCacheLockAbsentObjectQuery,
		claim.ObjectID, claim.BackendDigest, claim.CleanupToken).
		Scan(&workspaceID, &sizeBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		var already bool
		if qerr := tx.QueryRow(ctx, ciCacheAlreadyDeletedQuery,
			claim.ObjectID, claim.BackendDigest).Scan(&already); qerr == nil && already {
			return nil
		}
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if workspaceID != claim.WorkspaceID || sizeBytes != claim.SizeBytes {
		return ErrConflict
	}
	var reservedBytes, reservedObjects int64
	if err := tx.QueryRow(ctx, ciCacheLockUsageQuery, workspaceID).
		Scan(&reservedBytes, &reservedObjects); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		return err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	tag, err := tx.Exec(ctx, ciCacheReleasePoolQuery,
		claim.BackendDigest, claim.SizeBytes, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	tag, err = tx.Exec(ctx, ciCacheReleaseUsageQuery, workspaceID, claim.SizeBytes)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, ciCacheRemoveEntryQuery, claim.ObjectID); err != nil {
		return err
	}
	tag, err = tx.Exec(ctx, ciCacheAckAbsentQuery,
		claim.ObjectID, claim.BackendDigest, claim.CleanupToken, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, ciCachePruneNamespacesQuery); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// retryCICacheCleanup defers an uncertain deletion; it never refunds quota
// and cannot erase a lease owned by another token.
func (s *Store) retryCICacheCleanup(ctx context.Context, claim ciCacheCleanupClaim) error {
	if ctx == nil || ctx.Err() != nil ||
		!ciCacheBlobValidID(claim.ObjectID) ||
		!ciCacheBlobValidDigest(claim.BackendDigest) ||
		!ciCacheBlobValidDigest(claim.CleanupToken) {
		return ErrConflict
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	tag, err := s.pool.Exec(ctx, ciCacheRetryCleanupQuery,
		claim.ObjectID, claim.BackendDigest, claim.CleanupToken, now,
		now.Add(ciCacheCleanupRetry))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}
