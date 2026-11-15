CREATE TABLE IF NOT EXISTS wren_jobs (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    requester_account_id text REFERENCES accounts(id) ON DELETE SET NULL,
    wren_id text NOT NULL REFERENCES wrens(id) ON DELETE CASCADE,
    lead jsonb NOT NULL,
    origin_conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
    parent_run_id text NOT NULL REFERENCES operator_runs(id) ON DELETE CASCADE,
    operation_key text NOT NULL,
    child_conversation_id text NOT NULL UNIQUE REFERENCES operator_conversations(id) ON DELETE CASCADE,
    child_run_id text NOT NULL UNIQUE REFERENCES operator_runs(id) ON DELETE CASCADE,
    return_run_id text UNIQUE REFERENCES operator_runs(id) ON DELETE SET NULL,
    depth integer NOT NULL DEFAULT 1 CHECK(depth=1),
    title text NOT NULL,
    prompt text NOT NULL,
    status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','running','completed','failed','cancelled')),
    result text NOT NULL DEFAULT '',
    failure text NOT NULL DEFAULT '',
    cancelled_at timestamptz,
    published_at timestamptz,
    return_suppressed boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    reconciled_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(parent_run_id,operation_key)
);
CREATE INDEX IF NOT EXISTS wren_jobs_origin_idx ON wren_jobs(origin_conversation_id,created_at DESC);
CREATE INDEX IF NOT EXISTS wren_jobs_pending_return_idx ON wren_jobs(updated_at,id) WHERE return_run_id IS NULL AND return_suppressed=false;

ALTER TABLE wren_jobs ADD COLUMN IF NOT EXISTS reconciled_at timestamptz NOT NULL DEFAULT now();
