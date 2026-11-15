-- The nonce is a nonsecret protocol identity, never a bearer capability.
CREATE TABLE ci_runner_bootstrap_grants (
    operation_id text PRIMARY KEY REFERENCES ci_runner_lifecycles(operation_id) ON DELETE CASCADE,
    bootstrap_id text NOT NULL UNIQUE CHECK (bootstrap_id ~ '^[0-9a-f]{64}$'),
    maximum_seconds bigint NOT NULL CHECK (maximum_seconds BETWEEN 1 AND 21600),
    expires_at timestamptz NOT NULL,
    attempted_at timestamptz,
    delivered_at timestamptz,
    created_at timestamptz NOT NULL,
    CHECK (delivered_at IS NULL OR attempted_at IS NOT NULL)
);
