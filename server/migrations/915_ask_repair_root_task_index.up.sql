CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_ask_repair_root_task
    ON ask_repair_root (repair_task_id) WHERE repair_task_id IS NOT NULL;
