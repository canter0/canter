CREATE TABLE IF NOT EXISTS wren_artifact_pins (
 artifact_id text NOT NULL REFERENCES wren_artifacts(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(artifact_id,account_id)
);
CREATE INDEX IF NOT EXISTS wren_artifact_pins_account_idx ON wren_artifact_pins(account_id,created_at DESC);
