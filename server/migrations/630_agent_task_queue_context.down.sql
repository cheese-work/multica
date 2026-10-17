ALTER TABLE agent_task_queue
    DROP COLUMN IF EXISTS context_tokens,
    DROP COLUMN IF EXISTS context_window;
