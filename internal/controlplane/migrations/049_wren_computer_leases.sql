ALTER TABLE wren_computers ADD COLUMN IF NOT EXISTS mutation_run_id text;
ALTER TABLE wren_computers ADD COLUMN IF NOT EXISTS mutation_lease_token text;
ALTER TABLE wren_computers ADD COLUMN IF NOT EXISTS mutation_expires_at timestamptz;
