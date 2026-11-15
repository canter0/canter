CREATE TABLE IF NOT EXISTS wren_projects (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id),
 name text NOT NULL,
 system_name text,
 revision integer NOT NULL DEFAULT 1 CHECK(revision>0),
 archived boolean NOT NULL DEFAULT false,
 created_by text REFERENCES accounts(id) ON DELETE SET NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(workspace_id,id),
 FOREIGN KEY(workspace_id,system_name) REFERENCES systems(workspace_id,name)
);
CREATE TABLE IF NOT EXISTS wren_project_notes (
 id text PRIMARY KEY,
 workspace_id text NOT NULL,
 project_id text NOT NULL,
 current_revision integer NOT NULL CHECK(current_revision>0),
 deleted boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(workspace_id,project_id) REFERENCES wren_projects(workspace_id,id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS wren_project_note_revisions (
 note_id text NOT NULL REFERENCES wren_project_notes(id) ON DELETE CASCADE,
 revision integer NOT NULL,
 author_id text REFERENCES accounts(id) ON DELETE SET NULL,
 title text NOT NULL,
 content text NOT NULL,
 sources jsonb NOT NULL DEFAULT '{}',
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(note_id,revision)
);
CREATE TABLE IF NOT EXISTS wren_project_grants (
 id text PRIMARY KEY,
 workspace_id text NOT NULL,
 project_id text NOT NULL,
 conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
 target_kind text NOT NULL CHECK(target_kind IN ('private','room')),
 wren_id text,
 owner_id text,
 revision integer NOT NULL DEFAULT 1 CHECK(revision>0),
 revoked boolean NOT NULL DEFAULT false,
 created_by text REFERENCES accounts(id) ON DELETE SET NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(project_id,conversation_id),
 FOREIGN KEY(workspace_id,project_id) REFERENCES wren_projects(workspace_id,id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS wren_project_job_grants (
 job_id text NOT NULL REFERENCES wren_jobs(id) ON DELETE CASCADE,
 project_id text NOT NULL REFERENCES wren_projects(id) ON DELETE CASCADE,
 source_grant_id text NOT NULL REFERENCES wren_project_grants(id) ON DELETE CASCADE,
 source_revision integer NOT NULL,
 PRIMARY KEY(job_id,project_id)
);
CREATE TABLE IF NOT EXISTS wren_project_run_reads (
 run_id text NOT NULL REFERENCES operator_runs(id) ON DELETE CASCADE,
 project_id text NOT NULL REFERENCES wren_projects(id) ON DELETE CASCADE,
 grant_id text NOT NULL REFERENCES wren_project_grants(id) ON DELETE CASCADE,
 grant_revision integer NOT NULL,
 read_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(run_id,project_id)
);
CREATE INDEX IF NOT EXISTS wren_project_grants_conversation_idx ON wren_project_grants(conversation_id) WHERE NOT revoked;
