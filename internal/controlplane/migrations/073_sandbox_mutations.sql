CREATE TABLE IF NOT EXISTS sandbox_mutations (
 token text PRIMARY KEY,
 sandbox_id text NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
 generation integer NOT NULL,
 expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS sandbox_mutations_active_idx ON sandbox_mutations(sandbox_id,expires_at);
