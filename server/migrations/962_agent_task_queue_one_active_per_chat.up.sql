CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_one_active_per_chat
  ON agent_task_queue (chat_session_id)
  WHERE chat_session_id IS NOT NULL
    AND status IN ('dispatched', 'waiting_local_directory', 'running');
