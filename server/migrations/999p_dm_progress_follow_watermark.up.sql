-- DM progress cards (CHE-3549, T3d): a follow's event watermark. The follow's
-- snapshot (last_rendered_status) is read under the issue lock at the moment the
-- watermark is taken, so pending rows created at or before the watermark are
-- already in the snapshot and the dispatcher drops them; later rows are the
-- outbox. A re-follow of an unfollowed issue takes a new watermark.
ALTER TABLE dm_progress_follow ADD COLUMN event_watermark TIMESTAMPTZ;
UPDATE dm_progress_follow SET event_watermark = created_at;
ALTER TABLE dm_progress_follow ALTER COLUMN event_watermark SET DEFAULT clock_timestamp();
ALTER TABLE dm_progress_follow ALTER COLUMN event_watermark SET NOT NULL;
