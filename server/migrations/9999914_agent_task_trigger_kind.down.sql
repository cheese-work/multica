SET LOCAL lock_timeout = '2s';

ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS trigger_kind;
