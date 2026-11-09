CREATE TABLE IF NOT EXISTS wren_job_inputs (
    id text PRIMARY KEY,
    job_id text NOT NULL REFERENCES wren_jobs(id) ON DELETE CASCADE,
    operation_key text NOT NULL,
    account_id text REFERENCES accounts(id) ON DELETE SET NULL,
    content text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    applied_at timestamptz,
    UNIQUE(job_id,operation_key)
);
CREATE INDEX IF NOT EXISTS wren_job_inputs_pending ON wren_job_inputs(job_id,created_at,id) WHERE applied_at IS NULL;
