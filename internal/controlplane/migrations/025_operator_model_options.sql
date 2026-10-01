ALTER TABLE operator_runs ADD COLUMN IF NOT EXISTS model_options jsonb NOT NULL DEFAULT '{}'::jsonb;
