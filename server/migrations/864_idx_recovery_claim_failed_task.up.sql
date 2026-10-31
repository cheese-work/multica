CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS recovery_claim_failed_task_idx
    ON recovery_claim (failed_task_id);
