-- Recoverable batch receipts: settled-but-ambiguous tasks keep reservations
-- until positive evidence; confirmed generation removal releases them.
ALTER TABLE super_sandbox_tasks DROP CONSTRAINT super_sandbox_tasks_state_check,
 ADD CONSTRAINT super_sandbox_tasks_state_check CHECK (state IN ('queued','starting','running','unknown','succeeded','failed','cancelled','outcome_unavailable')),
 ADD COLUMN reason text,
 ADD COLUMN resolution_mode text CHECK (resolution_mode IN ('close_rejected_start')),
 ADD COLUMN last_attempt_at timestamptz;
-- Durable provider evidence written only after DestroyVPS confirms removal of
-- this exact generation; idempotent facts, never inferred from mutable rows.
CREATE TABLE sandbox_destroyed_generations (
 workspace_id text NOT NULL,
 sandbox_id text NOT NULL,
 generation integer NOT NULL,
 confirmed_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,sandbox_id,generation),
 FOREIGN KEY(workspace_id,sandbox_id) REFERENCES sandboxes(workspace_id,id) ON DELETE CASCADE
);
CREATE INDEX super_sandboxes_page ON super_sandboxes(workspace_id,created_at DESC,id DESC);
