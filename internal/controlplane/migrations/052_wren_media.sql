CREATE TABLE IF NOT EXISTS wren_media_operations (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 run_id text REFERENCES operator_runs(id) ON DELETE SET NULL,
 authority_run_id text REFERENCES operator_runs(id) ON DELETE SET NULL,
 operation_key text NOT NULL,
 request_sha256 text NOT NULL,
 model text NOT NULL,
 mode text NOT NULL CHECK(mode IN ('generate','edit')),
 name text NOT NULL,prompt text NOT NULL,
 source_artifact_id text,
 source_revision integer NOT NULL DEFAULT 0,
 state text NOT NULL CHECK(state IN ('running','unknown','failed','ready','conflict','blocked','completed')),
 failure text NOT NULL DEFAULT '',
 provider_request_id text NOT NULL DEFAULT '',
 usage jsonb NOT NULL DEFAULT '{}',
 width integer NOT NULL DEFAULT 0,height integer NOT NULL DEFAULT 0,
 sha256 text NOT NULL DEFAULT '',
 output_bytes bytea CHECK(output_bytes IS NULL OR octet_length(output_bytes)<=8388608),
 artifact_id text REFERENCES wren_artifacts(id) ON DELETE SET NULL,
 artifact_revision integer NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(account_id,operation_key)
);
CREATE INDEX IF NOT EXISTS wren_media_scope_idx ON wren_media_operations(workspace_id,conversation_id,account_id,created_at DESC);
CREATE INDEX IF NOT EXISTS wren_media_rate_idx ON wren_media_operations(account_id,created_at DESC);

ALTER TABLE wren_media_operations ADD COLUMN IF NOT EXISTS visual_sha256 text NOT NULL DEFAULT '';
