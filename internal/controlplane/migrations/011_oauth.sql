CREATE TABLE IF NOT EXISTS oauth_identities (
    provider text NOT NULL CHECK (provider IN ('google', 'github')),
    subject text NOT NULL,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    email text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (provider, subject),
    UNIQUE (account_id, provider)
);

CREATE TABLE IF NOT EXISTS oauth_login_states (
    state_hash bytea PRIMARY KEY,
    browser_hash bytea NOT NULL,
    provider text NOT NULL,
    verifier text NOT NULL,
    nonce text NOT NULL,
    next_path text NOT NULL,
    mode text NOT NULL,
    invite_hash bytea NOT NULL,
    link_account_id text REFERENCES accounts(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS oauth_login_states_expiry_idx ON oauth_login_states(expires_at);
