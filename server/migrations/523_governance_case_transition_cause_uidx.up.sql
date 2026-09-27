CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_governance_case_transition_cause
    ON governance_case_transition (case_id, cause_event_key);
