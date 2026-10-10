ALTER TABLE accounts ADD COLUMN IF NOT EXISTS onboarding_completed_at timestamptz;

-- Preserve established accounts while letting unused accounts see the welcome
-- they missed. Run this backfill only on the first application of the migration.
UPDATE accounts a SET onboarding_completed_at = now()
WHERE a.onboarding_completed_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '034_account_onboarding')
  AND (
    EXISTS (SELECT 1 FROM operator_conversations c WHERE c.account_id = a.id)
    OR EXISTS (SELECT 1 FROM agent_installations i WHERE i.created_by = a.id)
    OR EXISTS (
      SELECT 1 FROM memberships m JOIN systems s ON s.workspace_id = m.workspace_id
      WHERE m.account_id = a.id
    )
  );
