DROP FUNCTION IF EXISTS audit_workspace_delete(UUID);
DROP FUNCTION IF EXISTS audit_retention_purge(UUID, TIMESTAMPTZ);
DROP TABLE IF EXISTS workspace_audit_event;
DROP FUNCTION IF EXISTS workspace_audit_event_guard();
