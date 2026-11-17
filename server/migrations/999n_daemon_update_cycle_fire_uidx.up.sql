-- One cycle per machine and cron fire time (CHE-5466).
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS daemon_update_cycle_fire_uidx
    ON daemon_update_cycle (machine_owner_id, daemon_id, fire_at);
