CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS agent_task_queue_handoff_dispatch_uidx ON agent_task_queue (handoff_dispatch_id) WHERE handoff_dispatch_id IS NOT NULL;
