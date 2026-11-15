CREATE TABLE IF NOT EXISTS sandboxes (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 name text NOT NULL,
 size text NOT NULL,
 tier text NOT NULL CHECK(tier IN ('free','paid')),
 phase text NOT NULL DEFAULT 'queued',
 plan jsonb,
 ssh_key_version text NOT NULL DEFAULT '',
 ssh_key_ciphertext bytea,
 address inet,
 host_fingerprint text,
 monthly_cents bigint NOT NULL DEFAULT 0,
 failure text,
 delete_requested boolean NOT NULL DEFAULT false,
 sleep_requested boolean NOT NULL DEFAULT false,
 created_by_kind text NOT NULL DEFAULT 'human',
 created_by_id text,
 created_by_name text,
 last_used_at timestamptz,
 awake_since timestamptz,
 checkpoint_object_ref text,
 checkpoint_key_version text NOT NULL DEFAULT '',
 checkpoint_nonce bytea,
 checkpoint_manifest bytea,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 next_attempt_at timestamptz
);
CREATE INDEX IF NOT EXISTS sandboxes_workspace_phase_idx ON sandboxes(workspace_id,phase);
CREATE INDEX IF NOT EXISTS sandboxes_due_idx ON sandboxes(next_attempt_at) WHERE phase NOT IN ('deleted');
CREATE TABLE IF NOT EXISTS sandbox_free_usage (
 workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 month date NOT NULL,
 awake_seconds bigint NOT NULL DEFAULT 0,
 PRIMARY KEY(workspace_id,month)
);
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS ssh_public_key text NOT NULL DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS generation integer NOT NULL DEFAULT 1;
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS sleep_forced_at timestamptz;
