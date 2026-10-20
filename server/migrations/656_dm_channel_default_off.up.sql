-- CHE-1848: DM switches default to off, matching the notification-channels API
-- (server/internal/handler/notification_channel.go). With no switch row a DM
-- reply creates no record, and inbox_item_visible hides existing dm_reply rows.
CREATE OR REPLACE FUNCTION dm_channel_enabled(p_workspace uuid, p_user uuid, p_channel text)
RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT COALESCE(
        (SELECT p.enabled FROM notification_channel_preference p
          WHERE p.workspace_id = p_workspace AND p.user_id = p_user
            AND p.source = 'dm' AND p.channel = p_channel),
        false)
$$;
