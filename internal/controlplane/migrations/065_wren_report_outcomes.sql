ALTER TABLE wren_run_outcomes DROP CONSTRAINT IF EXISTS wren_run_outcomes_outcome_check;
ALTER TABLE wren_run_outcomes ADD CONSTRAINT wren_run_outcomes_outcome_check CHECK(outcome IN ('quiet','blocked','report'));
ALTER TABLE wren_run_outcomes DROP CONSTRAINT IF EXISTS wren_run_outcomes_reason_check;
ALTER TABLE wren_run_outcomes ADD CONSTRAINT wren_run_outcomes_reason_check CHECK(length(reason)>0 AND length(reason)<=CASE WHEN outcome='report' THEN 12000 ELSE 1000 END);
