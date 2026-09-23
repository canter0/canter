-- Immutable, conversation-private evidence lives outside model checkpoints.
CREATE TABLE IF NOT EXISTS operator_web_sources (
    id text PRIMARY KEY,
    conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
    url text NOT NULL,
    title text NOT NULL,
    published_at text NOT NULL DEFAULT '',
    retrieved_at timestamptz NOT NULL DEFAULT now(),
    kind text NOT NULL CHECK (kind IN ('preview','document')),
    content text NOT NULL CHECK (octet_length(content) <= 131072),
    truncated boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS operator_web_sources_conversation_idx ON operator_web_sources(conversation_id,retrieved_at DESC,id);
CREATE INDEX IF NOT EXISTS operator_web_sources_url_idx ON operator_web_sources(conversation_id,url,retrieved_at DESC);

CREATE TABLE IF NOT EXISTS operator_web_cache (
    conversation_id text NOT NULL REFERENCES operator_conversations(id) ON DELETE CASCADE,
    cache_key text NOT NULL,
    result jsonb NOT NULL CHECK (octet_length(result::text) <= 16384),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(conversation_id,cache_key)
);
