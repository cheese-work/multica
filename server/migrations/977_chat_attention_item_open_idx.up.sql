CREATE INDEX CONCURRENTLY IF NOT EXISTS chat_attention_item_open_idx
    ON chat_attention_item (user_id, workspace_id)
    WHERE resolved_at IS NULL;
