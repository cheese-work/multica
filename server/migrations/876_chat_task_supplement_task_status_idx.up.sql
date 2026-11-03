CREATE INDEX CONCURRENTLY IF NOT EXISTS chat_task_supplement_task_status_idx
    ON chat_task_supplement (task_id, status);
