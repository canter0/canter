CREATE TABLE IF NOT EXISTS standing_policies (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    system_name text NOT NULL,
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    digest text NOT NULL CHECK (length(digest) = 64),
    envelope jsonb NOT NULL,
    workspace_revision bigint NOT NULL,
    system_revision bigint NOT NULL,
    created_by_account text NOT NULL REFERENCES accounts(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    revoked_by_account text REFERENCES accounts(id),
    FOREIGN KEY (workspace_id, system_name)
        REFERENCES systems(workspace_id, name),
    UNIQUE (workspace_id, system_name, name)
);

CREATE INDEX IF NOT EXISTS standing_policies_active_idx
    ON standing_policies(workspace_id, system_name, expires_at)
    WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS change_policy_decisions (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    system_name text NOT NULL,
    change_id text NOT NULL,
    change_digest text NOT NULL CHECK (length(change_digest) = 64),
    outcome text NOT NULL CHECK (outcome IN ('automatic', 'human-approval-required')),
    phase text NOT NULL CHECK (phase IN ('decided', 'authorized', 'queued', 'failed')),
    policy_id text REFERENCES standing_policies(id),
    policy_digest text NOT NULL DEFAULT '',
    evaluated_by_installation text NOT NULL REFERENCES agent_installations(id),
    reason text NOT NULL,
    execution_id text REFERENCES executions(id),
    failure text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, system_name, change_id)
        REFERENCES change_records(workspace_id, system_name, change_id),
    UNIQUE (workspace_id, system_name, change_id, change_digest)
);

CREATE INDEX IF NOT EXISTS change_policy_decisions_change_idx
    ON change_policy_decisions(workspace_id, system_name, change_id, created_at DESC);
