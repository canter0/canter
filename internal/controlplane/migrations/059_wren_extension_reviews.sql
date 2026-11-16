CREATE TABLE IF NOT EXISTS wren_extension_reviews (
 id text PRIMARY KEY,
 installation_id text NOT NULL REFERENCES wren_extension_installations(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 run_id text NOT NULL REFERENCES operator_runs(id) ON DELETE CASCADE,
 version integer NOT NULL,
 installation_revision integer NOT NULL,
 credential_revision integer NOT NULL DEFAULT 0,
 tool_name text NOT NULL,
 arguments jsonb NOT NULL,
 arguments_sha256 text NOT NULL,
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','declined','accepted')),
 call_id text REFERENCES wren_extension_calls(id) ON DELETE SET NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL DEFAULT now()+interval '24 hours',
 UNIQUE(installation_id,account_id,run_id,version,tool_name,arguments_sha256)
);
ALTER TABLE wren_extension_reviews ADD COLUMN IF NOT EXISTS account_label text NOT NULL DEFAULT '';
