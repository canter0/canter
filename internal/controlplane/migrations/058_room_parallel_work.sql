-- A Wren keeps one active assignment in a room; distinct Wrens can work
-- concurrently. Team-turn dependencies are enforced by the dispatcher.
DROP INDEX IF EXISTS operator_one_running_run_idx;
CREATE UNIQUE INDEX IF NOT EXISTS operator_running_speaker_idx
    ON operator_runs(conversation_id, (COALESCE(speaker->>'id','')))
    WHERE status='running';

-- Legacy room scratch/notes have no reliable author. Preserve them, but do
-- not copy them into anyone's private working context. Public memory and
-- conversation messages remain the deliberate shared context.
CREATE TABLE IF NOT EXISTS operator_room_working_context (
    conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
    speaker_id text NOT NULL CHECK (speaker_id<>''),
    requester_account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    notes jsonb NOT NULL DEFAULT '{}',
    scratch jsonb NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(conversation_id,speaker_id,requester_account_id),
    CHECK (octet_length(notes::text)<=16384),
    CHECK (octet_length(scratch::text)<=2097152)
);
