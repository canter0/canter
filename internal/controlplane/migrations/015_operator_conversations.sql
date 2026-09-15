CREATE TABLE IF NOT EXISTS operator_conversations (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    account_id text NOT NULL REFERENCES accounts(id),
    title text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS operator_conversations_owner_idx ON operator_conversations(workspace_id,account_id,updated_at DESC);
CREATE TABLE IF NOT EXISTS operator_runs (
    id text PRIMARY KEY,
    conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
    request_id text NOT NULL,
    status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','running','completed','failed','cancelled')),
    model text NOT NULL,
    checkpoint jsonb NOT NULL DEFAULT '[]',
    steps integer NOT NULL DEFAULT 0,
    lease_token text,
    lease_expires_at timestamptz,
    failure text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    UNIQUE(conversation_id,request_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS operator_one_active_run_idx ON operator_runs(conversation_id) WHERE status IN ('queued','running');
CREATE TABLE IF NOT EXISTS operator_messages (
    id text PRIMARY KEY,
    conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
    run_id text NOT NULL REFERENCES operator_runs(id) ON DELETE CASCADE,
    role text NOT NULL CHECK(role IN ('user','assistant')),
    content text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(run_id,role)
);
CREATE TABLE IF NOT EXISTS operator_events (
    sequence bigserial PRIMARY KEY,
    conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
    run_id text REFERENCES operator_runs(id) ON DELETE CASCADE,
    kind text NOT NULL,
    data jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS operator_events_conversation_idx ON operator_events(conversation_id,sequence);
CREATE TABLE IF NOT EXISTS operator_tool_calls (
    run_id text NOT NULL REFERENCES operator_runs(id) ON DELETE CASCADE,
    call_id text NOT NULL,
    name text NOT NULL,
    arguments jsonb NOT NULL,
    result jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(run_id,call_id)
);
CREATE TABLE IF NOT EXISTS operator_grants (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    account_id text NOT NULL REFERENCES accounts(id),
    installation_id text NOT NULL REFERENCES agent_installations(id),
    PRIMARY KEY(workspace_id,account_id)
);

ALTER TABLE operator_messages ADD COLUMN IF NOT EXISTS surface jsonb;
