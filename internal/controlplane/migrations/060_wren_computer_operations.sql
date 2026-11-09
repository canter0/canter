CREATE TABLE IF NOT EXISTS wren_computer_operations (
 computer_id text PRIMARY KEY REFERENCES wren_computers(id) ON DELETE CASCADE,
 token text NOT NULL,
 expires_at timestamptz NOT NULL
);
