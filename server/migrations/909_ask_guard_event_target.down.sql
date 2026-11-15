ALTER TABLE ask_guard_event
    DROP COLUMN IF EXISTS intent_id,
    DROP COLUMN IF EXISTS member_id,
    DROP COLUMN IF EXISTS cleared_at,
    DROP COLUMN IF EXISTS cleared_by_request_id;
