CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS ci_signature_owner_signature_uidx ON ci_signature_owner (workspace_id, repo_owner, repo_name, signature_hash);
