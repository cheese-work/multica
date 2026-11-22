CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS browser_profile_member_uidx
    ON browser_profile_member (profile_id, user_id);
