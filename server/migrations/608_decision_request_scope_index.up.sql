CREATE INDEX CONCURRENTLY IF NOT EXISTS decision_request_scope_idx ON decision_request (workspace_id, issue_id, created_at, id);
