ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS chat_compaction_epoch;
ALTER TABLE chat_session DROP COLUMN IF EXISTS compaction_epoch;
