-- At most one open incident per cause key.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS incident_cause_open_uidx
    ON incident_cause (workspace_id, issue_id, kind) WHERE closed_at IS NULL;
