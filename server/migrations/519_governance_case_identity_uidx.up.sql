-- One lifecycle generation per subject/rule identity.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_governance_case_identity_generation
    ON governance_case (workspace_id, subject_type, subject_id, subject_revision, rule_id, generation);
