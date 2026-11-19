ALTER TABLE personal_customizations DROP CONSTRAINT personal_customizations_kind_check;
ALTER TABLE personal_customizations ADD CONSTRAINT personal_customizations_kind_check CHECK(kind IN ('rule','hook','skill'));
ALTER TABLE personal_customizations ADD COLUMN action text NOT NULL DEFAULT '';
ALTER TABLE personal_customizations ADD COLUMN description text NOT NULL DEFAULT '';
ALTER TABLE personal_customizations ADD COLUMN source text NOT NULL DEFAULT '';
ALTER TABLE personal_customizations ADD COLUMN source_revision text NOT NULL DEFAULT '';
UPDATE personal_customizations SET action=CASE WHEN event='PreToolUse' THEN 'block' ELSE 'agent' END;

CREATE TABLE personal_skill_files (
 customization_id text NOT NULL REFERENCES personal_customizations(id) ON DELETE CASCADE,
 path text NOT NULL,
 content bytea NOT NULL,
 PRIMARY KEY(customization_id,path)
);
CREATE TABLE customization_operations (
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 operation_key text NOT NULL,
 digest text NOT NULL,
 result jsonb NOT NULL,
 PRIMARY KEY(workspace_id,account_id,operation_key)
);
CREATE TABLE personal_hook_firings (
 run_id text NOT NULL REFERENCES operator_runs(id) ON DELETE CASCADE,
 call_id text NOT NULL,
 customization_id text NOT NULL REFERENCES personal_customizations(id) ON DELETE CASCADE,
 revision integer NOT NULL,
 instructions text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(run_id,call_id,customization_id)
);
