-- Run record for agent presets: which preset a claim resolved (requested) and
-- the settings the claim handed the daemon (claimed). claimed_model already
-- exists (CHE-1862). All NULL = not recorded for the current claim generation.
ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS preset_name TEXT,
    ADD COLUMN IF NOT EXISTS preset_source TEXT,
    ADD COLUMN IF NOT EXISTS preset_skips JSONB,
    ADD COLUMN IF NOT EXISTS claimed_thinking_level TEXT,
    ADD COLUMN IF NOT EXISTS claimed_service_tier TEXT,
    ADD COLUMN IF NOT EXISTS claimed_context_args JSONB;
