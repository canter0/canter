ALTER TABLE ci_runner_admissions ADD COLUMN decision jsonb
    CHECK (decision IS NULL OR jsonb_typeof(decision)='object');
