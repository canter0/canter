ALTER TABLE github_repository_connections
    ADD COLUMN IF NOT EXISTS auth_provider text NOT NULL DEFAULT 'github'
        CHECK (auth_provider IN ('github', 'github-app')),
    ADD COLUMN IF NOT EXISTS encrypted_refresh_token bytea,
    ADD COLUMN IF NOT EXISTS refresh_expires_at timestamptz;
