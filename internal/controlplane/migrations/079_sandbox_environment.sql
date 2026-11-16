ALTER TABLE workspace_secrets ADD COLUMN sandbox_allowed boolean NOT NULL DEFAULT false;
ALTER TABLE workspace_secrets ADD CONSTRAINT workspace_secrets_sandbox_purpose CHECK (NOT sandbox_allowed OR purpose='stored');
ALTER TABLE sandboxes ADD COLUMN environment jsonb NOT NULL DEFAULT '{}';
ALTER TABLE sandboxes ADD COLUMN setup jsonb NOT NULL DEFAULT '{}';
ALTER TABLE sandboxes ADD CONSTRAINT sandboxes_environment_object CHECK (jsonb_typeof(environment)='object');
ALTER TABLE sandboxes ADD CONSTRAINT sandboxes_setup_object CHECK (jsonb_typeof(setup)='object');
