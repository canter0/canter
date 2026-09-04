ALTER TABLE initial_deployments
    DROP CONSTRAINT IF EXISTS initial_deployments_workspace_id_system_name_key;

CREATE UNIQUE INDEX IF NOT EXISTS initial_deployments_active_system_idx
    ON initial_deployments(workspace_id, system_name)
    WHERE phase <> 'failed';
