CREATE TABLE IF NOT EXISTS deployment_artifacts (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    sha256 text NOT NULL CHECK (length(sha256) = 64),
    storage_key text NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes > 0),
    content_type text NOT NULL,
    filename text NOT NULL DEFAULT '',
    entries jsonb NOT NULL DEFAULT '[]'::jsonb,
    uploaded_by_kind text NOT NULL,
    uploaded_by_id text NOT NULL,
    uploaded_session_id text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, sha256)
);

ALTER TABLE deployment_artifacts
    ADD COLUMN IF NOT EXISTS entries jsonb NOT NULL DEFAULT '[]'::jsonb;

CREATE TABLE IF NOT EXISTS initial_deployments (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    system_name text NOT NULL,
    phase text NOT NULL CHECK (phase IN ('drafted','authorized','queued','running','succeeded','failed')),
    summary text NOT NULL,
    digest text NOT NULL,
    document jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (workspace_id, system_name)
);

CREATE INDEX IF NOT EXISTS initial_deployments_workspace_time_idx
    ON initial_deployments (workspace_id, created_at DESC);

CREATE TABLE IF NOT EXISTS initial_deployment_executions (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    deployment_id text NOT NULL REFERENCES initial_deployments(id),
    system_name text NOT NULL,
    phase text NOT NULL CHECK (phase IN ('queued','running','succeeded','failed')),
    requested_by_kind text NOT NULL,
    requested_by_id text NOT NULL,
    requested_session_id text NOT NULL DEFAULT '',
    attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT now(),
    claimed_by text,
    claim_token text NOT NULL DEFAULT '',
    lease_expires_at timestamptz,
    failure text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    completed_at timestamptz
);

ALTER TABLE initial_deployment_executions
    ADD COLUMN IF NOT EXISTS claim_token text NOT NULL DEFAULT '';

-- Historical executions are retained for an immutable audit trail. A failed
-- proposal may be retried, but only one queued/running execution may exist.
ALTER TABLE initial_deployment_executions
    DROP CONSTRAINT IF EXISTS initial_deployment_executions_deployment_id_key;

CREATE UNIQUE INDEX IF NOT EXISTS initial_deployment_executions_active_deployment_idx
    ON initial_deployment_executions (deployment_id)
    WHERE phase IN ('queued','running');

CREATE INDEX IF NOT EXISTS initial_deployment_executions_claimable_idx
    ON initial_deployment_executions (phase, available_at, lease_expires_at);
