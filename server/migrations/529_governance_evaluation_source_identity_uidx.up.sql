-- One source-index entry per captured object in an immutable evaluation.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_governance_evaluation_source_identity
    ON governance_evaluation_source (evaluation_id, object_type, object_id, object_revision);
