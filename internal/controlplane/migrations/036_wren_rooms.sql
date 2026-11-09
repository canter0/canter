CREATE TABLE IF NOT EXISTS wren_rooms (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    account_id text NOT NULL REFERENCES accounts(id),
    conversation_id text NOT NULL UNIQUE REFERENCES operator_conversations(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK(kind IN ('channel','group')),
    name text NOT NULL,
    purpose text NOT NULL DEFAULT '',
    section text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS wren_room_members (
    room_id text NOT NULL REFERENCES wren_rooms(id) ON DELETE CASCADE,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    PRIMARY KEY(room_id,account_id)
);
CREATE TABLE IF NOT EXISTS wren_room_wrens (
    room_id text NOT NULL REFERENCES wren_rooms(id) ON DELETE CASCADE,
    wren_id text NOT NULL REFERENCES wrens(id) ON DELETE CASCADE,
    position integer NOT NULL,
    PRIMARY KEY(room_id,wren_id)
);
ALTER TABLE operator_runs ADD COLUMN IF NOT EXISTS speaker jsonb;
ALTER TABLE operator_runs ADD COLUMN IF NOT EXISTS sender_name text NOT NULL DEFAULT '';
ALTER TABLE operator_runs ADD COLUMN IF NOT EXISTS requester_account_id text REFERENCES accounts(id) ON DELETE SET NULL;
ALTER TABLE operator_runs ADD COLUMN IF NOT EXISTS turn_id text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS wren_rooms_workspace_idx ON wren_rooms(workspace_id);

ALTER TABLE wren_room_members DROP CONSTRAINT IF EXISTS wren_room_members_account_id_fkey;
ALTER TABLE wren_room_members ADD CONSTRAINT wren_room_members_account_id_fkey FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE;
ALTER TABLE operator_runs DROP CONSTRAINT IF EXISTS operator_runs_requester_account_id_fkey;
ALTER TABLE operator_runs ADD CONSTRAINT operator_runs_requester_account_id_fkey FOREIGN KEY(requester_account_id) REFERENCES accounts(id) ON DELETE SET NULL;
