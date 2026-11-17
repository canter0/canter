ALTER TABLE sandboxes ADD UNIQUE (workspace_id,id);

ALTER TABLE sandbox_jobs
    ADD COLUMN workspace_id text,
    ADD COLUMN generation bigint,
    ADD COLUMN history_kind text NOT NULL DEFAULT 'command',
    ADD COLUMN actor_kind text NOT NULL DEFAULT '',
    ADD COLUMN actor_id text NOT NULL DEFAULT '',
    ADD COLUMN history_status text NOT NULL DEFAULT 'unknown',
    ADD COLUMN history_observed_at timestamptz,
    ADD COLUMN history_terminal_at timestamptz,
    ADD COLUMN history_exit_code integer,
    ADD COLUMN history_output_state text NOT NULL DEFAULT 'not_collected'
        CHECK (history_output_state IN ('not_collected','collecting','complete','incomplete','truncated','expired','unavailable')),
    ADD COLUMN history_next_offset integer NOT NULL DEFAULT 0 CHECK (history_next_offset BETWEEN 0 AND 1048576),
    ADD COLUMN history_nonce text NOT NULL DEFAULT '',
    ADD COLUMN history_lease_until timestamptz,
    ADD COLUMN history_next_attempt timestamptz NOT NULL DEFAULT now(),
    ADD FOREIGN KEY (workspace_id,sandbox_id) REFERENCES sandboxes(workspace_id,id) ON DELETE CASCADE;

UPDATE sandbox_jobs j SET workspace_id=s.workspace_id
FROM sandboxes s WHERE s.id=j.sandbox_id;

CREATE INDEX sandbox_job_history_recent ON sandbox_jobs(workspace_id,sandbox_id,started_at DESC,job_id DESC);
CREATE INDEX sandbox_job_history_collect ON sandbox_jobs(history_next_attempt,sandbox_id,job_id)
    WHERE history_output_state='collecting';
CREATE INDEX sandbox_job_history_expiry ON sandbox_jobs(history_terminal_at)
    WHERE history_next_offset>0;

CREATE TABLE sandbox_job_history_settings (
    workspace_id text PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    retain_output boolean NOT NULL
);

CREATE TABLE sandbox_job_history_chunks (
    sandbox_id text NOT NULL,
    job_id text NOT NULL,
    byte_offset integer NOT NULL CHECK (byte_offset >= 0),
    byte_size integer NOT NULL CHECK (byte_size BETWEEN 1 AND 65536),
    key_id text NOT NULL,
    ciphertext bytea NOT NULL,
    PRIMARY KEY (sandbox_id,job_id,byte_offset),
    FOREIGN KEY (sandbox_id,job_id) REFERENCES sandbox_jobs(sandbox_id,job_id) ON DELETE CASCADE
);

CREATE FUNCTION erase_deleted_sandbox_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM sandbox_jobs WHERE sandbox_id=NEW.id;
    RETURN NEW;
END;
$$;

CREATE TRIGGER erase_deleted_sandbox_history AFTER UPDATE OF phase ON sandboxes
    FOR EACH ROW WHEN (OLD.phase IS DISTINCT FROM NEW.phase AND NEW.phase='deleted')
    EXECUTE FUNCTION erase_deleted_sandbox_history();
