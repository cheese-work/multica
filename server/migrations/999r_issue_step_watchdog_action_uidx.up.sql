-- At most one action of a kind per status occurrence (CHE-6116 S2).
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS issue_step_watchdog_action_occurrence_uidx
    ON issue_step_watchdog_action (issue_id, occurrence_key, action);
