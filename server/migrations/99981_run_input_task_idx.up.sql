CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_run_input_task ON run_input (task_id) WHERE task_id IS NOT NULL;
