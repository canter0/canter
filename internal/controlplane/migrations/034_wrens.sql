CREATE TABLE IF NOT EXISTS wrens (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    account_id text NOT NULL REFERENCES accounts(id),
    conversation_id text NOT NULL UNIQUE REFERENCES operator_conversations(id) ON DELETE CASCADE,
    name text NOT NULL,
    purpose text NOT NULL DEFAULT '',
    shape text NOT NULL DEFAULT 'pebble',
    palette integer NOT NULL DEFAULT 0 CHECK (palette BETWEEN 0 AND 7),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS wrens_owner_idx ON wrens(workspace_id, account_id, created_at);
