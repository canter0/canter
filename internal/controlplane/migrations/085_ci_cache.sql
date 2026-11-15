ALTER TABLE ci_settings ADD COLUMN cache_epoch bigint NOT NULL DEFAULT 1
    CHECK (cache_epoch > 0);
CREATE INDEX ci_jobs_cache_active_run
    ON ci_jobs(github_repository_id,github_run_id,github_job_id) WHERE status='running';

-- Operator authorization for a dedicated, private cache directory. A user
-- enabling cache does not authorize storage or create provider resources.
CREATE TABLE ci_cache_pool (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    backend_digest text NOT NULL DEFAULT ''
        CHECK (backend_digest = '' OR backend_digest ~ '^[0-9a-f]{64}$'),
    enabled boolean NOT NULL DEFAULT false,
    max_total_bytes bigint NOT NULL DEFAULT 0 CHECK (max_total_bytes >= 0),
    max_workspace_bytes bigint NOT NULL DEFAULT 0
        CHECK (max_workspace_bytes BETWEEN 0 AND max_total_bytes),
    max_entry_bytes bigint NOT NULL DEFAULT 0
        CHECK (max_entry_bytes BETWEEN 0 AND 536870912
            AND max_entry_bytes <= max_workspace_bytes),
    max_objects integer NOT NULL DEFAULT 0 CHECK (max_objects BETWEEN 0 AND 10000),
    max_workspace_objects integer NOT NULL DEFAULT 0
        CHECK (max_workspace_objects BETWEEN 0 AND max_objects),
    ttl_seconds integer NOT NULL DEFAULT 0 CHECK (ttl_seconds BETWEEN 0 AND 604800),
    reserved_bytes bigint NOT NULL DEFAULT 0
        CHECK (reserved_bytes BETWEEN 0 AND max_total_bytes),
    reserved_objects integer NOT NULL DEFAULT 0
        CHECK (reserved_objects BETWEEN 0 AND max_objects),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (NOT enabled OR
        (backend_digest <> '' AND max_entry_bytes > 0 AND max_workspace_objects > 0 AND ttl_seconds > 0))
);
INSERT INTO ci_cache_pool(singleton) VALUES (true);

-- This ledger intentionally outlives workspace deletion. Counters are released
-- only after the private backend proves both pending and final files absent.
CREATE TABLE ci_cache_workspace_usage (
    workspace_id text PRIMARY KEY,
    reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    reserved_objects integer NOT NULL DEFAULT 0 CHECK (reserved_objects >= 0)
);

CREATE TABLE ci_cache_namespaces (
    digest text PRIMARY KEY CHECK (digest ~ '^[0-9a-f]{64}$'),
    workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    installation_id text NOT NULL,
    github_repository_id bigint NOT NULL CHECK (github_repository_id > 0),
    epoch bigint NOT NULL CHECK (epoch > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id,installation_id)
        REFERENCES ci_github_installations(workspace_id,id) ON DELETE CASCADE,
    FOREIGN KEY (installation_id,github_repository_id)
        REFERENCES ci_repositories(installation_id,github_repository_id) ON DELETE CASCADE
);

CREATE TABLE ci_cache_objects (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{64}$'),
    workspace_id text NOT NULL,
    namespace_digest text NOT NULL,
    key_digest text NOT NULL,
    version_digest text NOT NULL,
    backend_digest text NOT NULL CHECK (backend_digest ~ '^[0-9a-f]{64}$'),
    runner_id text NOT NULL,
    source_job_id text NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 536870912),
    sha256 text NOT NULL,
    state text NOT NULL DEFAULT 'reserved'
        CHECK (state IN ('reserved','writing','published','cleanup','deleted')),
    writer_token text NOT NULL DEFAULT ''
        CHECK (writer_token = '' OR writer_token ~ '^[0-9a-f]{64}$'),
    write_attempted_at timestamptz,
    upload_deadline_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    physical_absent_at timestamptz,
    quota_released_at timestamptz,
    cleanup_token text NOT NULL DEFAULT ''
        CHECK (cleanup_token = '' OR cleanup_token ~ '^[0-9a-f]{64}$'),
    cleanup_lease_until timestamptz,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (upload_deadline_at > created_at),
    CHECK (expires_at > created_at),
    CHECK ((cleanup_token = '') = (cleanup_lease_until IS NULL)),
    CHECK ((quota_released_at IS NULL) = (state <> 'deleted')),
    CHECK (state <> 'deleted' OR physical_absent_at IS NOT NULL),
    CHECK ((state = 'deleted' AND namespace_digest = '' AND sha256 = ''
                AND key_digest = '' AND version_digest = ''
                AND runner_id = '' AND source_job_id = '')
        OR (state <> 'deleted' AND namespace_digest ~ '^[0-9a-f]{64}$'
                AND key_digest ~ '^[0-9a-f]{64}$' AND version_digest ~ '^[0-9a-f]{64}$'
                AND sha256 ~ '^[0-9a-f]{64}$'
                AND runner_id <> '' AND source_job_id <> ''))
);
CREATE INDEX ci_cache_objects_cleanup ON ci_cache_objects(next_attempt_at,id)
    WHERE state <> 'deleted';
CREATE INDEX ci_cache_objects_workspace ON ci_cache_objects(workspace_id,state);
CREATE UNIQUE INDEX ci_cache_objects_pending_key
    ON ci_cache_objects(namespace_digest,key_digest,version_digest)
    WHERE state IN ('reserved','writing','published');

CREATE TABLE ci_cache_entries (
    namespace_digest text NOT NULL REFERENCES ci_cache_namespaces(digest) ON DELETE CASCADE,
    key_digest text NOT NULL CHECK (key_digest ~ '^[0-9a-f]{64}$'),
    version_digest text NOT NULL CHECK (version_digest ~ '^[0-9a-f]{64}$'),
    object_id text NOT NULL UNIQUE REFERENCES ci_cache_objects(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace_digest,key_digest,version_digest)
);
