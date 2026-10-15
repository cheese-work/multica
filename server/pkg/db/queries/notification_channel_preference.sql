-- name: ListNotificationChannelPreferences :many
SELECT * FROM notification_channel_preference
WHERE workspace_id = $1 AND user_id = $2;

-- name: SetNotificationChannelPreference :exec
INSERT INTO notification_channel_preference (workspace_id, user_id, source, channel, enabled)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (workspace_id, user_id, source, channel)
DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now();
