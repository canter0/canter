CREATE TABLE IF NOT EXISTS node_installations (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    system_name text NOT NULL,
    m1_prefix text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz,
    revoked_at timestamptz,
    UNIQUE (workspace_id, system_name, id)
);

CREATE TABLE IF NOT EXISTS node_enrollments (
    id text PRIMARY KEY,
    node_id text NOT NULL REFERENCES node_installations(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz
);

CREATE TABLE IF NOT EXISTS node_credentials (
    id text PRIMARY KEY,
    node_id text NOT NULL REFERENCES node_installations(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);

CREATE INDEX IF NOT EXISTS node_installations_scope_idx ON node_installations(workspace_id, system_name) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS node_installations_active_scope_idx ON node_installations(workspace_id, system_name) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS node_enrollments_active_node_idx ON node_enrollments(node_id) WHERE consumed_at IS NULL;
CREATE INDEX IF NOT EXISTS node_credentials_node_idx ON node_credentials(node_id) WHERE revoked_at IS NULL;
