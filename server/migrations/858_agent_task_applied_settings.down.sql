ALTER TABLE agent_task_queue
    DROP COLUMN IF EXISTS applied_model,
    DROP COLUMN IF EXISTS applied_thinking_level,
    DROP COLUMN IF EXISTS applied_service_tier,
    DROP COLUMN IF EXISTS applied_context_args,
    DROP COLUMN IF EXISTS applied_reported_at;
