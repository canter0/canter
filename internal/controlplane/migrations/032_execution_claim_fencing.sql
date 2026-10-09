ALTER TABLE executions
    ADD COLUMN IF NOT EXISTS claim_token text NOT NULL DEFAULT '';
