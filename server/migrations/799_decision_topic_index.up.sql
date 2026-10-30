CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_session_decision_topic
    ON chat_session (decision_topic_id) WHERE decision_topic_id IS NOT NULL;
