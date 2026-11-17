-- CHE-6193: a wakeup a run guard refuses is paused with the guard's name.
ALTER TABLE issue_wakeup DROP CONSTRAINT IF EXISTS issue_wakeup_paused_reason_check;
ALTER TABLE issue_wakeup ADD CONSTRAINT issue_wakeup_paused_reason_check
    CHECK (paused_reason IN ('max_fires','loop','rate','run_budget','weekly_run_guard'));
