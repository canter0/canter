ALTER TABLE accounts ADD COLUMN IF NOT EXISTS email_verified_at timestamptz;
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS auth_version bigint NOT NULL DEFAULT 0;
ALTER TABLE human_sessions ADD COLUMN IF NOT EXISTS auth_version bigint NOT NULL DEFAULT 0;
ALTER TABLE human_sessions ADD COLUMN IF NOT EXISTS authenticated_at timestamptz;
UPDATE human_sessions SET authenticated_at=created_at WHERE authenticated_at IS NULL;
ALTER TABLE human_sessions ALTER COLUMN authenticated_at SET DEFAULT now();
ALTER TABLE human_sessions ALTER COLUMN authenticated_at SET NOT NULL;
ALTER TABLE human_sessions ADD COLUMN IF NOT EXISTS mfa_verified_at timestamptz;
ALTER TABLE human_sessions ADD COLUMN IF NOT EXISTS user_agent text NOT NULL DEFAULT '';
ALTER TABLE human_sessions ADD COLUMN IF NOT EXISTS ip_address text NOT NULL DEFAULT '';

-- Only previously verified provider identities establish email ownership. Never
-- grandfather password registrations or overwrite an existing verification.
UPDATE accounts a SET email_verified_at=a.created_at
WHERE a.email_verified_at IS NULL AND EXISTS
 (SELECT 1 FROM oauth_identities i WHERE i.account_id=a.id AND lower(i.email)=a.email);

CREATE TABLE IF NOT EXISTS account_totp (
 account_id text PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
 key_id text NOT NULL, ciphertext bytea NOT NULL,
 enabled_at timestamptz, pending_until timestamptz,
 last_step bigint NOT NULL DEFAULT -1
);
CREATE TABLE IF NOT EXISTS account_recovery_codes (
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 code_hash bytea NOT NULL, used_at timestamptz,
 PRIMARY KEY(account_id,code_hash)
);
CREATE TABLE IF NOT EXISTS account_passkeys (
 id text PRIMARY KEY, account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 credential_id bytea NOT NULL UNIQUE, name text NOT NULL,
 key_id text NOT NULL, ciphertext bytea NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), last_used_at timestamptz
);
CREATE INDEX IF NOT EXISTS account_passkeys_account_idx ON account_passkeys(account_id);
CREATE TABLE IF NOT EXISTS auth_challenges (
 token_hash bytea PRIMARY KEY, purpose text NOT NULL,
 account_id text REFERENCES accounts(id) ON DELETE CASCADE,
 email text NOT NULL DEFAULT '', code_hash bytea,
 auth_version bigint NOT NULL DEFAULT 0,
 session_id text REFERENCES human_sessions(id) ON DELETE CASCADE,
 payload jsonb NOT NULL DEFAULT '{}',
 attempts integer NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS auth_challenges_expiry_idx ON auth_challenges(expires_at);
CREATE TABLE IF NOT EXISTS auth_rate_windows (
 bucket_hash bytea PRIMARY KEY, count integer NOT NULL, expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS account_security_events (
 id text PRIMARY KEY, account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 action text NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS account_security_events_account_idx ON account_security_events(account_id,created_at DESC);
CREATE TABLE IF NOT EXISTS auth_email_outbox (
 id text PRIMARY KEY, key_id text NOT NULL, ciphertext bytea,
 account_id text REFERENCES accounts(id) ON DELETE CASCADE,
 attempts integer NOT NULL DEFAULT 0, next_attempt_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL, status text NOT NULL DEFAULT 'queued',
 provider_id text UNIQUE, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS auth_email_outbox_queue_idx ON auth_email_outbox(next_attempt_at) WHERE status='queued';
CREATE TABLE IF NOT EXISTS auth_email_suppressions (
 email_hash bytea PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now()
);
