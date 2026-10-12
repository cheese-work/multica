CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_governance_attempt_case_ordinal
    ON governance_attempt (case_id, ordinal);
