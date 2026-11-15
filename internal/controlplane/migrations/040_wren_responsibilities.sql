CREATE TABLE IF NOT EXISTS wren_responsibilities (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
    owner_id text REFERENCES accounts(id) ON DELETE SET NULL,
    wren_id text REFERENCES wrens(id) ON DELETE SET NULL,
    name text NOT NULL,
    instruction text NOT NULL,
    schedule jsonb NOT NULL,
    state text NOT NULL CHECK(state IN ('active','paused','revoked')),
    revision integer NOT NULL CHECK(revision>0),
    next_run_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS wren_responsibility_wakes (
    id text PRIMARY KEY,
    responsibility_id text NOT NULL REFERENCES wren_responsibilities(id) ON DELETE CASCADE,
    accepted_revision integer NOT NULL,
    event_key text NOT NULL,
    trigger_kind text NOT NULL CHECK(trigger_kind IN ('schedule','manual','event')),
    scheduled_for timestamptz NOT NULL,
    time_zone text NOT NULL DEFAULT 'UTC',
    instruction text NOT NULL,
    wren_snapshot jsonb NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}',
    state text NOT NULL CHECK(state IN ('queued','dispatched','failed','cancelled')),
    run_id text UNIQUE REFERENCES operator_runs(id) ON DELETE SET NULL,
    failure text NOT NULL DEFAULT '',
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(responsibility_id,event_key)
);
ALTER TABLE operator_runs ADD COLUMN IF NOT EXISTS responsibility_id text REFERENCES wren_responsibilities(id) ON DELETE SET NULL;
ALTER TABLE operator_runs ADD COLUMN IF NOT EXISTS origin_kind text NOT NULL DEFAULT 'human' CHECK(origin_kind IN ('human','responsibility','job'));
CREATE INDEX IF NOT EXISTS wren_responsibilities_due_idx ON wren_responsibilities(next_run_at,id) WHERE state='active' AND next_run_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS wren_responsibilities_origin_idx ON wren_responsibilities(workspace_id,conversation_id,created_at DESC);
CREATE INDEX IF NOT EXISTS wren_responsibility_outbox_idx ON wren_responsibility_wakes(next_attempt_at,id) WHERE state='queued';
CREATE INDEX IF NOT EXISTS wren_responsibility_runs_idx ON operator_runs(responsibility_id) WHERE responsibility_id IS NOT NULL;

ALTER TABLE wren_responsibility_wakes ADD COLUMN IF NOT EXISTS time_zone text NOT NULL DEFAULT 'UTC';
