-- One executing run per conversation; additional user messages form a durable
-- inbox. The dispatcher yields at tool boundaries and claims these in order.
DROP INDEX IF EXISTS operator_one_active_run_idx;
CREATE UNIQUE INDEX IF NOT EXISTS operator_one_running_run_idx ON operator_runs(conversation_id) WHERE status='running';
CREATE INDEX IF NOT EXISTS operator_pending_runs_idx ON operator_runs(conversation_id,created_at,id) WHERE status IN ('queued','running');

CREATE TABLE IF NOT EXISTS operator_working_context (
    conversation_id text PRIMARY KEY REFERENCES operator_conversations(id) ON DELETE CASCADE,
    notes jsonb NOT NULL DEFAULT '{}',
    scratch jsonb NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (octet_length(notes::text) <= 16384),
    CHECK (octet_length(scratch::text) <= 2097152)
);
ALTER TABLE operator_tool_calls ADD COLUMN IF NOT EXISTS result_path text;
-- Preserve read handles for observations saved before the harness upgrade.
UPDATE operator_tool_calls SET result_path='/results/' || encode(sha256(convert_to(run_id,'UTF8') || decode('00','hex') || convert_to(call_id,'UTF8')),'hex') || '.json' WHERE result_path IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS operator_tool_result_path_idx ON operator_tool_calls(result_path) WHERE result_path IS NOT NULL;
