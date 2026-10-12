-- A CAS result and its idempotent cause key are each recorded once.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_governance_case_transition_revision
    ON governance_case_transition (case_id, resulting_state_revision);
