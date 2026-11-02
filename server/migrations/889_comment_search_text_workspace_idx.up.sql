CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_comment_search_text_workspace
    ON comment_search_text (workspace_id);
