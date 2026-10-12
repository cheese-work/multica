ALTER TABLE chat_session
    DROP COLUMN IF EXISTS continued_from_session_id,
    DROP COLUMN IF EXISTS continued_into_session_id;
