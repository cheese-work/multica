-- A --notify-only comment clears the no-member ask guard events its task opened
-- (CHE-7438); the event keeps the comment that cleared it. No foreign keys (repo rule).
ALTER TABLE ask_guard_event ADD COLUMN IF NOT EXISTS cleared_by_comment_id uuid;
