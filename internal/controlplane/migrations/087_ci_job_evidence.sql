CREATE TABLE ci_job_evidence_usage (
    workspace_id text NOT NULL,
    job_id text PRIMARY KEY,
    metric_count integer NOT NULL DEFAULT 0 CHECK (metric_count BETWEEN 0 AND 4096),
    test_count integer NOT NULL DEFAULT 0 CHECK (test_count BETWEEN 0 AND 1000),
    batch_count integer NOT NULL DEFAULT 0 CHECK (batch_count BETWEEN 0 AND 4096),
    accepted_bytes bigint NOT NULL DEFAULT 0 CHECK (accepted_bytes BETWEEN 0 AND 8388608),
    FOREIGN KEY (workspace_id,job_id) REFERENCES ci_jobs(workspace_id,id) ON DELETE CASCADE
);

CREATE TABLE ci_job_evidence_batches (
    workspace_id text NOT NULL,
    job_id text NOT NULL,
    batch_id text NOT NULL CHECK (batch_id ~ '^[0-9a-f]{64}$'),
    raw_sha256 text NOT NULL CHECK (raw_sha256 ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (job_id,batch_id),
    FOREIGN KEY (workspace_id,job_id) REFERENCES ci_jobs(workspace_id,id) ON DELETE CASCADE
);
