CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS comment_trigger_rate_limit_issue_agent_uidx
    ON comment_trigger_rate_limit (issue_id, agent_id);
