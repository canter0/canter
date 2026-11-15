ALTER TABLE operator_runs ADD COLUMN IF NOT EXISTS executor_kind text NOT NULL DEFAULT 'operator';
CREATE INDEX IF NOT EXISTS operator_executor_work_idx ON operator_runs(executor_kind,created_at) WHERE status IN ('queued','running');
-- Keep executor cleanup records if a conversation/job is deleted. A remote
-- process must not lose its stop/revocation handle merely because its UI did.
CREATE TABLE IF NOT EXISTS wren_harness_executions (
 job_id text PRIMARY KEY,
 run_id text NOT NULL UNIQUE,
 computer_id text NOT NULL REFERENCES wren_computers(id),
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','running','completed','failed','stop_requested','stopped')),
 process_id text NOT NULL DEFAULT '',
 snapshot jsonb NOT NULL DEFAULT '{}',
 grant_id text,
 failure text NOT NULL DEFAULT '',
 stop_requested boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 next_attempt_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS wren_harness_pending_idx ON wren_harness_executions(next_attempt_at) WHERE status IN ('pending','running','stop_requested');
CREATE TABLE IF NOT EXISTS wren_model_grants (
 id text PRIMARY KEY,
 job_id text NOT NULL,
 computer_id text NOT NULL REFERENCES wren_computers(id),
 workspace_id text NOT NULL,
 requester_account_id text REFERENCES accounts(id) ON DELETE SET NULL,
 token_hash bytea NOT NULL UNIQUE,
 token_key_version text NOT NULL,
 token_ciphertext bytea NOT NULL,
 model text NOT NULL,
 max_requests integer NOT NULL CHECK(max_requests BETWEEN 1 AND 200),
 used_requests integer NOT NULL DEFAULT 0 CHECK(used_requests>=0),
 max_output_tokens integer NOT NULL CHECK(max_output_tokens BETWEEN 1 AND 16384),
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS wren_model_grants_job_idx ON wren_model_grants(job_id);
CREATE TABLE IF NOT EXISTS wren_model_requests (
 id bigserial PRIMARY KEY,
 grant_id text NOT NULL REFERENCES wren_model_grants(id),
 workspace_id text NOT NULL,
 requester_account_id text NOT NULL,
 model text NOT NULL,
 status text NOT NULL DEFAULT 'started' CHECK(status IN ('started','completed','failed','uncertain')),
 http_status integer,
 usage jsonb NOT NULL DEFAULT '{}',
 upstream_id text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 completed_at timestamptz
);
CREATE INDEX IF NOT EXISTS wren_model_requests_account_idx ON wren_model_requests(requester_account_id,created_at DESC);
ALTER TABLE wren_harness_executions ADD COLUMN IF NOT EXISTS response_text text NOT NULL DEFAULT '';
ALTER TABLE wren_harness_executions ADD COLUMN IF NOT EXISTS prompt_request_id bigint NOT NULL DEFAULT 0;
ALTER TABLE wren_harness_executions ADD COLUMN IF NOT EXISTS process_stopped boolean NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS wren_harness_events (
 job_id text NOT NULL REFERENCES wren_harness_executions(job_id),
 cursor bigint NOT NULL,
 data jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(job_id,cursor)
);
CREATE UNIQUE INDEX IF NOT EXISTS wren_model_grants_active_job_idx ON wren_model_grants(job_id) WHERE revoked_at IS NULL;
ALTER TABLE wren_harness_executions ADD COLUMN IF NOT EXISTS terminal_status text NOT NULL DEFAULT '' CHECK(terminal_status IN ('','completed','failed','cancelled'));
