CREATE TABLE IF NOT EXISTS ci_connect_states (
    state_hash text PRIMARY KEY CHECK (length(state_hash)=64),
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ci_connect_states_expiry ON ci_connect_states(expires_at);
