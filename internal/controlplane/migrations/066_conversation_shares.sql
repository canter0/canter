CREATE TABLE IF NOT EXISTS conversation_shares (
 conversation_id text PRIMARY KEY REFERENCES operator_conversations(id) ON DELETE CASCADE,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 shared_by text REFERENCES accounts(id) ON DELETE SET NULL,
 shared_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS conversation_shares_workspace_idx ON conversation_shares(workspace_id,shared_at DESC);
