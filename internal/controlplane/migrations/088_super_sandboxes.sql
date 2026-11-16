-- Execution allocations, not VM billing limits. No worker lifecycle is owned here.
CREATE TABLE super_sandboxes (
 id text PRIMARY KEY CHECK (id ~ '^ssb_[a-zA-Z0-9_-]+$'),
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 -- Removing a creator from a shared organization withdraws authority without
 -- erasing the outstanding jobs that the reconciler still needs to cancel.
 creator_account_id text REFERENCES accounts(id) ON DELETE SET NULL,
 name text NOT NULL,
 state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','running','cancelling','succeeded','failed','cancelled','needs_attention')),
 max_concurrency integer NOT NULL CHECK (max_concurrency BETWEEN 1 AND 16),
 max_worker_seconds integer NOT NULL CHECK (max_worker_seconds BETWEEN 1 AND 86400),
 reserved_worker_seconds integer NOT NULL CHECK (reserved_worker_seconds BETWEEN 1 AND max_worker_seconds),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 last_reconciled_at timestamptz NOT NULL DEFAULT '-infinity',
 started_at timestamptz,
 finished_at timestamptz,
 failure text NOT NULL DEFAULT '',
 UNIQUE(workspace_id,key_hash)
);
CREATE INDEX super_sandboxes_pending ON super_sandboxes(last_reconciled_at,created_at,id) WHERE finished_at IS NULL;
CREATE TABLE super_sandbox_workers (
 run_id text NOT NULL REFERENCES super_sandboxes(id) ON DELETE CASCADE,
 -- Keep the immutable ID/generation and task evidence if a sandbox row is
 -- physically cleaned up. A missing live link is not a replacement worker.
 worker_id text NOT NULL,
 sandbox_id text REFERENCES sandboxes(id) ON DELETE SET NULL,
 CHECK (sandbox_id IS NULL OR sandbox_id=worker_id),
 generation integer NOT NULL,
 worker_index integer NOT NULL,
 PRIMARY KEY(run_id,worker_id),
 UNIQUE(run_id,worker_index)
);
CREATE TABLE super_sandbox_tasks (
 id text PRIMARY KEY,
 run_id text NOT NULL REFERENCES super_sandboxes(id) ON DELETE CASCADE,
 task_index integer NOT NULL CHECK (task_index BETWEEN 0 AND 255),
 name text NOT NULL,
 command text NOT NULL,
 directory text NOT NULL,
 timeout_seconds integer NOT NULL CHECK (timeout_seconds BETWEEN 1 AND 3600),
 state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','starting','running','unknown','succeeded','failed','cancelled')),
 worker_id text,
 generation integer,
 job_id text UNIQUE,
 job_request jsonb,
 started_at timestamptz,
 finished_at timestamptz,
 exit_code integer,
 error text NOT NULL DEFAULT '',
 UNIQUE(run_id,task_index),
 FOREIGN KEY(run_id,worker_id) REFERENCES super_sandbox_workers(run_id,worker_id) ON DELETE CASCADE,
 CHECK ((worker_id IS NULL AND generation IS NULL AND job_id IS NULL AND job_request IS NULL)
     OR (worker_id IS NOT NULL AND generation IS NOT NULL AND job_id IS NOT NULL AND job_request IS NOT NULL))
);
-- Includes uncertain starts: an unknown receipt never frees a worker.
CREATE UNIQUE INDEX super_sandbox_worker_exclusive ON super_sandbox_tasks(worker_id)
 WHERE state IN ('starting','running','unknown');
CREATE INDEX super_sandbox_tasks_run ON super_sandbox_tasks(run_id,task_index);
