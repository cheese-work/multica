CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_run_input_owner_owned ON run_input (owner_task_id) WHERE state = 'owned';
