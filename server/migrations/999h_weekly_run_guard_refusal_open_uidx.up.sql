-- At most one open refusal, so one card, per issue (CHE-6172).
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS weekly_run_guard_refusal_open_uidx
    ON weekly_run_guard_refusal (issue_id)
    WHERE state = 'open';
