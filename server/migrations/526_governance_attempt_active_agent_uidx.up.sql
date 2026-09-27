-- A case cannot carry two simultaneously admitted agent attempts.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_governance_attempt_active_agent
    ON governance_attempt (case_id)
    WHERE kind = 'agent' AND terminal_at IS NULL;
