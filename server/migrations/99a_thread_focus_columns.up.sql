-- CHE-6426 (S1 of CHE-6385): the facts the thread-focus rules read. All
-- additive and nullable: a row written before this migration has no answer and
-- stays NULL. No foreign keys (repo rule); the references are checked in app code.
--   comment.notify_only        true for an FYI comment, false for an ask, NULL for old rows
--   comment.ask_source_root_id the thread root an ask thread was opened from
--   decision_request.reask_of_request_id  the request a re-ask continues
ALTER TABLE comment ADD COLUMN IF NOT EXISTS notify_only BOOLEAN;
ALTER TABLE comment ADD COLUMN IF NOT EXISTS ask_source_root_id UUID;
ALTER TABLE decision_request ADD COLUMN IF NOT EXISTS reask_of_request_id UUID;

-- ask_guard_event.mode also holds `move` (a long-thread ask moved to its own
-- thread) and `refused` (a re-ask gate refusal). The widened CHECK is NOT VALID:
-- it checks new rows and takes no long scan under the lock. The recovery and
-- repair workers read only mode = 'block', so neither new mode reaches them.
ALTER TABLE ask_guard_event DROP CONSTRAINT IF EXISTS ask_guard_event_mode_check;
ALTER TABLE ask_guard_event ADD CONSTRAINT ask_guard_event_mode_check
    CHECK (mode IN ('warn', 'block', 'move', 'refused')) NOT VALID;
