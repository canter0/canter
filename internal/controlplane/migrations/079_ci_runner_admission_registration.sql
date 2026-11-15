CREATE TABLE ci_runner_admissions (
 workspace_id text NOT NULL,
 runner_id text PRIMARY KEY,
 admission_id text NOT NULL CHECK (admission_id ~ '^[0-9a-f]{64}$'),
 github_repository_id bigint NOT NULL CHECK (github_repository_id > 0),
 runner_name text NOT NULL CHECK (runner_name ~ '^[A-Za-z0-9_-]{1,64}$'),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (workspace_id,admission_id),
 FOREIGN KEY (workspace_id,runner_id) REFERENCES ci_runners(workspace_id,id) ON DELETE CASCADE
);
