CREATE TABLE sandbox_transfers (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    sandbox_id text NOT NULL,
    generation bigint NOT NULL,
    plan jsonb NOT NULL,
    state text NOT NULL DEFAULT 'planned' CHECK (state IN ('planned','cancelled','complete','stale')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL DEFAULT now()+interval '24 hours',
    FOREIGN KEY (workspace_id,sandbox_id) REFERENCES sandboxes(workspace_id,id) ON DELETE CASCADE
);
CREATE INDEX sandbox_transfers_scope ON sandbox_transfers(workspace_id,sandbox_id,expires_at);

CREATE TABLE sandbox_transfer_files (
    transfer_id text NOT NULL REFERENCES sandbox_transfers(id) ON DELETE CASCADE,
    entry integer NOT NULL CHECK (entry>=0),
    state text NOT NULL CHECK (state IN ('staged','complete','conflict','failed','uncertain','skipped')),
    byte_offset bigint NOT NULL CHECK (byte_offset>=0),
    message text NOT NULL DEFAULT '',
    PRIMARY KEY (transfer_id,entry)
);
