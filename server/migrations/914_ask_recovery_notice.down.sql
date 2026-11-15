ALTER TABLE ask_guard_event
    DROP COLUMN IF EXISTS source_text,
    DROP COLUMN IF EXISTS delivered_at,
    DROP COLUMN IF EXISTS repair_reason;
DROP TABLE IF EXISTS ask_recovery_notice;
