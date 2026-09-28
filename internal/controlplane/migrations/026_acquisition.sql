-- Anonymous first visits expire after 30 days. Only fixed source categories and
-- public landing paths are accepted; no URLs, IPs or query strings are stored.
CREATE TABLE IF NOT EXISTS acquisition_visits (
    token_hash bytea PRIMARY KEY,
    source text NOT NULL CHECK (source IN ('direct','google','bing','duckduckgo','yahoo','referral','paid','campaign')),
    landing_path text NOT NULL CHECK (landing_path IN ('/','/pricing')),
    occurred_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS acquisition_visits_expiry ON acquisition_visits(expires_at);

CREATE TABLE IF NOT EXISTS acquisition_daily (
    day date NOT NULL,
    source text NOT NULL,
    landing_path text NOT NULL,
    visitors bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (day,source,landing_path)
);

CREATE TABLE IF NOT EXISTS account_acquisition (
    account_id text PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    workspace_id text NOT NULL UNIQUE REFERENCES workspaces(id) ON DELETE CASCADE,
    visit_hash bytea NOT NULL UNIQUE,
    source text NOT NULL,
    landing_path text NOT NULL,
    occurred_at timestamptz NOT NULL,
    signed_up_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS account_acquisition_cohort ON account_acquisition(occurred_at);

-- Settled invoice amounts are separate from metered usage. They include taxes
-- and are gross payments, not net revenue or profit. Refunds are not represented.
CREATE TABLE IF NOT EXISTS billing_paid_invoices (
    invoice_id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    amount_paid bigint NOT NULL CHECK (amount_paid > 0),
    currency text NOT NULL,
    paid_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS billing_paid_invoices_workspace ON billing_paid_invoices(workspace_id,paid_at);

CREATE OR REPLACE VIEW acquisition_funnel_daily AS
WITH outcomes AS (
    SELECT (a.occurred_at AT TIME ZONE 'UTC')::date AS day,a.source,a.landing_path,
        count(*) AS signups,
        count(*) FILTER (WHERE EXISTS(SELECT 1 FROM agent_installations i WHERE i.workspace_id=a.workspace_id)) AS connected_agents,
        count(*) FILTER (WHERE EXISTS(SELECT 1 FROM initial_deployment_executions e WHERE e.workspace_id=a.workspace_id AND e.phase='succeeded')) AS deployed_workspaces,
        count(*) FILTER (WHERE EXISTS(SELECT 1 FROM billing_usage_events u WHERE u.workspace_id=a.workspace_id)) AS usage_workspaces,
        count(*) FILTER (WHERE EXISTS(SELECT 1 FROM billing_paid_invoices p WHERE p.workspace_id=a.workspace_id)) AS paid_workspaces,
        sum(COALESCE((SELECT sum(p.amount_paid) FROM billing_paid_invoices p WHERE p.workspace_id=a.workspace_id AND p.currency='usd'),0)) AS gross_paid_usd_cents
    FROM account_acquisition a GROUP BY 1,2,3
)
SELECT d.day,d.source,d.landing_path,d.visitors,
    COALESCE(o.signups,0) AS signups,
    COALESCE(o.connected_agents,0) AS connected_agents,
    COALESCE(o.deployed_workspaces,0) AS deployed_workspaces,
    COALESCE(o.usage_workspaces,0) AS usage_workspaces,
    COALESCE(o.paid_workspaces,0) AS paid_workspaces,
    COALESCE(o.gross_paid_usd_cents,0) AS gross_paid_usd_cents
FROM acquisition_daily d LEFT JOIN outcomes o USING(day,source,landing_path);
