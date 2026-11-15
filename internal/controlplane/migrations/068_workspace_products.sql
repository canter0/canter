CREATE TABLE IF NOT EXISTS workspace_products (
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 product text NOT NULL,
 enabled_at timestamptz NOT NULL DEFAULT now(),
 enabled_by_kind text NOT NULL DEFAULT 'human',
 enabled_by_id text,
 PRIMARY KEY(workspace_id,product)
);
