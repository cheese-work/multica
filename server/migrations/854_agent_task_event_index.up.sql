CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_event_ws_runtime_at ON agent_task_event (workspace_id, runtime_id, at);
