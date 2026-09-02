CREATE TABLE IF NOT EXISTS workspace_usage_caps (
    workspace_id text PRIMARY KEY REFERENCES workspaces(id),
    limit_cents integer NOT NULL DEFAULT 500 CHECK (limit_cents >= 0),
    reserved_cents integer NOT NULL DEFAULT 0 CHECK (reserved_cents >= 0),
    spent_cents integer NOT NULL DEFAULT 0 CHECK (spent_cents >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (reserved_cents + spent_cents <= limit_cents)
);

INSERT INTO workspace_usage_caps(workspace_id,limit_cents)
SELECT id,500 FROM workspaces
ON CONFLICT (workspace_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS usage_reservations (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    subject_kind text NOT NULL,
    subject_id text NOT NULL,
    amount_cents integer NOT NULL CHECK (amount_cents > 0),
    phase text NOT NULL CHECK (phase IN ('reserved','committed','released')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id,subject_kind,subject_id)
);

CREATE INDEX IF NOT EXISTS usage_reservations_workspace_idx
    ON usage_reservations(workspace_id,phase);
