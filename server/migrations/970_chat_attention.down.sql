SET LOCAL lock_timeout = '2s';

DROP TABLE IF EXISTS chat_attention_mark;
DROP TABLE IF EXISTS chat_attention_item;
DROP TRIGGER IF EXISTS trg_chat_session_default_chain_root ON chat_session;
DROP FUNCTION IF EXISTS chat_session_default_chain_root();
ALTER TABLE chat_session DROP COLUMN IF EXISTS chain_root_session_id;
