-- name: InsertProvenanceExportLog :one
-- CHE-755: audit row for one provenance export. manifest carries ids,
-- revisions and digests only, never exported content.
INSERT INTO provenance_export_log (
    workspace_id, actor_type, actor_id, request_digest, manifest_digest,
    cutoff, source_count, included_count, excluded_count, manifest
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
RETURNING *;

-- name: DeleteExpiredProvenanceExportLogsForWorkspace :execrows
-- CHE-766: scheduled retention cleanup. Deletes audit rows older than the
-- workspace's own configured retention window rather than a single global
-- cutoff, so each workspace's admin-set retention_days is honored exactly.
-- Returns the row count so the scheduler's audit trail records how many
-- manifests were reaped per tick.
DELETE FROM provenance_export_log
WHERE workspace_id = $1
  AND created_at < now() - make_interval(days => sqlc.arg('retention_days')::int);

-- name: ListWorkspaceExportRetentionSettings :many
-- CHE-766: retention-days per workspace, for the scheduler to fan out
-- DeleteExpiredProvenanceExportLogsForWorkspace one call per workspace.
SELECT id AS workspace_id, export_manifest_retention_days
FROM workspace
ORDER BY id ASC;
