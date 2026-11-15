ALTER TABLE wren_message_connections ADD COLUMN IF NOT EXISTS receiving_address text NOT NULL DEFAULT '';
ALTER TABLE wren_message_connections ADD COLUMN IF NOT EXISTS responsibility_id text REFERENCES wren_responsibilities(id) ON DELETE SET NULL;
CREATE TABLE IF NOT EXISTS wren_message_inbox (
 id text PRIMARY KEY,
 connection_id text NOT NULL REFERENCES wren_message_connections(id) ON DELETE CASCADE,
 workspace_id text NOT NULL REFERENCES workspaces(id),
 conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
 provider_id text NOT NULL,
 source_event_id text NOT NULL,
 receiving_address text NOT NULL DEFAULT '',
 responsibility_id text REFERENCES wren_responsibilities(id) ON DELETE SET NULL,
 from_address text NOT NULL,
 to_addresses jsonb NOT NULL,
 subject text NOT NULL,
 message_id text NOT NULL DEFAULT '',
 in_reply_to text NOT NULL DEFAULT '',
 references_header text NOT NULL DEFAULT '',
 thread_key text NOT NULL DEFAULT '',
 text_content text NOT NULL DEFAULT '' CHECK(octet_length(text_content)<=131072),
 text_available boolean NOT NULL DEFAULT false,
 html_present boolean NOT NULL DEFAULT false,
 body_format text NOT NULL DEFAULT '',
 attachments jsonb NOT NULL DEFAULT '[]',
 state text NOT NULL CHECK(state IN ('pending','fetching','ready','failed','blocked')),
 failure text NOT NULL DEFAULT '',
 wake_id text REFERENCES wren_responsibility_wakes(id) ON DELETE SET NULL,
 received_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(connection_id,provider_id)
);
CREATE INDEX IF NOT EXISTS wren_message_inbox_origin_idx ON wren_message_inbox(workspace_id,conversation_id,received_at DESC);
CREATE TABLE IF NOT EXISTS wren_message_webhook_events (
 id text PRIMARY KEY,
 connection_id text NOT NULL REFERENCES wren_message_connections(id) ON DELETE CASCADE,
 provider_event_id text NOT NULL,
 body_sha256 text NOT NULL,
 kind text NOT NULL,
 provider_id text NOT NULL,
 metadata jsonb NOT NULL,
 inbox_id text REFERENCES wren_message_inbox(id) ON DELETE CASCADE,
 responsibility_id text REFERENCES wren_responsibilities(id) ON DELETE SET NULL,
 state text NOT NULL CHECK(state IN ('pending','fetching','applied','failed','blocked','unmatched')),
 attempts integer NOT NULL DEFAULT 0,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 lease_token text,
 lease_expires_at timestamptz,
 failure text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(connection_id,provider_event_id)
);
CREATE INDEX IF NOT EXISTS wren_message_webhook_pending_idx ON wren_message_webhook_events(state,next_attempt_at);

ALTER TABLE wren_message_inbox ADD COLUMN IF NOT EXISTS body_format text NOT NULL DEFAULT '';

ALTER TABLE wren_message_inbox ADD COLUMN IF NOT EXISTS responsibility_id text REFERENCES wren_responsibilities(id) ON DELETE SET NULL;

ALTER TABLE wren_message_inbox ADD COLUMN IF NOT EXISTS receiving_address text NOT NULL DEFAULT '';
