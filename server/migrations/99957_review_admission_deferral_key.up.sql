CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS review_admission_deferral_issue_agent_idx
    ON review_admission_deferral (issue_id, agent_id);
