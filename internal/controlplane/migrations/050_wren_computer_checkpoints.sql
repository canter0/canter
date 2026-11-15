CREATE TABLE IF NOT EXISTS wren_computer_checkpoints (
 id text PRIMARY KEY,
 workspace_id text NOT NULL,
 conversation_id text NOT NULL,
 owner_id text NOT NULL,
 scope_kind text NOT NULL CHECK(scope_kind IN ('private','room')),
 computer_id text NOT NULL,
 state text NOT NULL DEFAULT 'creating' CHECK(state IN ('creating','ready','restoring','deleting','failed')),
 include_browser boolean NOT NULL DEFAULT false,
 delete_after boolean NOT NULL DEFAULT false,
 key_version text NOT NULL DEFAULT '',
 encryption_nonce bytea NOT NULL DEFAULT ''::bytea,
 object jsonb NOT NULL DEFAULT '{}',
 manifest jsonb NOT NULL DEFAULT '{}',
 failure text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 next_attempt_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS wren_checkpoints_scope_idx ON wren_computer_checkpoints(workspace_id,conversation_id,created_at DESC);
CREATE INDEX IF NOT EXISTS wren_checkpoints_work_idx ON wren_computer_checkpoints(next_attempt_at) WHERE state IN ('creating','deleting','restoring');
ALTER TABLE wren_computers ADD COLUMN IF NOT EXISTS restore_checkpoint_id text;
ALTER TABLE wren_computer_checkpoints ADD COLUMN IF NOT EXISTS encryption_nonce bytea NOT NULL DEFAULT ''::bytea;
