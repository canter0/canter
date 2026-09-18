ALTER TABLE oauth_login_states ADD COLUMN IF NOT EXISTS workspace_id text REFERENCES workspaces(id) ON DELETE CASCADE;

CREATE TABLE IF NOT EXISTS github_repository_connections (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    github_id text NOT NULL,
    login text NOT NULL,
    encrypted_token bytea NOT NULL,
    scopes text NOT NULL,
    expires_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(account_id, workspace_id)
);
