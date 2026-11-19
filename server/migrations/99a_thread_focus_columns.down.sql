-- Rows of mode `move` or `refused` written while the up file was applied stay in
-- place; the restored CHECK is NOT VALID, so it judges new rows only.
ALTER TABLE ask_guard_event DROP CONSTRAINT IF EXISTS ask_guard_event_mode_check;
ALTER TABLE ask_guard_event ADD CONSTRAINT ask_guard_event_mode_check
    CHECK (mode IN ('warn', 'block')) NOT VALID;

ALTER TABLE decision_request DROP COLUMN IF EXISTS reask_of_request_id;
ALTER TABLE comment DROP COLUMN IF EXISTS ask_source_root_id;
ALTER TABLE comment DROP COLUMN IF EXISTS notify_only;
