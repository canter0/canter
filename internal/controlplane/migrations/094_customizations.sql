CREATE TABLE IF NOT EXISTS personal_customizations (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 kind text NOT NULL CHECK(kind IN ('rule','hook')),
 name text NOT NULL,
 body text NOT NULL,
 event text NOT NULL DEFAULT '',
 matcher text NOT NULL DEFAULT '',
 enabled boolean NOT NULL DEFAULT true,
 revision integer NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS personal_customizations_owner ON personal_customizations(workspace_id,account_id,kind);

CREATE TABLE IF NOT EXISTS plugin_connections (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 slug text NOT NULL,
 catalog jsonb NOT NULL,
 state text NOT NULL DEFAULT 'installed',
 failure text NOT NULL DEFAULT '',
 extension_id text REFERENCES wren_extensions(id) ON DELETE SET NULL,
 key_id text NOT NULL DEFAULT '',
 ciphertext bytea,
 revision integer NOT NULL DEFAULT 1,
 enabled boolean NOT NULL DEFAULT true,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(workspace_id,account_id,slug)
);
ALTER TABLE wren_extension_installations ADD COLUMN IF NOT EXISTS plugin_connection_id text REFERENCES plugin_connections(id) ON DELETE CASCADE;
CREATE UNIQUE INDEX IF NOT EXISTS plugin_connection_conversation ON wren_extension_installations(plugin_connection_id,conversation_id) WHERE plugin_connection_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS plugin_oauth_states (
 state_hash bytea PRIMARY KEY,
 browser_hash bytea NOT NULL,
 connection_id text NOT NULL REFERENCES plugin_connections(id) ON DELETE CASCADE,
 revision integer NOT NULL,
 key_id text NOT NULL,
 ciphertext bytea NOT NULL,
 expires_at timestamptz NOT NULL
);
