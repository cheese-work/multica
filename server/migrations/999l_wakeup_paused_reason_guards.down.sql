UPDATE issue_wakeup SET paused_reason=NULL, enabled=true WHERE paused_reason IN ('run_budget','weekly_run_guard');
ALTER TABLE issue_wakeup DROP CONSTRAINT IF EXISTS issue_wakeup_paused_reason_check;
ALTER TABLE issue_wakeup ADD CONSTRAINT issue_wakeup_paused_reason_check
    CHECK (paused_reason IN ('max_fires','loop','rate'));
