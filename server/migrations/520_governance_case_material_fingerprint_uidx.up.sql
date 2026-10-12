-- Duplicate events, sweeps, and requests resolve the existing generation for
-- the same material evidence rather than allocating another one.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_governance_case_material_fingerprint
    ON governance_case (workspace_id, subject_type, subject_id, subject_revision, rule_id, material_fingerprint);
