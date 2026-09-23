ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS agent_authority jsonb NOT NULL DEFAULT '{"inspect":true,"draft":true,"applyMode":"automatic"}';
-- Existing grants remain individual overrides; adding defaults must not widen them.
ALTER TABLE agent_installations ADD COLUMN IF NOT EXISTS use_workspace_authority boolean NOT NULL DEFAULT false;
