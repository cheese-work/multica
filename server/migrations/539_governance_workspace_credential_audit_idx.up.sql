CREATE INDEX CONCURRENTLY governance_workspace_credential_audit_workspace_purpose_idx
    ON governance_workspace_credential_audit (workspace_id, purpose, created_at);
