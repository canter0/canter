CREATE TABLE IF NOT EXISTS wren_extensions (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 owner_id text REFERENCES accounts(id) ON DELETE SET NULL,
 current_version integer NOT NULL CHECK(current_version>0),
 disabled boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS wren_extension_versions (
 extension_id text NOT NULL REFERENCES wren_extensions(id) ON DELETE CASCADE,
 version integer NOT NULL CHECK(version>0),
 author_id text REFERENCES accounts(id) ON DELETE SET NULL,
 manifest jsonb NOT NULL,
 sha256 text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(extension_id,version)
);
CREATE TABLE IF NOT EXISTS wren_extension_installations (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
 scope_kind text NOT NULL CHECK(scope_kind IN ('private','room')),
 owner_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 wren_id text REFERENCES wrens(id) ON DELETE CASCADE,
 extension_id text NOT NULL REFERENCES wren_extensions(id) ON DELETE CASCADE,
 version integer NOT NULL,
 revision integer NOT NULL DEFAULT 1 CHECK(revision>0),
 enabled boolean NOT NULL DEFAULT true,
 revoked_at timestamptz,
 allow_write boolean NOT NULL DEFAULT false,
 credential_mode text NOT NULL CHECK(credential_mode IN ('personal','service')),
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(extension_id,version) REFERENCES wren_extension_versions(extension_id,version),
 CHECK(scope_kind<>'private' OR wren_id IS NOT NULL),
 CHECK(scope_kind<>'room' OR wren_id IS NULL)
);
CREATE TABLE IF NOT EXISTS wren_extension_credentials (
 installation_id text NOT NULL REFERENCES wren_extension_installations(id) ON DELETE CASCADE,
 credential_key text NOT NULL,
 account_id text REFERENCES accounts(id) ON DELETE CASCADE,
 revision integer NOT NULL CHECK(revision>0),
 key_id text NOT NULL DEFAULT '',ciphertext bytea,
 updated_by text REFERENCES accounts(id) ON DELETE SET NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(installation_id,credential_key)
);
CREATE TABLE IF NOT EXISTS wren_extension_calls (
 id text PRIMARY KEY,
 installation_id text NOT NULL REFERENCES wren_extension_installations(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 run_id text REFERENCES operator_runs(id) ON DELETE SET NULL,
 operation_key text NOT NULL,
 tool_name text NOT NULL,
 version integer NOT NULL,
 arguments_sha256 text NOT NULL,
 status text NOT NULL CHECK(status IN ('running','completed','failed','unknown')),
 is_private boolean NOT NULL DEFAULT false,
 http_status integer NOT NULL DEFAULT 0,
 result jsonb,
 failure text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),completed_at timestamptz,
 UNIQUE(installation_id,account_id,operation_key)
);
CREATE TABLE IF NOT EXISTS wren_extension_event_bindings (
 id text PRIMARY KEY,
 installation_id text NOT NULL REFERENCES wren_extension_installations(id) ON DELETE CASCADE,
 owner_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 responsibility_id text NOT NULL REFERENCES wren_responsibilities(id) ON DELETE CASCADE,
 event_name text NOT NULL,
 token_hash bytea NOT NULL,
 revision integer NOT NULL DEFAULT 1,
 enabled boolean NOT NULL DEFAULT true,
 last_event_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(installation_id,responsibility_id,event_name)
);
CREATE INDEX IF NOT EXISTS wren_extension_installations_scope_idx ON wren_extension_installations(workspace_id,conversation_id) WHERE enabled AND revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS wren_extension_calls_recent_idx ON wren_extension_calls(account_id,created_at DESC);
CREATE INDEX IF NOT EXISTS wren_extension_event_install_idx ON wren_extension_event_bindings(installation_id);

CREATE UNIQUE INDEX IF NOT EXISTS wren_extension_active_install_idx ON wren_extension_installations(conversation_id,extension_id) WHERE revoked_at IS NULL;
