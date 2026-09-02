ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS family_id text;
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS parent_id text REFERENCES agent_credentials(id);

UPDATE agent_credentials SET family_id=id WHERE family_id IS NULL;
ALTER TABLE agent_credentials ALTER COLUMN family_id SET NOT NULL;

ALTER TABLE agent_sessions ADD COLUMN IF NOT EXISTS credential_id text REFERENCES agent_credentials(id);

CREATE INDEX IF NOT EXISTS agent_credentials_family_idx ON agent_credentials(family_id);
CREATE INDEX IF NOT EXISTS agent_sessions_credential_idx ON agent_sessions(credential_id) WHERE ended_at IS NULL;
