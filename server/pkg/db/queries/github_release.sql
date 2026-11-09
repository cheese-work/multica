-- =====================
-- Release issues from a verified C00 deploy (CHE-4937)
-- Repo owner and name are stored lowercased; callers pass them lowercased.
-- =====================

-- name: ClaimGitHubReleaseDeploy :one
-- Returns a row only for the caller that owns this deploy: a fresh insert, or a
-- takeover of a claim that never got an issue (a crash) after two minutes.
-- No row means another delivery already owns it.
INSERT INTO github_release_deploy (workspace_id, repo_owner, repo_name, source_sha, image_digest, old_sha, run_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (workspace_id, repo_owner, repo_name, source_sha, image_digest) DO UPDATE
    SET claimed_at = now(), run_id = EXCLUDED.run_id
    WHERE github_release_deploy.issue_id IS NULL
      AND github_release_deploy.claimed_at < now() - interval '2 minutes'
RETURNING *;

-- name: GetGitHubReleaseDeploy :one
SELECT * FROM github_release_deploy
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND source_sha = $4 AND image_digest = $5;

-- name: SetGitHubReleaseDeployIssue :exec
UPDATE github_release_deploy SET issue_id = $6
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND source_sha = $4 AND image_digest = $5;

-- name: SetGitHubReleaseDeployLabeled :exec
UPDATE github_release_deploy SET labeled_at = now()
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND source_sha = $4 AND image_digest = $5;

-- name: EnsureIssueLabelByName :one
-- Find-or-create under the (workspace, type, lower(name)) unique index.
INSERT INTO issue_label (workspace_id, resource_type, name, description, color)
VALUES ($1, 'issue', $2, $3, $4)
ON CONFLICT (workspace_id, resource_type, LOWER(name)) DO UPDATE SET name = issue_label.name
RETURNING *;

-- name: ListReleaseMergedPullRequests :many
-- Merged PRs since the previous release of this repo (7 days when none).
SELECT pr_number, title, html_url FROM github_pull_request pr
WHERE pr.workspace_id = $1 AND pr.repo_owner = $2 AND pr.repo_name = $3
  AND pr.state = 'merged'
  AND pr.merged_at > COALESCE((
      SELECT max(d.claimed_at) FROM github_release_deploy d
      WHERE d.workspace_id = $1 AND d.repo_owner = $2 AND d.repo_name = $3
        AND d.source_sha <> $4 AND d.issue_id IS NOT NULL
  ), now() - interval '7 days')
ORDER BY pr.merged_at ASC
LIMIT 100;

-- name: DeleteUnissuedGitHubReleaseDeploy :exec
-- Frees a claim whose issue create failed. Never touches a claim that has an issue.
DELETE FROM github_release_deploy
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND source_sha = $4 AND image_digest = $5
  AND issue_id IS NULL;
