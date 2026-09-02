CREATE TABLE IF NOT EXISTS schema_migrations (
    version text PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS beta_invites (
    key_hash bytea PRIMARY KEY,
    label text NOT NULL DEFAULT '',
    consumed_by text,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS accounts (
    id text PRIMARY KEY,
    email text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    disabled_at timestamptz
);

CREATE TABLE IF NOT EXISTS workspaces (
    id text PRIMARY KEY,
    name text NOT NULL,
    revision bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS memberships (
    account_id text NOT NULL REFERENCES accounts(id),
    workspace_id text NOT NULL REFERENCES workspaces(id),
    role text NOT NULL CHECK (role IN ('owner', 'operator', 'viewer')),
    PRIMARY KEY (account_id, workspace_id)
);

CREATE TABLE IF NOT EXISTS human_sessions (
    id text PRIMARY KEY,
    account_id text NOT NULL REFERENCES accounts(id),
    token_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);

CREATE TABLE IF NOT EXISTS agent_installations (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    name text NOT NULL,
    harness text NOT NULL,
    inspect_allowed boolean NOT NULL DEFAULT true,
    draft_allowed boolean NOT NULL DEFAULT false,
    apply_mode text NOT NULL DEFAULT 'human-approval-required',
    created_by text NOT NULL REFERENCES accounts(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz,
    revoked_at timestamptz
);

CREATE TABLE IF NOT EXISTS agent_credentials (
    id text PRIMARY KEY,
    installation_id text NOT NULL REFERENCES agent_installations(id),
    refresh_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    rotated_at timestamptz,
    revoked_at timestamptz
);

CREATE TABLE IF NOT EXISTS device_authorizations (
    id text PRIMARY KEY,
    device_hash bytea NOT NULL UNIQUE,
    user_code text NOT NULL UNIQUE,
    requested_name text NOT NULL,
    harness text NOT NULL,
    requested_inspect boolean NOT NULL DEFAULT true,
    requested_draft boolean NOT NULL DEFAULT false,
    requested_apply_mode text NOT NULL DEFAULT 'human-approval-required',
    workspace_id text REFERENCES workspaces(id),
    authorized_by text REFERENCES accounts(id),
    installation_id text REFERENCES agent_installations(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    authorized_at timestamptz,
    exchanged_at timestamptz,
    denied_at timestamptz
);

CREATE TABLE IF NOT EXISTS agent_sessions (
    id text PRIMARY KEY,
    installation_id text NOT NULL REFERENCES agent_installations(id),
    access_hash bytea NOT NULL UNIQUE,
    client_instance text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    ended_at timestamptz
);

CREATE TABLE IF NOT EXISTS systems (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    name text NOT NULL,
    revision bigint NOT NULL DEFAULT 1,
    contract jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, name)
);

CREATE TABLE IF NOT EXISTS executions (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    system_name text NOT NULL,
    change_id text NOT NULL,
    phase text NOT NULL CHECK (phase IN ('queued','running','succeeded','failed')),
    requested_by_kind text NOT NULL,
    requested_by_id text NOT NULL,
    requested_session_id text NOT NULL DEFAULT '',
    attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT now(),
    claimed_by text,
    lease_expires_at timestamptz,
    failure text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    completed_at timestamptz,
    UNIQUE (workspace_id, system_name, change_id)
);

CREATE TABLE IF NOT EXISTS change_records (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    system_name text NOT NULL,
    change_id text NOT NULL,
    phase text NOT NULL,
    summary text NOT NULL,
    digest text NOT NULL,
    document jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, system_name, change_id)
);

CREATE INDEX IF NOT EXISTS change_records_workspace_time_idx
    ON change_records (workspace_id, created_at DESC);

CREATE INDEX IF NOT EXISTS executions_claimable_idx
    ON executions (phase, available_at, lease_expires_at);

CREATE TABLE IF NOT EXISTS audit_events (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    actor_kind text NOT NULL,
    actor_id text NOT NULL,
    session_id text NOT NULL DEFAULT '',
    action text NOT NULL,
    subject text NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS audit_events_workspace_time_idx
    ON audit_events (workspace_id, occurred_at DESC);
