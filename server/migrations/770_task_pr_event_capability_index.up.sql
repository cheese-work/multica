CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS task_pr_event_capability_task_uidx
    ON task_pr_event_capability (task_id);
