ALTER TABLE agent_task_queue
    DROP COLUMN IF EXISTS task_kind,
    DROP COLUMN IF EXISTS compact_reason,
    DROP COLUMN IF EXISTS compact_trigger_tokens;
