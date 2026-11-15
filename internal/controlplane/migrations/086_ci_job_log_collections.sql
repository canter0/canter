CREATE TABLE ci_job_log_collections (
    workspace_id text NOT NULL,
    job_id text PRIMARY KEY,
    state text NOT NULL CHECK (state IN ('collecting','complete','unavailable')),
    owner_nonce text NOT NULL,
    lease_until timestamptz NOT NULL,
    next_attempt_at timestamptz NOT NULL,
    truncated boolean NOT NULL DEFAULT false,
    collected_at timestamptz,
    FOREIGN KEY (workspace_id,job_id) REFERENCES ci_jobs(workspace_id,id) ON DELETE CASCADE,
    CHECK (owner_nonce='' OR owner_nonce ~ '^[0-9a-f]{64}$'),
    CHECK ((state='collecting')=(owner_nonce<>'')),
    CHECK ((state='complete')=(collected_at IS NOT NULL)),
    CHECK (NOT truncated OR state='complete')
);

CREATE INDEX ci_job_log_collections_due
    ON ci_job_log_collections(next_attempt_at,job_id) WHERE state<>'complete';
