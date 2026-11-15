CREATE TABLE IF NOT EXISTS wren_computer_commands (
    computer_id text NOT NULL REFERENCES wren_computers(id) ON DELETE CASCADE,
    command_id text NOT NULL,
    status text NOT NULL DEFAULT 'unconfirmed',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(computer_id, command_id)
);
-- Preserve commands launched by older runtimes, including replies that ended
-- before their shell job. A receipt's terminal state is still checked remotely
-- before it can clear a takeover barrier.
INSERT INTO wren_computer_commands(computer_id,command_id)
SELECT DISTINCT c.id,COALESCE(t.result->>'id','command_'||substr(encode(sha256(convert_to(r.id||':'||(t.arguments->>'operationKey'),'UTF8')),'hex'),1,32))
FROM operator_tool_calls t JOIN operator_runs r ON r.id=t.run_id
LEFT JOIN wren_jobs j ON j.child_conversation_id=r.conversation_id
JOIN wren_computers c ON c.conversation_id=COALESCE(j.origin_conversation_id,r.conversation_id)
WHERE t.name='canter_computer_run' AND (t.result->>'id' LIKE 'command_%' OR COALESCE(t.arguments->>'operationKey','')<>'')
AND c.phase<>'deleted' AND t.created_at>=c.created_at AND t.created_at<=c.expires_at
ON CONFLICT DO NOTHING;
CREATE INDEX IF NOT EXISTS wren_computer_commands_pending_idx ON wren_computer_commands(computer_id)
WHERE status NOT IN ('completed','cancelled','failed','timed_out','not_started');
