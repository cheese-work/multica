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
