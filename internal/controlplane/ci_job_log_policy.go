package controlplane

import (
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ciJobLogPollInterval   = 2 * time.Second
	ciJobLogWorkers        = 2
	ciJobLogLeaseWindow    = time.Minute
	ciJobLogRetryInterval  = 5 * time.Minute
	ciJobLogBackfillWindow = 7 * 24 * time.Hour
	ciJobLogDownloadBound  = 20 * time.Second
	ciJobLogDBBound        = 5 * time.Second
	ciJobLogMaxBytes       = 1 << 20
	ciJobLogChunkBytes     = 16384
)

// Backfill is bounded to recently updated completed managed jobs. This does
// not delete previously collected logs or authorize any runner execution.
const ciJobLogCandidatesSQL = `
SELECT j.workspace_id,j.id
FROM ci_jobs j
JOIN ci_runners r ON r.workspace_id=j.workspace_id AND r.id=j.runner_id
    AND r.assigned_job_id=j.id AND r.installation_id=j.installation_id
JOIN ci_github_installations i ON i.workspace_id=j.workspace_id AND i.id=j.installation_id
JOIN ci_repositories p ON p.workspace_id=j.workspace_id AND p.installation_id=j.installation_id
    AND p.github_repository_id=j.github_repository_id
LEFT JOIN ci_job_log_collections c ON c.workspace_id=j.workspace_id AND c.job_id=j.id
WHERE j.status IN ('completed','cancelled') AND r.execution_started_at IS NOT NULL
    AND i.status='active' AND p.enabled AND j.updated_at>=$1
    AND EXISTS(SELECT 1 FROM workspace_products wp
        WHERE wp.workspace_id=j.workspace_id AND wp.product='ci')
    AND (c.job_id IS NULL OR (c.state<>'complete' AND c.next_attempt_at<=$2 AND c.lease_until<=$2))
ORDER BY j.updated_at,j.id
LIMIT 2`

// Claim and publication both lock the workspace first and then re-read this
// authoritative scope inside their transaction. No network I/O while locked.
const ciJobLogWorkspaceLockSQL = `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`

const ciJobLogScopeSQL = `
SELECT j.workspace_id,j.id,r.id,i.github_installation_id,
       j.github_repository_id,j.github_job_id,j.github_run_id
FROM ci_jobs j
JOIN ci_runners r ON r.workspace_id=j.workspace_id AND r.id=j.runner_id
    AND r.assigned_job_id=j.id AND r.installation_id=j.installation_id
JOIN ci_github_installations i ON i.workspace_id=j.workspace_id AND i.id=j.installation_id
JOIN ci_repositories p ON p.workspace_id=j.workspace_id AND p.installation_id=j.installation_id
    AND p.github_repository_id=j.github_repository_id
WHERE j.workspace_id=$1 AND j.id=$2 AND j.status IN ('completed','cancelled')
    AND r.execution_started_at IS NOT NULL AND i.status='active' AND p.enabled
    AND EXISTS(SELECT 1 FROM workspace_products wp
        WHERE wp.workspace_id=j.workspace_id AND wp.product='ci')
FOR UPDATE OF j,r,i,p`

// Nonces fence stale downloaders; expired leases alone never permit a stale
// writer to publish after another process has taken ownership.
const ciJobLogClaimSQL = `
INSERT INTO ci_job_log_collections
    (workspace_id,job_id,state,owner_nonce,lease_until,next_attempt_at)
VALUES($1,$2,'collecting',$3,$4,$5)
ON CONFLICT(job_id) DO UPDATE
SET state='collecting',owner_nonce=EXCLUDED.owner_nonce,
    lease_until=EXCLUDED.lease_until,next_attempt_at=EXCLUDED.next_attempt_at
WHERE ci_job_log_collections.workspace_id=EXCLUDED.workspace_id
    AND ci_job_log_collections.state<>'complete'
    AND ci_job_log_collections.lease_until<=$5
    AND ci_job_log_collections.next_attempt_at<=$5
RETURNING job_id`

const ciJobLogLeaseSQL = `
SELECT state,owner_nonce,lease_until
FROM ci_job_log_collections
WHERE workspace_id=$1 AND job_id=$2
FOR UPDATE`

const ciJobLogNextSequenceSQL = `
SELECT COALESCE(MAX(sequence),-1)+1
FROM ci_job_logs WHERE workspace_id=$1 AND job_id=$2`

const ciJobLogInsertSQL = `
INSERT INTO ci_job_logs(workspace_id,job_id,sequence,stream,content,created_at)
VALUES($1,$2,$3,'stdout',$4,$5)`

const ciJobLogCompleteSQL = `
UPDATE ci_job_log_collections
SET state='complete',owner_nonce='',lease_until=$4,next_attempt_at=$4,
    truncated=$5,collected_at=$4
WHERE workspace_id=$1 AND job_id=$2 AND owner_nonce=$3
    AND state='collecting' AND lease_until>$4`

const ciJobLogUnavailableSQL = `
UPDATE ci_job_log_collections
SET state='unavailable',owner_nonce='',lease_until=$4,next_attempt_at=$5
WHERE workspace_id=$1 AND job_id=$2 AND owner_nonce=$3 AND state='collecting'`

const ciJobLogCollectionSQL = `
SELECT state,truncated,collected_at
FROM ci_job_log_collections WHERE workspace_id=$1 AND job_id=$2`

// GitHub's plaintext is bounded before storage. PostgreSQL text cannot hold
// NUL; invalid UTF-8 and NUL are explicitly replaced, not silently dropped.
func normalizeCIJobLog(raw []byte) (string, bool) {
	truncated := len(raw) > ciJobLogMaxBytes
	if truncated {
		raw = raw[:ciJobLogMaxBytes]
	}
	text := strings.ToValidUTF8(string(raw), "\uFFFD")
	text = strings.ReplaceAll(text, "\x00", "\uFFFD")
	if len(text) > ciJobLogMaxBytes {
		text = text[:ciJobLogMaxBytes]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		truncated = true
	}
	return text, truncated
}

func splitCIJobLog(text string) []string {
	var chunks []string
	for text != "" {
		n := len(text)
		if n > ciJobLogChunkBytes {
			n = ciJobLogChunkBytes
			for !utf8.ValidString(text[:n]) {
				n--
			}
		}
		chunks = append(chunks, text[:n])
		text = text[n:]
	}
	return chunks
}
