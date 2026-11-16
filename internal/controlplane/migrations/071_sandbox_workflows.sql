ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS labels jsonb NOT NULL DEFAULT '{}';
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS idle_timeout_seconds integer NOT NULL DEFAULT -1;
DO $$
BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'sandbox_idle_timeout_valid' AND conrelid = 'sandboxes'::regclass) THEN
		ALTER TABLE sandboxes ADD CONSTRAINT sandbox_idle_timeout_valid
			CHECK (idle_timeout_seconds=-1 OR idle_timeout_seconds=0 OR idle_timeout_seconds BETWEEN 60 AND 86400);
	END IF;
END $$;
CREATE TABLE IF NOT EXISTS sandbox_create_requests (
	workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	key_hash text NOT NULL,
	request_hash text NOT NULL,
	sandbox_id text NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
	created_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY(workspace_id,key_hash)
);
