-- =====================
-- Release issues from a verified C00 deploy (CHE-4937)
-- Repo owner and name are stored lowercased; callers pass them lowercased.
-- =====================

-- name: LinkGitHubReleaseDeploy :exec
-- Records which issue a deploy opened. Idempotent: a redelivery, a recovery
-- after a crash and a concurrent caller all write the same row once.
INSERT INTO github_release_deploy (workspace_id, repo_owner, repo_name, source_sha, image_digest, old_sha, run_id, issue_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (workspace_id, repo_owner, repo_name, source_sha, image_digest) DO NOTHING;

-- name: GetGitHubReleaseDeploy :one
SELECT * FROM github_release_deploy
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
