CREATE TABLE IF NOT EXISTS wren_run_outcomes (
 run_id text PRIMARY KEY REFERENCES operator_runs(id) ON DELETE CASCADE,
 outcome text NOT NULL CHECK(outcome IN ('quiet')),
 reason text NOT NULL CHECK(length(reason)>0 AND length(reason)<=1000),
 created_at timestamptz NOT NULL DEFAULT now()
);
