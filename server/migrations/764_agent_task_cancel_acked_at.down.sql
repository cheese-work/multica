ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS stop_unconfirmed_at;
ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS cancel_acked_at;
