CREATE TABLE ci_runner_creation_journal (
    operation_id text PRIMARY KEY REFERENCES ci_runner_lifecycles(operation_id) ON DELETE CASCADE,
    stage text NOT NULL DEFAULT 'not_started'
        CHECK (stage IN ('not_started','security_group','port','server')),
    known_resources jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(known_resources)='array'),
    creator_finished_at timestamptz,
    result_digest text CHECK (result_digest ~ '^[0-9a-f]{64}$'),
    mutation_attempted boolean,
    result_settled boolean,
    result_resources jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(result_resources)='array'),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CHECK ((creator_finished_at IS NULL) = (result_digest IS NULL)),
    CHECK ((creator_finished_at IS NULL) = (mutation_attempted IS NULL)),
    CHECK ((creator_finished_at IS NULL) = (result_settled IS NULL))
);
