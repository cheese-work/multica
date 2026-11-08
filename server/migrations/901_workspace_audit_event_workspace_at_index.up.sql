CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_workspace_audit_event_workspace_at
    ON workspace_audit_event (workspace_id, at DESC, id DESC);
