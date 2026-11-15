package controlplane

// Cache mutations lock the workspace, then pool, then object/usage rows. The
// filesystem authorize callback claims a write while holding the blob lock.
// Cleanup commits its claim before acquiring that same blob lock.
const (
	ciCachePoolQuery = `
SELECT backend_digest,enabled,max_total_bytes,max_workspace_bytes,max_entry_bytes,
       max_objects,max_workspace_objects,ttl_seconds,reserved_bytes,reserved_objects
FROM ci_cache_pool WHERE singleton`
	ciCachePoolLockQuery = ciCachePoolQuery + ` FOR UPDATE`

	// $1 backend; $2 total bytes; $3 workspace bytes; $4 entry bytes;
	// $5 objects; $6 workspace objects; $7 TTL seconds; $8 enabled; $9 now.
	// A backend cannot be replaced while any old physical objects remain.
	ciCacheConfigurePoolQuery = `
UPDATE ci_cache_pool
SET backend_digest=$1,max_total_bytes=$2,max_workspace_bytes=$3,max_entry_bytes=$4,
    max_objects=$5,max_workspace_objects=$6,ttl_seconds=$7,enabled=$8,updated_at=$9
WHERE singleton AND (backend_digest=$1 OR reserved_objects=0)
  AND reserved_bytes<=$2 AND reserved_objects<=$5
  AND NOT EXISTS(SELECT 1 FROM ci_cache_workspace_usage u
    WHERE u.reserved_bytes>$3 OR u.reserved_objects>$6)`

	ciCacheLockWorkspaceQuery = `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`
	ciCacheEnsureUsageQuery   = `
INSERT INTO ci_cache_workspace_usage(workspace_id) VALUES($1)
ON CONFLICT(workspace_id) DO NOTHING`
	ciCacheLockUsageQuery = `
SELECT reserved_bytes,reserved_objects
FROM ci_cache_workspace_usage WHERE workspace_id=$1 FOR UPDATE`

	// $1 backend; $2 bytes; $3 now. Subtraction avoids an overflow in the cap check.
	ciCacheReservePoolQuery = `
UPDATE ci_cache_pool
SET reserved_bytes=reserved_bytes+$2,reserved_objects=reserved_objects+1,updated_at=$3
WHERE singleton AND enabled AND backend_digest=$1
  AND $2 BETWEEN 1 AND max_entry_bytes
  AND reserved_bytes<=max_total_bytes-$2 AND reserved_objects<max_objects`
	// $1 workspace; $2 bytes; $3 workspace byte cap; $4 workspace object cap.
	ciCacheReserveUsageQuery = `
UPDATE ci_cache_workspace_usage
SET reserved_bytes=reserved_bytes+$2,reserved_objects=reserved_objects+1
WHERE workspace_id=$1 AND reserved_bytes<=$3::bigint-$2::bigint AND reserved_objects<$4`

	// $1 namespace; $2 workspace; $3 installation; $4 repository; $5 epoch; $6 now.
	ciCacheEnsureNamespaceQuery = `
INSERT INTO ci_cache_namespaces(digest,workspace_id,installation_id,github_repository_id,epoch,created_at)
VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(digest) DO NOTHING`
	ciCacheNamespaceScopeQuery = `
SELECT workspace_id,installation_id,github_repository_id,epoch
FROM ci_cache_namespaces WHERE digest=$1`

	// Immutable entries support exact key/version matches only. No cross-ref,
	// default-branch, cross-environment or cross-workspace restore fallback.
	// $1 namespace; $2 workspace; $3 installation; $4 repository; $5 epoch;
	// $6 key digest; $7 version digest; $8 backend; $9 now.
	ciCacheLookupEntryQuery = `
SELECT o.id,o.backend_digest,o.size_bytes,o.sha256,o.expires_at
FROM ci_cache_entries e
JOIN ci_cache_namespaces n ON n.digest=e.namespace_digest
JOIN ci_cache_objects o ON o.id=e.object_id AND o.namespace_digest=n.digest
JOIN ci_cache_pool b ON b.singleton AND b.enabled AND b.backend_digest=o.backend_digest
WHERE n.digest=$1 AND n.workspace_id=$2 AND n.installation_id=$3
  AND n.github_repository_id=$4 AND n.epoch=$5
  AND e.key_digest=$6 AND e.version_digest=$7 AND o.backend_digest=$8
  AND o.state='published' AND o.expires_at>$9`
	// Blob download authorization, with the same trust scope as lookup.
	// $1 object; $2 namespace; $3 workspace; $4 installation; $5 repository;
	// $6 epoch; $7 backend; $8 now.
	ciCacheEntryForObjectQuery = `
SELECT o.id,o.backend_digest,o.size_bytes,o.sha256,o.expires_at
FROM ci_cache_entries e
JOIN ci_cache_namespaces n ON n.digest=e.namespace_digest
JOIN ci_cache_objects o ON o.id=e.object_id AND o.namespace_digest=n.digest
JOIN ci_cache_pool b ON b.singleton AND b.enabled AND b.backend_digest=o.backend_digest
WHERE o.id=$1 AND n.digest=$2 AND n.workspace_id=$3 AND n.installation_id=$4
  AND n.github_repository_id=$5 AND n.epoch=$6 AND o.backend_digest=$7
  AND o.state='published' AND o.expires_at>$8`

	// Called under the pool lock; a pending writer also owns the immutable key.
	ciCacheExistingKeyQuery = `
SELECT id,state FROM ci_cache_objects
WHERE namespace_digest=$1 AND key_digest=$2 AND version_digest=$3
  AND state IN ('reserved','writing','published')`

	// $1 ID; $2 workspace; $3 namespace; $4 key; $5 version; $6 backend;
	// $7 runner; $8 job; $9 bytes; $10 SHA; $11 upload deadline; $12 expiry; $13 now.
	ciCacheInsertObjectQuery = `
INSERT INTO ci_cache_objects(id,workspace_id,namespace_digest,key_digest,version_digest,
    backend_digest,runner_id,source_job_id,size_bytes,sha256,
    upload_deadline_at,expires_at,created_at,updated_at,next_attempt_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13,$13)`

	// An upload belongs to its admitted runner/job, not merely its repository.
	// $1 ID; $2 workspace; $3 namespace; $4 backend; $5 runner; $6 job.
	ciCacheObjectForJobQuery = `
SELECT id,size_bytes,sha256,state,writer_token,upload_deadline_at,expires_at,key_digest,version_digest
FROM ci_cache_objects
WHERE id=$1 AND workspace_id=$2 AND namespace_digest=$3 AND backend_digest=$4
  AND runner_id=$5 AND source_job_id=$6`
	ciCacheObjectForJobLockQuery = ciCacheObjectForJobQuery + ` FOR UPDATE`

	// Execute ONLY inside Blob.Put's authorize callback while its exclusive
	// object lock is held. Never preclaim then defer acquiring the file lock.
	// $1 ID; $2 workspace; $3 namespace; $4 backend; $5 runner; $6 job;
	// $7 fresh writer token; $8 now.
	ciCacheClaimWriteQuery = `
UPDATE ci_cache_objects
SET state='writing',writer_token=$7,write_attempted_at=$8,updated_at=$8
WHERE id=$1 AND workspace_id=$2 AND namespace_digest=$3 AND backend_digest=$4
  AND runner_id=$5 AND source_job_id=$6 AND state='reserved'
  AND upload_deadline_at>$8 AND expires_at>$8
RETURNING size_bytes,sha256,key_digest,version_digest`

	// Called after fresh job/epoch/deadline validation under workspace+pool
	// locks. Immutable-key conflicts never replace another object's entry.
	// $1 object; $2 writer token; $3 now.
	ciCachePublishEntryQuery = `
INSERT INTO ci_cache_entries(namespace_digest,key_digest,version_digest,object_id,created_at)
SELECT namespace_digest,key_digest,version_digest,id,$3 FROM ci_cache_objects
WHERE id=$1 AND state='writing' AND writer_token=$2
  AND upload_deadline_at>$3 AND expires_at>$3
ON CONFLICT(namespace_digest,key_digest,version_digest) DO NOTHING
RETURNING object_id`
	ciCachePublishObjectQuery = `
UPDATE ci_cache_objects SET state='published',writer_token='',updated_at=$3
WHERE id=$1 AND state='writing' AND writer_token=$2`

	// Failure/corruption invalidates only an object in the authorized namespace.
	// $1 object; $2 namespace; $3 backend; $4 now.
	ciCacheInvalidateObjectQuery = `
UPDATE ci_cache_objects SET state='cleanup',writer_token='',next_attempt_at=$4,updated_at=$4
WHERE id=$1 AND namespace_digest=$2 AND backend_digest=$3 AND state<>'deleted'`
	ciCacheRemoveEntryQuery = `DELETE FROM ci_cache_entries WHERE object_id=$1`

	// Published objects outlive their source job: otherwise no subsequent job
	// could hit cache. Incomplete writes do not outlive that job's authority.
	// $1 backend; $2 now; $3 batch size (bounded by the two cleanup workers).
	ciCacheCleanupCandidatesQuery = `
SELECT o.id,o.workspace_id,o.size_bytes,o.state
FROM ci_cache_objects o
WHERE o.backend_digest=$1 AND o.state<>'deleted' AND o.next_attempt_at<=$2
  AND (o.cleanup_lease_until IS NULL OR o.cleanup_lease_until<=$2)
  AND (
    o.state='cleanup' OR o.expires_at<=$2
    OR (o.state IN ('reserved','writing') AND o.upload_deadline_at<=$2)
    OR NOT EXISTS(
      SELECT 1 FROM ci_cache_namespaces n
      JOIN ci_github_installations i ON i.workspace_id=n.workspace_id AND i.id=n.installation_id
      JOIN ci_repositories p ON p.workspace_id=n.workspace_id
        AND p.installation_id=n.installation_id AND p.github_repository_id=n.github_repository_id
      LEFT JOIN ci_settings st ON st.workspace_id=n.workspace_id
      WHERE n.digest=o.namespace_digest AND n.workspace_id=o.workspace_id
        AND i.status='active' AND p.enabled AND COALESCE(st.cache_enabled,true)
        AND n.epoch=COALESCE(st.cache_epoch,1)
        AND EXISTS(SELECT 1 FROM workspace_products wp
          WHERE wp.workspace_id=n.workspace_id AND wp.product='ci'))
    OR (o.state='published' AND NOT EXISTS(
      SELECT 1 FROM ci_cache_entries e WHERE e.object_id=o.id))
    OR (o.state IN ('reserved','writing') AND NOT EXISTS(
      SELECT 1 FROM ci_runners r
      JOIN ci_jobs j ON j.workspace_id=r.workspace_id AND j.id=r.assigned_job_id AND j.runner_id=r.id
      JOIN ci_runner_lifecycles l ON l.workspace_id=r.workspace_id AND l.runner_id=r.id
      WHERE r.workspace_id=o.workspace_id AND r.id=o.runner_id AND j.id=o.source_job_id
        AND r.phase='running' AND j.status='running' AND NOT r.delete_requested
        AND r.execution_stopped_at IS NULL AND r.deadline_at>$2
        AND l.stop_requested_at IS NULL AND l.hard_deadline_at>$2)))
ORDER BY o.next_attempt_at,o.created_at,o.id LIMIT $3
FOR UPDATE OF o SKIP LOCKED`

	// Claim and remove entry in one transaction, then COMMIT before file I/O.
	// $1 object; $2 backend; $3 cleanup token; $4 now; $5 lease expiry.
	ciCacheClaimCleanupQuery = `
UPDATE ci_cache_objects
SET state='cleanup',writer_token='',cleanup_token=$3,cleanup_lease_until=$5,
    next_attempt_at=$5,updated_at=$4
WHERE id=$1 AND backend_digest=$2 AND state<>'deleted'
  AND (cleanup_lease_until IS NULL OR cleanup_lease_until<=$4)`

	// Absence acknowledgements lock pool BEFORE object, then usage. Caller has
	// a fresh successful Blob.Delete for this exact backend/ID/cleanup token.
	ciCacheLockAbsentObjectQuery = `
SELECT workspace_id,size_bytes FROM ci_cache_objects
WHERE id=$1 AND backend_digest=$2 AND state='cleanup' AND cleanup_token=$3
  AND quota_released_at IS NULL FOR UPDATE`
	ciCacheAlreadyDeletedQuery = `
SELECT true FROM ci_cache_objects
WHERE id=$1 AND backend_digest=$2 AND state='deleted'
  AND quota_released_at IS NOT NULL AND physical_absent_at IS NOT NULL`
	// $1 backend; $2 bytes; $3 now.
	ciCacheReleasePoolQuery = `
UPDATE ci_cache_pool
SET reserved_bytes=reserved_bytes-$2,reserved_objects=reserved_objects-1,updated_at=$3
WHERE singleton AND backend_digest=$1 AND reserved_bytes>=$2 AND reserved_objects>0`
	// $1 workspace; $2 bytes.
	ciCacheReleaseUsageQuery = `
UPDATE ci_cache_workspace_usage
SET reserved_bytes=reserved_bytes-$2,reserved_objects=reserved_objects-1
WHERE workspace_id=$1 AND reserved_bytes>=$2 AND reserved_objects>0`
	// $1 object; $2 backend; $3 cleanup token; $4 now. Erase private metadata.
	ciCacheAckAbsentQuery = `
UPDATE ci_cache_objects
SET state='deleted',physical_absent_at=$4,quota_released_at=$4,namespace_digest='',
    key_digest='',version_digest='',sha256='',runner_id='',source_job_id='',
    writer_token='',cleanup_token='',cleanup_lease_until=NULL,updated_at=$4
WHERE id=$1 AND backend_digest=$2 AND state='cleanup' AND cleanup_token=$3
  AND quota_released_at IS NULL`

	// Failed/uncertain deletion never refunds bytes; defer it for fair retries.
	// $1 object; $2 backend; $3 cleanup token; $4 now; $5 retry time.
	ciCacheRetryCleanupQuery = `
UPDATE ci_cache_objects SET cleanup_token='',cleanup_lease_until=NULL,
    next_attempt_at=$5,updated_at=$4
WHERE id=$1 AND backend_digest=$2 AND state='cleanup' AND cleanup_token=$3
  AND quota_released_at IS NULL`
	// Run only under the pool lock, so an empty namespace cannot be pruned
	// concurrently with a reservation inserting its first object.
	ciCachePruneNamespacesQuery = `
DELETE FROM ci_cache_namespaces n
WHERE NOT EXISTS(SELECT 1 FROM ci_cache_entries e WHERE e.namespace_digest=n.digest)
  AND NOT EXISTS(SELECT 1 FROM ci_cache_objects o
    WHERE o.namespace_digest=n.digest AND o.state<>'deleted')`
)
