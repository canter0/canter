ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS desktop boolean NOT NULL DEFAULT false;
ALTER TABLE sandbox_mutations ADD COLUMN IF NOT EXISTS desktop_input boolean NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS sandbox_desktop_control (
  sandbox_id text PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,
  account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  generation integer NOT NULL,
  lease_token text NOT NULL,
  expires_at timestamptz NOT NULL
);
