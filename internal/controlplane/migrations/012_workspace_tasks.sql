CREATE TABLE IF NOT EXISTS workspace_tasks (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    prompt text NOT NULL,
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','working','completed','failed')),
    requested_by text NOT NULL REFERENCES accounts(id),
    target_installation_id text REFERENCES agent_installations(id),
    claimed_by text REFERENCES agent_installations(id),
    result text NOT NULL DEFAULT '',
    model text NOT NULL DEFAULT 'gpt-5.6-luna',
    reasoning text NOT NULL DEFAULT 'medium',
    context jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS workspace_tasks_workspace_time_idx ON workspace_tasks(workspace_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS workspace_tasks_one_working_per_agent_idx ON workspace_tasks(claimed_by) WHERE status='working';
