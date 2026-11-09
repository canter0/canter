CREATE TABLE IF NOT EXISTS wren_memory (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id),
 conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
 scope_kind text NOT NULL CHECK(scope_kind IN ('private','room')),
 owner_id text REFERENCES accounts(id) ON DELETE CASCADE,
 wren_id text REFERENCES wrens(id) ON DELETE CASCADE,
 current_revision integer NOT NULL CHECK(current_revision>0),
 deleted boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK((scope_kind='private' AND owner_id IS NOT NULL AND wren_id IS NOT NULL) OR (scope_kind='room' AND owner_id IS NULL AND wren_id IS NULL))
);
CREATE TABLE IF NOT EXISTS wren_memory_revisions (
 memory_id text NOT NULL REFERENCES wren_memory(id) ON DELETE CASCADE,
 revision integer NOT NULL CHECK(revision>0),
 author_id text REFERENCES accounts(id) ON DELETE SET NULL,
 title text NOT NULL,
 content text NOT NULL,
 sources jsonb NOT NULL DEFAULT '{}',
 promoted_from_id text NOT NULL DEFAULT '',
 promoted_from_revision integer NOT NULL DEFAULT 0,
 deleted boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(memory_id,revision)
);
CREATE INDEX IF NOT EXISTS wren_memory_scope_idx ON wren_memory(workspace_id,conversation_id,updated_at DESC) WHERE NOT deleted;
CREATE TABLE IF NOT EXISTS wren_skills (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id),
 owner_id text REFERENCES accounts(id) ON DELETE CASCADE,
 current_version integer NOT NULL CHECK(current_version>0),
 disabled boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS wren_skill_versions (
 skill_id text NOT NULL REFERENCES wren_skills(id) ON DELETE CASCADE,
 version integer NOT NULL CHECK(version>0),
 author_id text REFERENCES accounts(id) ON DELETE SET NULL,
 name text NOT NULL,
 description text NOT NULL DEFAULT '',
 instructions text NOT NULL,
 required_capabilities jsonb NOT NULL DEFAULT '[]',
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(skill_id,version)
);
CREATE TABLE IF NOT EXISTS wren_skill_installations (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id),
 conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
 scope_kind text NOT NULL CHECK(scope_kind IN ('private','room')),
 owner_id text REFERENCES accounts(id) ON DELETE CASCADE,
 wren_id text REFERENCES wrens(id) ON DELETE CASCADE,
 skill_id text NOT NULL REFERENCES wren_skills(id) ON DELETE CASCADE,
 version integer NOT NULL,
 revision integer NOT NULL DEFAULT 1 CHECK(revision>0),
 enabled boolean NOT NULL DEFAULT true,
 author_id text REFERENCES accounts(id) ON DELETE SET NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(skill_id,version) REFERENCES wren_skill_versions(skill_id,version),
 UNIQUE(conversation_id,skill_id),
 CHECK((scope_kind='private' AND owner_id IS NOT NULL AND wren_id IS NOT NULL) OR (scope_kind='room' AND owner_id IS NULL AND wren_id IS NULL))
);
CREATE INDEX IF NOT EXISTS wren_skills_workspace_idx ON wren_skills(workspace_id,updated_at DESC);
CREATE INDEX IF NOT EXISTS wren_skill_installations_scope_idx ON wren_skill_installations(workspace_id,conversation_id) WHERE enabled;
