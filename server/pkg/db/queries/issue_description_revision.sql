-- name: ListIssueDescriptionRevisions :many
-- Workspace guard lives in the join: a foreign issue id returns no rows.
SELECT r.revision, r.actor_type, r.actor_id, r.created_at
FROM issue_description_revision r
JOIN issue i ON i.id = r.issue_id
WHERE r.issue_id = $1 AND i.workspace_id = $2
ORDER BY r.revision;

-- name: GetIssueDescriptionAtRevision :one
-- Text as of revision N: the latest snapshot at or before N.
SELECT r.revision, r.description
FROM issue_description_revision r
JOIN issue i ON i.id = r.issue_id
WHERE r.issue_id = $1 AND i.workspace_id = $2 AND r.revision <= $3
ORDER BY r.revision DESC
LIMIT 1;
