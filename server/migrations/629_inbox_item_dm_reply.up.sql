-- CHE-1793: DM reply notification records. One unread dm_reply row per
-- (recipient, DM) is coalesced through dedupe_key (index in 630); a decision
-- ping carries chat_session_id so it opens the DM. updated_at is nullable and
-- read as COALESCE(updated_at, created_at): adding NOT NULL DEFAULT now() would
-- re-stamp every existing row with the migration time and reorder the inbox.
-- No foreign keys; dependents are cleaned in application code.
ALTER TABLE inbox_item
    ADD COLUMN IF NOT EXISTS chat_session_id uuid,
    ADD COLUMN IF NOT EXISTS chat_message_id uuid,
    ADD COLUMN IF NOT EXISTS dedupe_key text,
    ADD COLUMN IF NOT EXISTS updated_at timestamptz;

-- The dm in-app switch, with the same defaults as the notification-channels API
-- (server/internal/handler/notification_channel.go): in_app on, push off.
CREATE OR REPLACE FUNCTION dm_channel_enabled(p_workspace uuid, p_user uuid, p_channel text)
RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT COALESCE(
        (SELECT p.enabled FROM notification_channel_preference p
          WHERE p.workspace_id = p_workspace AND p.user_id = p_user
            AND p.source = 'dm' AND p.channel = p_channel),
        p_channel = 'in_app')
$$;

-- Whether the inbox may show a row to its recipient. Rows with no DM always
-- show. A DM row shows only while the recipient could still read that DM: the
-- same checks as the DM read gate (member of the workspace, creator of a
-- non-command session, owner/admin/allow-listed for the agent). A dm_reply row
-- also hides while the user's dm in_app switch is off; push-only records stay
-- for read sync.
CREATE OR REPLACE FUNCTION inbox_item_visible(p_type text, p_chat_session uuid, p_workspace uuid, p_user uuid)
RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT p_chat_session IS NULL OR (
        EXISTS (
            SELECT 1
              FROM chat_session cs
              JOIN agent a ON a.id = cs.agent_id
              JOIN member m ON m.workspace_id = cs.workspace_id AND m.user_id = p_user
             WHERE cs.id = p_chat_session
               AND cs.workspace_id = p_workspace
               AND cs.creator_id = p_user
               AND (cs.explicitly_created_at IS NOT NULL
                    OR EXISTS (SELECT 1 FROM chat_message pm
                                WHERE pm.chat_session_id = cs.id AND pm.message_kind != 'channel_command'))
               AND (a.owner_id = p_user
                    OR m.role IN ('owner', 'admin')
                    OR (a.permission_mode = 'public_to'
                        AND EXISTS (SELECT 1 FROM agent_invocation_target t
                                     WHERE t.agent_id = a.id
                                       AND (t.target_type = 'workspace'
                                            OR (t.target_type = 'member' AND t.target_id = p_user)))))
        )
        AND (p_type <> 'dm_reply' OR dm_channel_enabled(p_workspace, p_user, 'in_app'))
    )
$$;
