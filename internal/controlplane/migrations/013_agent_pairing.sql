ALTER TABLE agent_installations ADD COLUMN IF NOT EXISTS expires_at timestamptz;
ALTER TABLE agent_sessions ADD COLUMN IF NOT EXISTS parent_session_id text REFERENCES agent_sessions(id);
ALTER TABLE agent_sessions ADD COLUMN IF NOT EXISTS worker_name text NOT NULL DEFAULT '';
ALTER TABLE agent_sessions ADD COLUMN IF NOT EXISTS worker_draft boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS agent_sessions_parent_idx ON agent_sessions(parent_session_id);
CREATE TABLE IF NOT EXISTS agent_pairings (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    created_by text NOT NULL REFERENCES accounts(id),
    token_hash bytea NOT NULL UNIQUE,
    device_id text UNIQUE REFERENCES device_authorizations(id),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    cancelled_at timestamptz
);
CREATE INDEX IF NOT EXISTS agent_sessions_installation_idx ON agent_sessions(installation_id);
