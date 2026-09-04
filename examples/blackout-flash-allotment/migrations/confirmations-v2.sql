ALTER TABLE claims ADD COLUMN IF NOT EXISTS confirmed_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS active_claim_confirmation_idx
  ON claims (confirmed_at) WHERE active;
