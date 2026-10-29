-- name: UpsertGitHubRouteGrant :one
INSERT INTO github_route_grant (project_id, user_id, granted_by)
VALUES ($1, $2, sqlc.narg('granted_by'))
ON CONFLICT (project_id, user_id) DO UPDATE SET project_id = EXCLUDED.project_id
RETURNING *;

-- name: DeleteGitHubRouteGrant :execrows
DELETE FROM github_route_grant WHERE project_id = $1 AND user_id = $2;

-- name: DeleteGitHubRouteGrantsByProject :exec
DELETE FROM github_route_grant WHERE project_id = $1;

-- name: ListGitHubRouteGrants :many
SELECT * FROM github_route_grant WHERE project_id = $1 ORDER BY created_at ASC;

-- name: HasGitHubRouteGrant :one
SELECT EXISTS (SELECT 1 FROM github_route_grant WHERE project_id = $1 AND user_id = $2);

-- name: ListGitHubPullRequestIssues :many
-- Binding-chain tail (plan 5.8 steps 3-5): every issue linked to the PR row,
-- with the issue's CURRENT project_id. Same-workspace join so a stale link
-- row can never cross workspaces.
SELECT i.* FROM issue_pull_request l
JOIN issue i ON i.id = l.issue_id AND i.workspace_id = $1
WHERE l.pull_request_id = $2
ORDER BY i.created_at ASC, i.id ASC;
