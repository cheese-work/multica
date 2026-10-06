CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_project_id_keyset ON issue(project_id, id);
