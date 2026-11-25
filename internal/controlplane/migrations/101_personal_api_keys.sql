CREATE TABLE IF NOT EXISTS personal_api_keys (
  id text PRIMARY KEY,
  account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  name text NOT NULL,
  token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash)=32),
  prefix text NOT NULL,
  auth_version bigint NOT NULL,
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  last_used_at timestamptz,
  revoked_at timestamptz,
  CHECK (expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS personal_api_keys_account_idx ON personal_api_keys(account_id,created_at DESC,id);
