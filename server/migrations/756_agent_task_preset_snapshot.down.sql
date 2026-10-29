ALTER TABLE agent_task_queue
    DROP COLUMN IF EXISTS preset_name,
    DROP COLUMN IF EXISTS preset_source,
    DROP COLUMN IF EXISTS preset_skips,
    DROP COLUMN IF EXISTS claimed_thinking_level,
    DROP COLUMN IF EXISTS claimed_service_tier,
    DROP COLUMN IF EXISTS claimed_context_args;
