CREATE TABLE IF NOT EXISTS workspace_secrets (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    name text NOT NULL,
    purpose text NOT NULL CHECK (purpose IN ('stored', 'openrouter')),
    note text NOT NULL DEFAULT '',
    ciphertext bytea,
    key_id text NOT NULL,
    version integer NOT NULL DEFAULT 1,
    updated_by text NOT NULL REFERENCES accounts(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at timestamptz,
    CHECK ((revoked_at IS NULL) = (ciphertext IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS workspace_secrets_name_idx ON workspace_secrets(workspace_id,name) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS workspace_secrets_openrouter_idx ON workspace_secrets(workspace_id) WHERE purpose='openrouter' AND revoked_at IS NULL;
