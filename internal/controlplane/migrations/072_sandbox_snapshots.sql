-- Tenant deletion must erase storage before removing these handles.
-- In particular, workspace_id deliberately has no cascading foreign key.
CREATE TABLE IF NOT EXISTS sandbox_snapshots (
 id text PRIMARY KEY,
 workspace_id text NOT NULL,
 source_sandbox_id text NOT NULL,
 source_generation integer NOT NULL CHECK(source_generation>0),
 source_size text NOT NULL,
 source_disk_gib integer NOT NULL CHECK(source_disk_gib>0),
 image_id text NOT NULL DEFAULT '',
 name text NOT NULL,
 state text NOT NULL CHECK(state IN ('creating','ready','failed','deleting','deleted')),
 retained boolean NOT NULL DEFAULT false,
 options jsonb NOT NULL,
 stats jsonb NOT NULL DEFAULT '{}',
 key_version text NOT NULL DEFAULT '',
 key_ciphertext bytea NOT NULL DEFAULT '',
 manifest_ref jsonb NOT NULL DEFAULT '{}',
 stored_bytes bigint NOT NULL DEFAULT 0 CHECK(stored_bytes>=0),
 failure text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 ready_at timestamptz,
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(workspace_id,id)
);
CREATE INDEX IF NOT EXISTS sandbox_snapshots_workspace_idx ON sandbox_snapshots(workspace_id,state,created_at);
CREATE TABLE IF NOT EXISTS sandbox_snapshot_parts (
 snapshot_id text NOT NULL REFERENCES sandbox_snapshots(id) ON DELETE CASCADE,
 ordinal integer NOT NULL CHECK(ordinal BETWEEN -1 AND 69999),
 object_ref jsonb NOT NULL,
 plain_bytes bigint NOT NULL CHECK(plain_bytes>0),
 plain_sha256 text NOT NULL CHECK(length(plain_sha256)=64),
 state text NOT NULL DEFAULT 'planned' CHECK(state IN ('planned','stored','deleted')),
 put_confirmed_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(snapshot_id,ordinal)
);
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS current_snapshot_id text;
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS pending_snapshot_id text;
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='sandbox_current_snapshot_scope' AND conrelid='sandboxes'::regclass) THEN
  ALTER TABLE sandboxes ADD CONSTRAINT sandbox_current_snapshot_scope
   FOREIGN KEY(workspace_id,current_snapshot_id) REFERENCES sandbox_snapshots(workspace_id,id);
 END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='sandbox_pending_snapshot_scope' AND conrelid='sandboxes'::regclass) THEN
  ALTER TABLE sandboxes ADD CONSTRAINT sandbox_pending_snapshot_scope
   FOREIGN KEY(workspace_id,pending_snapshot_id) REFERENCES sandbox_snapshots(workspace_id,id);
 END IF;
END $$;

CREATE TABLE IF NOT EXISTS sandbox_templates (
 id text PRIMARY KEY,
 workspace_id text NOT NULL,
 name text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 deleted_at timestamptz,
 UNIQUE(workspace_id,id)
);
CREATE UNIQUE INDEX IF NOT EXISTS sandbox_templates_name_idx ON sandbox_templates(workspace_id,lower(name)) WHERE deleted_at IS NULL;
CREATE TABLE IF NOT EXISTS sandbox_template_versions (
 workspace_id text NOT NULL,
 template_id text NOT NULL,
 version integer NOT NULL CHECK(version>0),
 snapshot_id text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 deleted_at timestamptz,
 PRIMARY KEY(template_id,version),
 FOREIGN KEY(workspace_id,template_id) REFERENCES sandbox_templates(workspace_id,id),
 FOREIGN KEY(workspace_id,snapshot_id) REFERENCES sandbox_snapshots(workspace_id,id)
);
CREATE INDEX IF NOT EXISTS sandbox_template_snapshot_idx ON sandbox_template_versions(snapshot_id) WHERE deleted_at IS NULL;

-- Minimal erasure handles survive tenant/snapshot removal. An uncertain PUT
-- can complete after its caller stops waiting, so its tombstone is rechecked.
-- These rows contain no data keys, plaintext, filenames or account identities.
CREATE TABLE IF NOT EXISTS sandbox_storage_erasures (
 scope text NOT NULL,
 object_key text NOT NULL,
 object_ref jsonb NOT NULL,
 uncertain boolean NOT NULL DEFAULT false,
 erased_at timestamptz,
 retry_at timestamptz NOT NULL DEFAULT now(),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(scope,object_key)
);
CREATE INDEX IF NOT EXISTS sandbox_storage_erasures_due_idx ON sandbox_storage_erasures(retry_at);
