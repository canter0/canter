ALTER TABLE initial_deployment_executions
    DROP CONSTRAINT IF EXISTS initial_deployment_executions_deployment_id_key;

CREATE UNIQUE INDEX IF NOT EXISTS initial_deployment_executions_active_deployment_idx
    ON initial_deployment_executions(deployment_id)
    WHERE phase IN ('queued', 'running');
