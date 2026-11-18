DELETE FROM daemon_pause WHERE reason = 'scheduled_update';
ALTER TABLE daemon_pause DROP CONSTRAINT IF EXISTS daemon_pause_reason_check;
ALTER TABLE daemon_pause ADD CONSTRAINT daemon_pause_reason_check
    CHECK (reason IN ('maintenance', 'incident', 'suspended'));
ALTER TABLE daemon_pause DROP COLUMN IF EXISTS expires_at;
ALTER TABLE daemon_pause DROP COLUMN IF EXISTS cycle_id;
DROP TABLE IF EXISTS daemon_update_cycle;
