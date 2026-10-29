CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_wakeup_fired_task ON chat_wakeup (fired_task_id) WHERE fired_task_id IS NOT NULL;
