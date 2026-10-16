-- DM auto-compact (CHE-1803, layer A3). task_kind is NULL for every ordinary
-- task and 'compact' for a compaction run, manual or automatic. compact_reason
-- and compact_trigger_tokens are set only on an automatic compaction: the
-- server-built reason shown in both DMs, and the prompt size that triggered it
-- (the 1.1x re-arm rule after a failed attempt reads it). All three are
-- nullable with no default, so the ALTER is metadata-only.
ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS task_kind TEXT,
    ADD COLUMN IF NOT EXISTS compact_reason TEXT,
    ADD COLUMN IF NOT EXISTS compact_trigger_tokens BIGINT;
