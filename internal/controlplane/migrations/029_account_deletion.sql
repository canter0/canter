-- Private account data is removed with the account. Shared workspace records
-- retain their meaning without retaining a reference to a deleted identity.
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_account_id_fkey;
ALTER TABLE memberships ADD CONSTRAINT memberships_account_id_fkey FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE;
ALTER TABLE human_sessions DROP CONSTRAINT IF EXISTS human_sessions_account_id_fkey;
ALTER TABLE human_sessions ADD CONSTRAINT human_sessions_account_id_fkey FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE;
ALTER TABLE operator_conversations DROP CONSTRAINT IF EXISTS operator_conversations_account_id_fkey;
ALTER TABLE operator_conversations ADD CONSTRAINT operator_conversations_account_id_fkey FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE;
ALTER TABLE operator_grants DROP CONSTRAINT IF EXISTS operator_grants_account_id_fkey;
ALTER TABLE operator_grants ADD CONSTRAINT operator_grants_account_id_fkey FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE;
ALTER TABLE agent_pairings DROP CONSTRAINT IF EXISTS agent_pairings_created_by_fkey;
ALTER TABLE agent_pairings ADD CONSTRAINT agent_pairings_created_by_fkey FOREIGN KEY(created_by) REFERENCES accounts(id) ON DELETE CASCADE;

ALTER TABLE agent_installations ALTER COLUMN created_by DROP NOT NULL;
ALTER TABLE agent_installations DROP CONSTRAINT IF EXISTS agent_installations_created_by_fkey;
ALTER TABLE agent_installations ADD CONSTRAINT agent_installations_created_by_fkey FOREIGN KEY(created_by) REFERENCES accounts(id) ON DELETE SET NULL;
ALTER TABLE device_authorizations DROP CONSTRAINT IF EXISTS device_authorizations_authorized_by_fkey;
ALTER TABLE device_authorizations ADD CONSTRAINT device_authorizations_authorized_by_fkey FOREIGN KEY(authorized_by) REFERENCES accounts(id) ON DELETE SET NULL;
ALTER TABLE workspace_tasks ALTER COLUMN requested_by DROP NOT NULL;
ALTER TABLE workspace_tasks DROP CONSTRAINT IF EXISTS workspace_tasks_requested_by_fkey;
ALTER TABLE workspace_tasks ADD CONSTRAINT workspace_tasks_requested_by_fkey FOREIGN KEY(requested_by) REFERENCES accounts(id) ON DELETE SET NULL;
ALTER TABLE standing_policies ALTER COLUMN created_by_account DROP NOT NULL;
ALTER TABLE standing_policies DROP CONSTRAINT IF EXISTS standing_policies_created_by_account_fkey;
ALTER TABLE standing_policies ADD CONSTRAINT standing_policies_created_by_account_fkey FOREIGN KEY(created_by_account) REFERENCES accounts(id) ON DELETE SET NULL;
ALTER TABLE standing_policies DROP CONSTRAINT IF EXISTS standing_policies_revoked_by_account_fkey;
ALTER TABLE standing_policies ADD CONSTRAINT standing_policies_revoked_by_account_fkey FOREIGN KEY(revoked_by_account) REFERENCES accounts(id) ON DELETE SET NULL;
ALTER TABLE workspace_secrets ALTER COLUMN updated_by DROP NOT NULL;
ALTER TABLE workspace_secrets DROP CONSTRAINT IF EXISTS workspace_secrets_updated_by_fkey;
ALTER TABLE workspace_secrets ADD CONSTRAINT workspace_secrets_updated_by_fkey FOREIGN KEY(updated_by) REFERENCES accounts(id) ON DELETE SET NULL;
ALTER TABLE change_approval_capabilities DROP CONSTRAINT IF EXISTS change_approval_capabilities_consumed_by_fkey;
ALTER TABLE change_approval_capabilities ADD CONSTRAINT change_approval_capabilities_consumed_by_fkey FOREIGN KEY(consumed_by) REFERENCES accounts(id) ON DELETE SET NULL;

ALTER TABLE auth_email_outbox ADD COLUMN IF NOT EXISTS recipient_hash bytea;
CREATE INDEX IF NOT EXISTS auth_email_outbox_recipient_idx ON auth_email_outbox(recipient_hash);

-- Remote artifact erasure is retried independently of the deleted identity.
CREATE TABLE IF NOT EXISTS account_deletion_artifacts (
    storage_key text PRIMARY KEY,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now()
);
