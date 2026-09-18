ALTER TABLE operator_messages ADD COLUMN IF NOT EXISTS attachments jsonb NOT NULL DEFAULT '[]';
