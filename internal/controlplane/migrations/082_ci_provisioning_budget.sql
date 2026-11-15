CREATE TABLE ci_provisioning_budget (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    currency text NOT NULL CHECK (currency IN ('USD','EUR','CAD')),
    profile_digest text NOT NULL CHECK (profile_digest ~ '^[0-9a-f]{64}$'),
    max_concurrent integer NOT NULL CHECK (max_concurrent BETWEEN 1 AND 1000),
    max_per_workspace integer NOT NULL CHECK (max_per_workspace BETWEEN 1 AND max_concurrent),
    reservation_micros bigint NOT NULL CHECK (reservation_micros > 0),
    admit_before timestamptz NOT NULL,
    authorized_micros bigint NOT NULL DEFAULT 0 CHECK (authorized_micros >= 0),
    committed_micros bigint NOT NULL DEFAULT 0 CHECK (committed_micros BETWEEN 0 AND authorized_micros),
    updated_at timestamptz NOT NULL
);

CREATE TABLE ci_provisioning_authorizations (
    id text PRIMARY KEY CHECK (id ~ '^[A-Za-z0-9_-]{1,128}$'),
    request_digest text NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    amount_micros bigint NOT NULL CHECK (amount_micros > 0),
    created_at timestamptz NOT NULL
);

CREATE TABLE ci_provisioning_reservations (
    operation_id text PRIMARY KEY CHECK (operation_id ~ '^[A-Za-z0-9_-]{1,128}$'),
    workspace_id text REFERENCES workspaces(id) ON DELETE SET NULL,
    runner_id text UNIQUE REFERENCES ci_runners(id) ON DELETE SET NULL,
    plan_digest text NOT NULL CHECK (plan_digest ~ '^[0-9a-f]{64}$'),
    profile_digest text NOT NULL CHECK (profile_digest ~ '^[0-9a-f]{64}$'),
    currency text NOT NULL CHECK (currency IN ('USD','EUR','CAD')),
    reserved_micros bigint NOT NULL CHECK (reserved_micros > 0),
    reserved_at timestamptz NOT NULL,
    released_at timestamptz,
    CHECK (released_at IS NULL OR released_at >= reserved_at)
);

CREATE INDEX ci_provisioning_active_workspace
    ON ci_provisioning_reservations(workspace_id) WHERE released_at IS NULL;
