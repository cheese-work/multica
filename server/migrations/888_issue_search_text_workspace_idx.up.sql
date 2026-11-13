CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_search_text_workspace
    ON issue_search_text (workspace_id);
