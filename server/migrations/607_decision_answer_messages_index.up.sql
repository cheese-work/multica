CREATE INDEX CONCURRENTLY IF NOT EXISTS decision_answer_messages_idx ON decision_answer USING gin (chat_message_ids);
