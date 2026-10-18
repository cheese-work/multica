-- Cheese, 2026-10-18: DM replies stay out of the inbox unless the user opts
-- in. A missing dm switch now means off for both channels; the inbox switches
-- keep their own defaults (server/internal/handler/notification_channel.go).
-- inbox_item_visible already hides dm_reply rows while dm in_app is off, so
-- existing records leave the inbox with no data change.
CREATE OR REPLACE FUNCTION dm_channel_enabled(p_workspace uuid, p_user uuid, p_channel text)
RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT COALESCE(
        (SELECT p.enabled FROM notification_channel_preference p
          WHERE p.workspace_id = p_workspace AND p.user_id = p_user
            AND p.source = 'dm' AND p.channel = p_channel),
        false)
$$;
