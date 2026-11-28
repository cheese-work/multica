-- A refused comment edit keeps the id of the comment it edits (CHE-10594), so ask
-- recovery can tell an event of an ask that already reached the member from a
-- distinct later ask. No foreign keys (repo rule).
ALTER TABLE ask_guard_event ADD COLUMN IF NOT EXISTS comment_id uuid;
