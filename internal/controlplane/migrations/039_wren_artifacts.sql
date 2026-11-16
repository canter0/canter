-- Published work is independent of a computer's lifetime. Its access follows
-- the originating conversation; content revisions are immutable.
CREATE TABLE IF NOT EXISTS wren_artifacts (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
    current_revision integer NOT NULL CHECK(current_revision>0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS wren_artifact_revisions (
    artifact_id text NOT NULL REFERENCES wren_artifacts(id) ON DELETE CASCADE,
    revision integer NOT NULL CHECK(revision>0),
    account_id text REFERENCES accounts(id) ON DELETE SET NULL,
    name text NOT NULL,
    media_type text NOT NULL,
    size_bytes integer NOT NULL CHECK(size_bytes>=0 AND size_bytes<=8388608),
    sha256 text NOT NULL CHECK(length(sha256)=64),
    provenance jsonb NOT NULL DEFAULT '{}',
    content bytea NOT NULL CHECK(octet_length(content)<=8388608),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(artifact_id,revision),
    CHECK(size_bytes=octet_length(content))
);
CREATE INDEX IF NOT EXISTS wren_artifacts_conversation_idx ON wren_artifacts(workspace_id,conversation_id,updated_at DESC,id);
