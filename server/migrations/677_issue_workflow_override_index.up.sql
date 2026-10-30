-- Direct-reference lookups for workflow deletion and edit (676).
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_workflow_override
    ON issue (workflow_id) WHERE workflow_id IS NOT NULL;
