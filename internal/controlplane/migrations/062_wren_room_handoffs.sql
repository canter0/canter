ALTER TABLE operator_runs DROP CONSTRAINT IF EXISTS operator_runs_origin_kind_check;
ALTER TABLE operator_runs ADD CONSTRAINT operator_runs_origin_kind_check CHECK(origin_kind IN ('human','responsibility','job','handoff'));

CREATE TABLE IF NOT EXISTS wren_room_handoffs (
 target_run_id text PRIMARY KEY REFERENCES operator_runs(id) ON DELETE CASCADE,
 parent_run_id text NOT NULL REFERENCES operator_runs(id) ON DELETE CASCADE,
 from_wren jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS wren_room_handoffs_parent_idx ON wren_room_handoffs(parent_run_id);
