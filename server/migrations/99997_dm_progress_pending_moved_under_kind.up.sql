-- CHE-3405 / CHE-3548 AC3: a reparent reaches the origin DM as a pending row of
-- kind moved_under, so the dispatcher batches, denies and flushes it like every
-- other change. The outbox is small (unsent rows only), so the constraint swap
-- is cheap.
ALTER TABLE dm_progress_pending DROP CONSTRAINT IF EXISTS dm_progress_pending_event_kind_check;
ALTER TABLE dm_progress_pending ADD CONSTRAINT dm_progress_pending_event_kind_check
    CHECK (event_kind IN ('status', 'assignee', 'pr', 'moved_under'));
