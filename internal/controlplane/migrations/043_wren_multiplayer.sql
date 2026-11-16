CREATE TABLE IF NOT EXISTS workspace_invites (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 inviter_id text REFERENCES accounts(id) ON DELETE SET NULL,
 email text NOT NULL,
 role text NOT NULL CHECK(role IN ('operator','viewer')),
 token_hash bytea NOT NULL UNIQUE,
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','accepted','declined','revoked')),
 expires_at timestamptz NOT NULL,
 accepted_by text REFERENCES accounts(id) ON DELETE SET NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS workspace_invites_workspace_idx ON workspace_invites(workspace_id,created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS workspace_invites_pending_email_idx ON workspace_invites(workspace_id,email) WHERE state='pending';
CREATE TABLE IF NOT EXISTS wren_message_replies (
 message_id text PRIMARY KEY REFERENCES operator_messages(id) ON DELETE CASCADE,
 conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
 target_message_id text NOT NULL REFERENCES operator_messages(id) ON DELETE CASCADE,
 target_wren_id text REFERENCES wrens(id) ON DELETE SET NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS wren_read_cursors (
 conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 sequence bigint NOT NULL DEFAULT 0 CHECK(sequence>=0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(conversation_id,account_id)
);
ALTER TABLE workspace_invites ADD COLUMN IF NOT EXISTS mail_outbox_id text REFERENCES auth_email_outbox(id) ON DELETE SET NULL;
