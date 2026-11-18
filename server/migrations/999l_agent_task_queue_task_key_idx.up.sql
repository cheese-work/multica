-- CHE-6888: ListGitHubReviewAuthorCandidates matches a task by the last 12 hex
-- digits of its id (the task key at the end of an agent branch name). Without an
-- index on that expression the match scans all of agent_task_queue.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_task_key
    ON agent_task_queue ((right(replace(id::text, '-', ''), 12)));
