-- Preserve existing titles; only new conversations request automatic titles.
ALTER TABLE operator_conversations ADD COLUMN IF NOT EXISTS title_state text NOT NULL DEFAULT 'manual'
    CHECK (title_state IN ('snippet', 'requested', 'generated', 'manual'));
ALTER TABLE operator_conversations ALTER COLUMN title_state SET DEFAULT 'snippet';
