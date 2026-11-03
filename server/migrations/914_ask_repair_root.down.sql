ALTER TABLE ask_guard_event
    DROP COLUMN IF EXISTS root_id,
    DROP COLUMN IF EXISTS repair_handled_at;
DROP TABLE IF EXISTS ask_repair_root;
