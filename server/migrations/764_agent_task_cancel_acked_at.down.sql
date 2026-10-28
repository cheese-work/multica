DROP TRIGGER IF EXISTS agent_task_keep_stop_unconfirmed ON agent_task_queue;
DROP FUNCTION IF EXISTS agent_task_keep_stop_unconfirmed();
ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS stop_unconfirmed_at;
ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS cancel_acked_at;
