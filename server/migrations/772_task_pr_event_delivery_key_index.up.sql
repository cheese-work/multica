CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS task_pr_event_delivery_key_uidx
    ON task_pr_event_delivery (task_id, event_id);
