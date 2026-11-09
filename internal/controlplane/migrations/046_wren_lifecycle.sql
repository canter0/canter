CREATE TABLE IF NOT EXISTS wren_lifecycle (
 conversation_id text PRIMARY KEY REFERENCES operator_conversations(id) ON DELETE CASCADE,
 archived_at timestamptz,
 archived_by text REFERENCES accounts(id) ON DELETE SET NULL,
 revision integer NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS wren_hidden_conversations (
 conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 hidden_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(conversation_id,account_id)
);
CREATE INDEX IF NOT EXISTS wren_lifecycle_archived_idx ON wren_lifecycle(conversation_id) WHERE archived_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS wren_hidden_account_idx ON wren_hidden_conversations(account_id,conversation_id);
