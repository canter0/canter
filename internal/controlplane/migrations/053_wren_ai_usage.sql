-- AI allowance is independent of workspace_usage_caps (compute cents).
-- Micro-USD avoids losing sub-cent speech/model costs. No automatic period reset.
CREATE TABLE IF NOT EXISTS wren_ai_allowances (
 workspace_id text PRIMARY KEY REFERENCES workspaces(id),
 limit_micro_usd bigint NOT NULL CHECK(limit_micro_usd >= 0),
 held_micro_usd bigint NOT NULL DEFAULT 0 CHECK(held_micro_usd >= 0),
 spent_micro_usd bigint NOT NULL DEFAULT 0 CHECK(spent_micro_usd >= 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS wren_ai_reservations (
 id text PRIMARY KEY,
 workspace_id text NOT NULL REFERENCES wren_ai_allowances(workspace_id),
 request_key text NOT NULL,
 account_id text NOT NULL,
 conversation_id text NOT NULL,
 run_id text NOT NULL DEFAULT '',
 kind text NOT NULL CHECK(kind IN ('operator','acp','dictation','media')),
 model text NOT NULL,
 state text NOT NULL CHECK(state IN ('reserved','dispatched','uncertain','settled','released')),
 amount_micro_usd bigint NOT NULL CHECK(amount_micro_usd>0),
 actual_micro_usd bigint CHECK(actual_micro_usd>=0),
 provider_id text NOT NULL DEFAULT '',
 usage jsonb NOT NULL DEFAULT '{}'::jsonb,
 dispatched_at timestamptz,
 settled_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(workspace_id,request_key)
);
CREATE INDEX IF NOT EXISTS wren_ai_reservations_active_idx ON wren_ai_reservations(workspace_id,state);

ALTER TABLE wren_ai_reservations ADD COLUMN IF NOT EXISTS estimate jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE wren_ai_allowances ADD COLUMN IF NOT EXISTS closed_at timestamptz;
ALTER TABLE wren_ai_reservations ADD COLUMN IF NOT EXISTS purpose text NOT NULL DEFAULT '';
ALTER TABLE wren_ai_reservations ADD COLUMN IF NOT EXISTS run_lease text NOT NULL DEFAULT '';
