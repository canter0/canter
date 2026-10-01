CREATE TABLE IF NOT EXISTS workspace_vps (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 phase text NOT NULL CHECK (phase IN ('drafted','queued','creating','ready','deleting','deleted')),
 document jsonb NOT NULL,
 delete_requested boolean NOT NULL DEFAULT false,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS workspace_vps_work_idx ON workspace_vps(next_attempt_at) WHERE phase NOT IN ('drafted','deleted');
CREATE INDEX IF NOT EXISTS workspace_vps_workspace_idx ON workspace_vps(workspace_id,created_at DESC);
