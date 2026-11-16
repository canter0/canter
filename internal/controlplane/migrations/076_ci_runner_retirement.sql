ALTER TABLE ci_runners ADD COLUMN IF NOT EXISTS billing_customer_id text NOT NULL DEFAULT '';
ALTER TABLE ci_runners ADD COLUMN IF NOT EXISTS billing_subscription_id text NOT NULL DEFAULT '';
ALTER TABLE ci_runners ALTER COLUMN installation_id DROP NOT NULL;
ALTER TABLE ci_runners ALTER COLUMN trigger_job_id DROP NOT NULL;
ALTER TABLE ci_runners ADD CONSTRAINT ci_runners_live_ownership CHECK (
    phase='deleted' OR (installation_id IS NOT NULL AND trigger_job_id IS NOT NULL)
);
