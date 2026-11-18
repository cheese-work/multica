DELETE FROM dm_progress_pending WHERE event_kind = 'moved_under';
ALTER TABLE dm_progress_pending DROP CONSTRAINT IF EXISTS dm_progress_pending_event_kind_check;
ALTER TABLE dm_progress_pending ADD CONSTRAINT dm_progress_pending_event_kind_check
    CHECK (event_kind IN ('status', 'assignee', 'pr'));
