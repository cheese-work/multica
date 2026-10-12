-- Source deletion/redaction finds copied context by workspace and source key.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_governance_evaluation_source_workspace_object
    ON governance_evaluation_source (workspace_id, object_type, object_id);
