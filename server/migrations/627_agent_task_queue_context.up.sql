-- Prompt size of a run's last model call and the model's context window, as the
-- daemon measured them (DM auto-compact, CHE-1803). NULL means unknown: the
-- daemon predates the report, the provider does not expose it, or the run did
-- not complete. Nullable with no default, so the ALTER is metadata-only.
ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS context_tokens BIGINT,
    ADD COLUMN IF NOT EXISTS context_window BIGINT;
