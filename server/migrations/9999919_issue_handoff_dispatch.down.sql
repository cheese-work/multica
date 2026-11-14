SET LOCAL lock_timeout = '2s';

ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS handoff_dispatch_id;
DROP TABLE IF EXISTS issue_handoff_dispatch;
