CREATE TABLE IF NOT EXISTS wren_app_accounts (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 provider text NOT NULL,
 endpoint text NOT NULL,
 label text NOT NULL,
 revision integer NOT NULL DEFAULT 1,
 key_id text NOT NULL,
 ciphertext bytea,
 expires_at timestamptz,
 reconnect boolean NOT NULL DEFAULT false,
 revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS wren_app_accounts_owner_idx ON wren_app_accounts(workspace_id,account_id) WHERE revoked_at IS NULL;
CREATE TABLE IF NOT EXISTS wren_app_oauth_states (
 state_hash bytea PRIMARY KEY,
 browser_hash bytea NOT NULL,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 provider text NOT NULL,
 key_id text NOT NULL,
 ciphertext bytea NOT NULL,
 expires_at timestamptz NOT NULL
);
ALTER TABLE wren_extension_credentials ADD COLUMN IF NOT EXISTS app_account_id text REFERENCES wren_app_accounts(id) ON DELETE SET NULL;
