CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS notification_channel_preference_key_uidx ON notification_channel_preference (workspace_id, user_id, source, channel);
