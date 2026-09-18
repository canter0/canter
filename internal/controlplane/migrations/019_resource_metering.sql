CREATE TABLE IF NOT EXISTS billing_resource_samples (
    workspace_id text NOT NULL REFERENCES workspace_billing(workspace_id) ON DELETE CASCADE,
    resource_id text NOT NULL,
    subscription_id text NOT NULL,
    period_start timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    units bigint NOT NULL CHECK (units >= 0),
    remainder numeric NOT NULL DEFAULT 0 CHECK (remainder >= 0),
    PRIMARY KEY(workspace_id,resource_id)
);
CREATE TABLE IF NOT EXISTS billing_collection_status (
    workspace_id text PRIMARY KEY REFERENCES workspace_billing(workspace_id) ON DELETE CASCADE,
    attempted_at timestamptz NOT NULL,
    succeeded_at timestamptz,
    issue text NOT NULL DEFAULT ''
);
