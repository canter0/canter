-- Keep stable timestamp/id history page lookups indexed per conversation.
CREATE INDEX IF NOT EXISTS operator_messages_conversation_created_idx
    ON operator_messages(conversation_id, created_at DESC, id DESC);
