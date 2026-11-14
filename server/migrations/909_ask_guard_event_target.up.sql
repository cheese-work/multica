-- Ask guard events for a decision intent target (CHE-3345 S3) and the matching
-- outcome that clears an event. intent_id and member_id name the ask identity
-- (set for an intent target; member_id may also name the one member of another
-- event). A request created with --recovers clears one event. Its unique index
-- is the next migration. No foreign keys (repo rule).
ALTER TABLE ask_guard_event
    ADD COLUMN IF NOT EXISTS intent_id uuid,
    ADD COLUMN IF NOT EXISTS member_id uuid,
    ADD COLUMN IF NOT EXISTS cleared_at timestamptz,
    ADD COLUMN IF NOT EXISTS cleared_by_request_id uuid;
