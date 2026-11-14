CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_admission ON agent_task_queue (issue_id) WHERE admission_state IS NOT NULL;
