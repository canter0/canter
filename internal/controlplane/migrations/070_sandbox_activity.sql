CREATE TABLE IF NOT EXISTS sandbox_jobs (
 sandbox_id text NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
 job_id text NOT NULL,
 started_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 PRIMARY KEY(sandbox_id,job_id)
);
CREATE TABLE IF NOT EXISTS sandbox_previews (
 id text PRIMARY KEY,
 sandbox_id text NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 port integer NOT NULL,
 token text NOT NULL UNIQUE,
 created_by_kind text NOT NULL DEFAULT 'human',
 created_by_id text,
 created_at timestamptz NOT NULL DEFAULT now(),
 revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS sandbox_previews_sandbox_idx ON sandbox_previews(sandbox_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS sandbox_previews_token_idx ON sandbox_previews(token) WHERE revoked_at IS NULL;
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS notice text NOT NULL DEFAULT '';
