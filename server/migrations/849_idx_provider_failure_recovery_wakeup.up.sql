CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS provider_failure_recovery_wakeup_idx ON provider_failure_recovery (recovery_wakeup_id) WHERE recovery_wakeup_id IS NOT NULL;
