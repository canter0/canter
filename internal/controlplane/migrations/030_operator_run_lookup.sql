-- Support the latest-run status lookup used by the conversation list.
CREATE INDEX IF NOT EXISTS operator_runs_conversation_created_idx
    ON operator_runs(conversation_id, created_at DESC) INCLUDE (status);
