-- Keep paid-resource intents after their room/account is removed so the
-- dispatcher can verify deletion before discarding their ownership record.
CREATE TABLE IF NOT EXISTS wren_computers (
 id text PRIMARY KEY,
 workspace_id text NOT NULL,
 conversation_id text NOT NULL,
 owner_id text REFERENCES accounts(id) ON DELETE SET NULL,
 scope_kind text NOT NULL CHECK(scope_kind IN ('private','room')),
 name text NOT NULL,
 phase text NOT NULL CHECK(phase IN ('queued','provisioning','ready','failed','deleting','deleted')),
 plan jsonb NOT NULL,
 ssh_key_version text NOT NULL,
 ssh_key_ciphertext bytea NOT NULL,
 address text NOT NULL DEFAULT '',
 host_fingerprint text NOT NULL DEFAULT '',
 monthly_cents bigint NOT NULL,
 failure text NOT NULL DEFAULT '',
 delete_requested boolean NOT NULL DEFAULT false,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 last_used_at timestamptz,
 expires_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS wren_computers_scope_idx ON wren_computers(conversation_id) WHERE phase<>'deleted';
CREATE INDEX IF NOT EXISTS wren_computers_work_idx ON wren_computers(next_attempt_at) WHERE phase NOT IN ('failed','deleted');
CREATE INDEX IF NOT EXISTS wren_computers_workspace_idx ON wren_computers(workspace_id,created_at DESC);

ALTER TABLE wren_computers ADD COLUMN IF NOT EXISTS control_account_id text REFERENCES accounts(id) ON DELETE SET NULL;
ALTER TABLE wren_computers ADD COLUMN IF NOT EXISTS control_expires_at timestamptz;
