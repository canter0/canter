CREATE TABLE IF NOT EXISTS wren_publications (
 wren_id text PRIMARY KEY REFERENCES wrens(id) ON DELETE CASCADE,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 publisher_id text REFERENCES accounts(id) ON DELETE SET NULL,
 profile jsonb NOT NULL,
 published boolean NOT NULL DEFAULT true,
 archived boolean NOT NULL DEFAULT false,
 revision integer NOT NULL CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS wren_publication_managers (
 wren_id text NOT NULL REFERENCES wren_publications(wren_id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 PRIMARY KEY(wren_id,account_id)
);
-- Keep a person's private history/profile snapshot after a publisher removes
-- the original identity. A missing publication denies new inference.
CREATE TABLE IF NOT EXISTS wren_shared_chats (
 wren_id text NOT NULL,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 conversation_id text NOT NULL UNIQUE REFERENCES operator_conversations(id) ON DELETE CASCADE,
 profile_snapshot jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(wren_id,account_id)
);
CREATE INDEX IF NOT EXISTS wren_shared_chats_account_idx ON wren_shared_chats(workspace_id,account_id);
CREATE TABLE IF NOT EXISTS wren_templates (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 author_id text REFERENCES accounts(id) ON DELETE SET NULL,
 profile jsonb NOT NULL,
 published boolean NOT NULL DEFAULT true,
 revision integer NOT NULL DEFAULT 1 CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS wren_configuration_copies (
 wren_id text PRIMARY KEY REFERENCES wrens(id) ON DELETE CASCADE,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 source_kind text NOT NULL CHECK(source_kind IN ('wren','template')),
 source_id text NOT NULL,
 source_revision integer NOT NULL,
 profile jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
