CREATE TABLE IF NOT EXISTS workspace_memory (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 current_revision integer NOT NULL CHECK(current_revision>0),
 deleted boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS workspace_memory_revisions (
 memory_id text NOT NULL REFERENCES workspace_memory(id) ON DELETE CASCADE,
 revision integer NOT NULL,
 author_id text REFERENCES accounts(id) ON DELETE SET NULL,
 title text NOT NULL CHECK(char_length(title) BETWEEN 1 AND 120),
 content text NOT NULL CHECK(deleted OR char_length(content) BETWEEN 1 AND 4000),
 deleted boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(memory_id,revision)
);
