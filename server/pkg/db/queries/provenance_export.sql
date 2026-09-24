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

-- name: DeleteExpiredProvenanceExportLogsForWorkspace :many
-- CHE-766: scheduled retention cleanup for one workspace. The cutoff is
-- computed from a live subquery on workspace.export_manifest_retention_days
-- — NOT a value the caller fetched earlier and passes in — so a concurrent
-- admin edit to retention_days (e.g. an emergency extension from 1 to 365
-- days to protect an active obligation) is honored by this exact statement,
-- not by whatever the retention setting happened to be when the scheduler's
-- workspace list was built. Returns the deleted ids so the caller can write
-- a workspace-scoped, content-free receipt of exactly what was removed
-- (never titles/descriptions/comment bodies — provenance_export_log already
-- carries only ids, digests and exclusion reasons per migration 515).
DELETE FROM provenance_export_log
WHERE workspace_id = $1
  AND created_at < now() - make_interval(days =>
        (SELECT export_manifest_retention_days FROM workspace WHERE id = $1)::int)
RETURNING id;

-- name: ListWorkspaceExportRetentionSettings :many
-- CHE-766: workspace ids for the scheduler to fan out
-- DeleteExpiredProvenanceExportLogsForWorkspace one call per workspace. Only
-- ids are needed — retention_days is read live by the delete query itself,
-- not carried from here, so this list can never go stale between being read
-- and being acted on.
SELECT id AS workspace_id
FROM workspace
ORDER BY id ASC;
