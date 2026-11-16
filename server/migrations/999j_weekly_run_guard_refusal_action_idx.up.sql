CREATE INDEX CONCURRENTLY IF NOT EXISTS weekly_run_guard_refusal_action_idx
    ON weekly_run_guard_refusal (action_id)
    WHERE action_id IS NOT NULL;
