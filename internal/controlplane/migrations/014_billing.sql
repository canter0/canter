CREATE TABLE IF NOT EXISTS workspace_billing (
    workspace_id text PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    customer_id text UNIQUE,
    subscription_id text UNIQUE,
    plan_id text NOT NULL DEFAULT 'payg' CHECK (plan_id IN ('payg','pro')),
    status text NOT NULL DEFAULT 'not_started',
    period_start timestamptz,
    period_end timestamptz,
    cancel_at_period_end boolean NOT NULL DEFAULT false,
    checkout_id text NOT NULL DEFAULT '',
    checkout_url text NOT NULL DEFAULT '',
    checkout_plan text NOT NULL DEFAULT '',
    checkout_expires_at timestamptz,
    checkout_attempt bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS billing_usage_events (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspace_billing(workspace_id) ON DELETE CASCADE,
    customer_id text NOT NULL,
    subscription_id text NOT NULL,
    amount_cents bigint NOT NULL CHECK (amount_cents > 0 AND amount_cents <= 100000000),
    resource text NOT NULL,
    rate_version text NOT NULL,
    occurred_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    first_attempt_at timestamptz,
    sent_at timestamptz,
    needs_reconciliation boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS billing_usage_workspace_period ON billing_usage_events(workspace_id, occurred_at);
CREATE INDEX IF NOT EXISTS billing_usage_pending ON billing_usage_events(created_at) WHERE sent_at IS NULL;
CREATE TABLE IF NOT EXISTS billing_webhook_events (
    id text PRIMARY KEY,
    received_at timestamptz NOT NULL DEFAULT now()
);
