DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version='065_wren_report_outcomes') THEN
  ALTER TABLE wren_run_outcomes DROP CONSTRAINT IF EXISTS wren_run_outcomes_outcome_check;
  ALTER TABLE wren_run_outcomes ADD CONSTRAINT wren_run_outcomes_outcome_check CHECK(outcome IN ('quiet','blocked'));
 END IF;
END $$;
