CREATE TABLE ci_settings (
    workspace_id text PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    paid_overrun_enabled boolean NOT NULL DEFAULT false,
    monthly_spend_limit_cents bigint NOT NULL DEFAULT 1000 CHECK (monthly_spend_limit_cents BETWEEN 1 AND 100000),
    cache_enabled boolean NOT NULL DEFAULT true,
    retain_failed_minutes integer NOT NULL DEFAULT 0 CHECK (retain_failed_minutes BETWEEN 0 AND 30),
    default_size text NOT NULL DEFAULT 'linux-x64-4' CHECK (default_size IN ('linux-x64-2','linux-x64-4')),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ci_github_installations (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    github_installation_id bigint NOT NULL UNIQUE CHECK (github_installation_id > 0),
    github_account_id bigint NOT NULL CHECK (github_account_id > 0),
    owner text NOT NULL,
    account_type text NOT NULL CHECK (account_type IN ('Organization','User')),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended','removed')),
    created_by text REFERENCES accounts(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id,id)
);

CREATE TABLE ci_repositories (
    workspace_id text NOT NULL,
    installation_id text NOT NULL,
    github_repository_id bigint NOT NULL CHECK (github_repository_id > 0),
    full_name text NOT NULL,
    private boolean NOT NULL DEFAULT true,
    default_branch text NOT NULL DEFAULT 'main',
    enabled boolean NOT NULL DEFAULT true,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id,github_repository_id),
    FOREIGN KEY (workspace_id,installation_id) REFERENCES ci_github_installations(workspace_id,id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX ci_repositories_name ON ci_repositories(installation_id,lower(full_name));

CREATE TABLE ci_webhook_deliveries (
    delivery_id text PRIMARY KEY CHECK (length(delivery_id) BETWEEN 1 AND 200),
    body_sha256 text NOT NULL CHECK (length(body_sha256)=64),
    event text NOT NULL CHECK (length(event) BETWEEN 1 AND 80),
    received_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ci_jobs (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    installation_id text NOT NULL,
    github_repository_id bigint NOT NULL,
    github_job_id bigint NOT NULL CHECK (github_job_id > 0),
    github_run_id bigint NOT NULL CHECK (github_run_id > 0),
    status text NOT NULL CHECK (status IN ('queued','provisioning','running','completed','cancelled','blocked')),
    runner_id text,
    document jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (installation_id,github_job_id),
    UNIQUE (workspace_id,id),
    FOREIGN KEY (workspace_id,installation_id) REFERENCES ci_github_installations(workspace_id,id),
    FOREIGN KEY (installation_id,github_repository_id) REFERENCES ci_repositories(installation_id,github_repository_id)
);
CREATE INDEX ci_jobs_workspace_history ON ci_jobs(workspace_id,created_at DESC,id DESC);
CREATE INDEX ci_jobs_queue ON ci_jobs(created_at,id) WHERE status='queued';

CREATE TABLE ci_runners (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    installation_id text NOT NULL,
    size_id text NOT NULL CHECK (size_id IN ('linux-x64-2','linux-x64-4')),
    trigger_job_id text NOT NULL,
    assigned_job_id text,
    github_runner_id bigint,
    phase text NOT NULL CHECK (phase IN ('queued','provisioning','registering','idle','running','retaining','deleting','deleted','cleanup_failed')),
    document jsonb NOT NULL,
    rate_version text NOT NULL DEFAULT 'ci-free-hour-2026-11-v1',
    rate_micros_per_minute bigint NOT NULL,
    paid_overrun_enabled boolean NOT NULL DEFAULT false,
    billing_customer_id text NOT NULL DEFAULT '',
    billing_subscription_id text NOT NULL DEFAULT '',
    execution_started_at timestamptz,
    execution_stopped_at timestamptz,
    deadline_at timestamptz,
    metered_billable_seconds bigint NOT NULL DEFAULT 0 CHECK (metered_billable_seconds >= 0),
    delete_requested boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id,id),
    UNIQUE (installation_id,github_runner_id),
    FOREIGN KEY (workspace_id,installation_id) REFERENCES ci_github_installations(workspace_id,id),
    FOREIGN KEY (workspace_id,trigger_job_id) REFERENCES ci_jobs(workspace_id,id),
    FOREIGN KEY (workspace_id,assigned_job_id) REFERENCES ci_jobs(workspace_id,id),
    CHECK ((size_id='linux-x64-2' AND rate_micros_per_minute=2000) OR (size_id='linux-x64-4' AND rate_micros_per_minute=4000))
);
CREATE INDEX ci_runners_live ON ci_runners(phase,created_at) WHERE phase<>'deleted';
ALTER TABLE ci_jobs ADD FOREIGN KEY (workspace_id,runner_id) REFERENCES ci_runners(workspace_id,id);

CREATE TABLE ci_usage_balances (
    workspace_id text PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    subscription_id text NOT NULL,
    remainder_units bigint NOT NULL DEFAULT 0 CHECK (remainder_units BETWEEN 0 AND 599999),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ci_usage_ledger (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    runner_id text NOT NULL,
    through_seconds bigint NOT NULL CHECK (through_seconds > 0),
    period_start date NOT NULL,
    delta_units bigint NOT NULL CHECK (delta_units > 0),
    rate_version text NOT NULL,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (runner_id,through_seconds),
    FOREIGN KEY (workspace_id,runner_id) REFERENCES ci_runners(workspace_id,id)
);
CREATE INDEX ci_usage_ledger_period ON ci_usage_ledger(workspace_id,period_start);

CREATE TABLE ci_budget_reservations (
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    runner_id text NOT NULL,
    period_start date NOT NULL,
    reserved_units bigint NOT NULL CHECK (reserved_units >= 0),
    PRIMARY KEY (runner_id,period_start),
    FOREIGN KEY (workspace_id,runner_id) REFERENCES ci_runners(workspace_id,id)
);

CREATE TABLE ci_job_logs (
    workspace_id text NOT NULL,
    job_id text NOT NULL,
    sequence bigint NOT NULL CHECK (sequence >= 0),
    stream text NOT NULL CHECK (stream IN ('stdout','stderr','system')),
    content text NOT NULL CHECK (octet_length(content) <= 16384),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id,sequence),
    FOREIGN KEY (workspace_id,job_id) REFERENCES ci_jobs(workspace_id,id) ON DELETE CASCADE
);

CREATE TABLE ci_job_metrics (
    workspace_id text NOT NULL,
    job_id text NOT NULL,
    sampled_at timestamptz NOT NULL,
    document jsonb NOT NULL,
    PRIMARY KEY (job_id,sampled_at),
    FOREIGN KEY (workspace_id,job_id) REFERENCES ci_jobs(workspace_id,id) ON DELETE CASCADE
);

CREATE TABLE ci_job_tests (
    workspace_id text NOT NULL,
    job_id text NOT NULL,
    id text NOT NULL,
    document jsonb NOT NULL,
    PRIMARY KEY (job_id,id),
    FOREIGN KEY (workspace_id,job_id) REFERENCES ci_jobs(workspace_id,id) ON DELETE CASCADE
);
