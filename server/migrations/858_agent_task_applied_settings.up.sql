-- Run record for agent presets, third tier: the launch selection the daemon
-- reported after its own fallbacks and drops, once per claim generation. It is
-- not the provider-observed model or effort. All NULL = unknown (older daemon,
-- or the report failed). Reset with the other snapshot columns whenever
-- dispatched_at is written.
ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS applied_model TEXT,
    ADD COLUMN IF NOT EXISTS applied_thinking_level TEXT,
    ADD COLUMN IF NOT EXISTS applied_service_tier TEXT,
    ADD COLUMN IF NOT EXISTS applied_context_args JSONB,
    ADD COLUMN IF NOT EXISTS applied_reported_at TIMESTAMPTZ;
