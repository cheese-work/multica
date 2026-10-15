DROP FUNCTION IF EXISTS inbox_item_visible(text, uuid, uuid, uuid);
DROP FUNCTION IF EXISTS dm_channel_enabled(uuid, uuid, text);
ALTER TABLE inbox_item
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS dedupe_key,
    DROP COLUMN IF EXISTS chat_message_id,
    DROP COLUMN IF EXISTS chat_session_id;
