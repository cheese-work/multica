CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS browser_profile_agent_uidx
    ON browser_profile_agent (profile_id, agent_id);
