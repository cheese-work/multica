-- At most one open cycle per machine (CHE-5466, D13).
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS daemon_update_cycle_open_uidx
    ON daemon_update_cycle (machine_owner_id, daemon_id)
    WHERE finished_at IS NULL;
