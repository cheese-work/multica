DROP TRIGGER chat_session_channel_origin_immutable ON chat_session;
DROP FUNCTION preserve_chat_session_channel_origin();
ALTER TABLE chat_session DROP COLUMN channel_origin_at;
