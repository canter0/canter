-- Correspondence identities and reviewed drafts are separate from account mail.
CREATE TABLE IF NOT EXISTS wren_message_connections (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES workspaces(id),
 conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
 owner_id text REFERENCES accounts(id) ON DELETE SET NULL,
 scope jsonb NOT NULL,
 name text NOT NULL,
 provider text NOT NULL CHECK(provider='resend'),
 from_address text NOT NULL,
 enabled boolean NOT NULL DEFAULT true,
 revision integer NOT NULL CHECK(revision>0),
 key_id text NOT NULL,
 ciphertext bytea,
 revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS wren_message_connections_origin_idx ON wren_message_connections(workspace_id,conversation_id);
ALTER TABLE wren_message_connections ADD COLUMN IF NOT EXISTS webhook_key_id text NOT NULL DEFAULT '';
ALTER TABLE wren_message_connections ADD COLUMN IF NOT EXISTS webhook_ciphertext bytea;
CREATE TABLE IF NOT EXISTS wren_message_drafts (
 id text PRIMARY KEY,
 connection_id text NOT NULL REFERENCES wren_message_connections(id),
 workspace_id text NOT NULL REFERENCES workspaces(id),
 conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
 creator_id text REFERENCES accounts(id) ON DELETE SET NULL,
 run_id text REFERENCES operator_runs(id) ON DELETE SET NULL,
 instruction_key text NOT NULL,
 revision integer NOT NULL CHECK(revision>0),
 content jsonb NOT NULL,
 state text NOT NULL CHECK(state IN ('draft','rejected','dispatching','accepted','delivered','bounced','complained','failed','unknown')),
 provider_id text NOT NULL DEFAULT '',
 failure text NOT NULL DEFAULT '',
 send_actor_id text REFERENCES accounts(id) ON DELETE SET NULL,
 dispatch_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(connection_id,creator_id,instruction_key)
);
CREATE UNIQUE INDEX IF NOT EXISTS wren_message_one_proposal_per_run_idx ON wren_message_drafts(connection_id,run_id) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS wren_message_drafts_origin_idx ON wren_message_drafts(workspace_id,conversation_id,created_at DESC);
CREATE TABLE IF NOT EXISTS wren_message_draft_revisions (
 draft_id text NOT NULL REFERENCES wren_message_drafts(id) ON DELETE CASCADE,
 revision integer NOT NULL,
 content jsonb NOT NULL,
 author_id text REFERENCES accounts(id) ON DELETE SET NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(draft_id,revision)
);
CREATE TABLE IF NOT EXISTS wren_message_receipts (
 id text PRIMARY KEY,
 draft_id text NOT NULL REFERENCES wren_message_drafts(id) ON DELETE CASCADE,
 kind text NOT NULL,
 provider_id text NOT NULL DEFAULT '',
 detail text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE wren_message_drafts ADD COLUMN IF NOT EXISTS authority_key text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS wren_message_rejections (
 connection_id text NOT NULL REFERENCES wren_message_connections(id) ON DELETE CASCADE,
 creator_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 authority_key text NOT NULL,
 draft_id text NOT NULL REFERENCES wren_message_drafts(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(connection_id,creator_id,authority_key)
);
