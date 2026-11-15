CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS chat_attention_item_one_open_later_idx
    ON chat_attention_item (chain_root_session_id, user_id)
    WHERE kind = 'later' AND resolved_at IS NULL;
