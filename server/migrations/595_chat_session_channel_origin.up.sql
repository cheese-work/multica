ALTER TABLE chat_session ADD COLUMN channel_origin_at timestamptz;

WITH origins AS (
    SELECT chat_session_id, MIN(created_at) AS origin_at
    FROM (
        SELECT chat_session_id, created_at FROM channel_chat_session_binding
        UNION ALL
        SELECT chat_session_id, created_at FROM chat_message WHERE channel_ingested
    ) AS evidence
    GROUP BY chat_session_id
)
UPDATE chat_session AS session
SET channel_origin_at = origins.origin_at
FROM origins
WHERE session.id = origins.chat_session_id;

CREATE FUNCTION preserve_chat_session_channel_origin() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.channel_origin_at IS NOT NULL
       AND NEW.channel_origin_at IS DISTINCT FROM OLD.channel_origin_at THEN
        RAISE EXCEPTION 'chat session channel origin is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER chat_session_channel_origin_immutable
BEFORE UPDATE OF channel_origin_at ON chat_session
FOR EACH ROW EXECUTE FUNCTION preserve_chat_session_channel_origin();
