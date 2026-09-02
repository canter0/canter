CREATE TABLE IF NOT EXISTS change_approval_capabilities (
    id text PRIMARY KEY,
    token_hash bytea NOT NULL UNIQUE,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    system_name text NOT NULL,
    change_id text NOT NULL,
    digest text NOT NULL CHECK (length(digest) = 64),
    requested_by_installation text NOT NULL REFERENCES agent_installations(id),
    requested_session_id text NOT NULL DEFAULT '',
    action text NOT NULL CHECK (action = 'authorize-and-apply'),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    consumed_by text REFERENCES accounts(id),
    execution_id text REFERENCES executions(id),
    revoked_at timestamptz,
    FOREIGN KEY (workspace_id, system_name, change_id)
        REFERENCES change_records(workspace_id, system_name, change_id)
);

CREATE INDEX IF NOT EXISTS change_approval_capabilities_change_idx
    ON change_approval_capabilities(workspace_id, system_name, change_id, created_at DESC);

CREATE INDEX IF NOT EXISTS change_approval_capabilities_active_idx
    ON change_approval_capabilities(expires_at)
    WHERE consumed_at IS NULL AND revoked_at IS NULL;
