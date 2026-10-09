CREATE INDEX IF NOT EXISTS change_records_workspace_page_idx
    ON change_records (workspace_id, created_at DESC, change_id DESC);

CREATE INDEX IF NOT EXISTS change_records_workspace_pending_page_idx
    ON change_records (workspace_id, created_at DESC, change_id DESC)
    WHERE phase NOT IN ('committed','rejected','reverted');

CREATE INDEX IF NOT EXISTS initial_deployments_workspace_page_idx
    ON initial_deployments (workspace_id, created_at DESC, id DESC);

DROP INDEX IF EXISTS change_records_workspace_time_idx;
DROP INDEX IF EXISTS initial_deployments_workspace_time_idx;
