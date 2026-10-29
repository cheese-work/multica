ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS replacement_released_at;
ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS pull_request_id;
