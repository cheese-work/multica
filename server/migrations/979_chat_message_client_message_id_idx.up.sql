CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS chat_message_client_message_id_idx
    ON chat_message (chat_session_id, client_message_id)
    WHERE client_message_id IS NOT NULL;
