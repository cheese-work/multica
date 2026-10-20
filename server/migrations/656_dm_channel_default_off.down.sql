CREATE OR REPLACE FUNCTION dm_channel_enabled(p_workspace uuid, p_user uuid, p_channel text)
RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT COALESCE(
        (SELECT p.enabled FROM notification_channel_preference p
          WHERE p.workspace_id = p_workspace AND p.user_id = p_user
            AND p.source = 'dm' AND p.channel = p_channel),
        p_channel = 'in_app')
$$;
