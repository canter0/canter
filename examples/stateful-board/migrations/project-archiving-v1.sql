ALTER TABLE posts ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS posts_archived_at_idx ON posts(archived_at) WHERE archived_at IS NOT NULL;
